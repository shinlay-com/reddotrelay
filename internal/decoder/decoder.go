package decoder

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"reddotrelay/internal/core"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

var ErrUnconfiguredEvent = errors.New("unconfigured contract event")

type ContractSpec struct {
	Address string
	ABIPath string
	ABIJSON []byte
	Events  []EventSpec
}

type EventSpec struct {
	Selector     string
	Destinations []core.WebhookDestination
}

type contract struct {
	address common.Address
	events  map[common.Hash]configuredEvent
	routes  map[string][]core.WebhookDestination
}

type configuredEvent struct {
	abi          abi.Event
	destinations []core.WebhookDestination
}

type Decoder struct {
	contracts map[common.Address]contract
	now       func() time.Time
}

// Load reads contract ABI files and indexes only the selected event names or
// canonical signatures. Relative ABI paths are resolved from baseDir.
func Load(baseDir string, specs []ContractSpec) (*Decoder, error) {
	decoder := &Decoder{contracts: make(map[common.Address]contract, len(specs)), now: time.Now}
	for i, spec := range specs {
		if !common.IsHexAddress(spec.Address) {
			return nil, fmt.Errorf("contract %d has invalid address %q", i, spec.Address)
		}
		address := common.HexToAddress(spec.Address)
		if _, exists := decoder.contracts[address]; exists {
			return nil, fmt.Errorf("duplicate contract address %s", address.Hex())
		}
		parsed, err := loadABI(baseDir, address, spec)
		if err != nil {
			return nil, err
		}
		selected, routes, err := selectEvents(parsed, spec.Events)
		if err != nil {
			return nil, fmt.Errorf("contract %s: %w", address.Hex(), err)
		}
		decoder.contracts[address] = contract{address: address, events: selected, routes: routes}
	}
	return decoder, nil
}

func loadABI(baseDir string, address common.Address, spec ContractSpec) (abi.ABI, error) {
	if len(spec.ABIJSON) > 0 {
		if spec.ABIPath != "" {
			return abi.ABI{}, fmt.Errorf("contract %s ABI path and inline JSON are mutually exclusive", address.Hex())
		}
		parsed, err := abi.JSON(bytes.NewReader(spec.ABIJSON))
		if err != nil {
			return abi.ABI{}, fmt.Errorf("parse ABI for %s: %w", address.Hex(), err)
		}
		return parsed, nil
	}
	if spec.ABIPath == "" {
		return abi.ABI{}, fmt.Errorf("contract %s ABI is required", address.Hex())
	}
	path := spec.ABIPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return abi.ABI{}, fmt.Errorf("open ABI for %s: %w", address.Hex(), err)
	}
	parsed, parseErr := abi.JSON(file)
	closeErr := file.Close()
	if parseErr != nil {
		return abi.ABI{}, fmt.Errorf("parse ABI for %s: %w", address.Hex(), parseErr)
	}
	if closeErr != nil {
		return abi.ABI{}, fmt.Errorf("close ABI for %s: %w", address.Hex(), closeErr)
	}
	return parsed, nil
}

func selectEvents(parsed abi.ABI, selectors []EventSpec) (map[common.Hash]configuredEvent, map[string][]core.WebhookDestination, error) {
	if len(selectors) == 0 {
		return nil, nil, errors.New("at least one event selector is required")
	}
	selected := make(map[common.Hash]configuredEvent)
	routes := make(map[string][]core.WebhookDestination)
	for _, spec := range selectors {
		if len(spec.Destinations) == 0 {
			return nil, nil, fmt.Errorf("event selector %q has no webhook destinations", spec.Selector)
		}
		matched := false
		for _, event := range parsed.Events {
			if spec.Selector != event.RawName && spec.Selector != event.Sig {
				continue
			}
			if event.Anonymous {
				return nil, nil, fmt.Errorf("anonymous event %s is not supported", event.Sig)
			}
			if _, exists := selected[event.ID]; exists {
				return nil, nil, fmt.Errorf("event %s is selected more than once", event.Sig)
			}
			destinations := append([]core.WebhookDestination(nil), spec.Destinations...)
			selected[event.ID] = configuredEvent{abi: event, destinations: destinations}
			routes[event.Sig] = destinations
			matched = true
		}
		if !matched {
			return nil, nil, fmt.Errorf("event selector %q was not found in ABI", spec.Selector)
		}
	}
	return selected, routes, nil
}

func (d *Decoder) Decode(_ context.Context, raw core.RawLog) (core.Event, error) {
	if !common.IsHexAddress(raw.Address) {
		return core.Event{}, fmt.Errorf("invalid log address %q", raw.Address)
	}
	configured, ok := d.contracts[common.HexToAddress(raw.Address)]
	if !ok || len(raw.Topics) == 0 {
		return core.Event{}, ErrUnconfiguredEvent
	}
	if len(strings.TrimPrefix(raw.Topics[0], "0x")) != 64 {
		return core.Event{}, errors.New("invalid event signature topic")
	}
	topic0 := common.HexToHash(raw.Topics[0])
	selected, ok := configured.events[topic0]
	if !ok {
		return core.Event{}, ErrUnconfiguredEvent
	}
	eventABI := selected.abi

	values := make(map[string]any, len(eventABI.Inputs))
	nonIndexed := eventABI.Inputs.NonIndexed()
	unpacked, err := nonIndexed.Unpack(raw.Data)
	if err != nil {
		return core.Event{}, fmt.Errorf("decode %s data: %w", eventABI.Sig, err)
	}
	nonIndexedPosition := 0
	for i, argument := range eventABI.Inputs {
		if !argument.Indexed {
			values[argumentName(argument.Name, i)] = jsonSafeValue(unpacked[nonIndexedPosition])
			nonIndexedPosition++
		}
	}

	indexed := make(abi.Arguments, 0)
	for i, argument := range eventABI.Inputs {
		if argument.Indexed {
			argument.Name = argumentName(argument.Name, i)
			indexed = append(indexed, argument)
		}
	}
	if len(raw.Topics)-1 != len(indexed) {
		return core.Event{}, fmt.Errorf("decode %s topics: got %d indexed topics, want %d", eventABI.Sig, len(raw.Topics)-1, len(indexed))
	}
	indexedTopics := make([]common.Hash, len(indexed))
	for i, topic := range raw.Topics[1:] {
		if len(strings.TrimPrefix(topic, "0x")) != 64 {
			return core.Event{}, fmt.Errorf("decode %s topic %d: invalid hash", eventABI.Sig, i+1)
		}
		indexedTopics[i] = common.HexToHash(topic)
	}
	if err := abi.ParseTopicsIntoMap(values, indexed, indexedTopics); err != nil {
		return core.Event{}, fmt.Errorf("decode %s indexed fields: %w", eventABI.Sig, err)
	}
	for name, value := range values {
		values[name] = jsonSafeValue(value)
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return core.Event{}, fmt.Errorf("encode %s payload: %w", eventABI.Sig, err)
	}
	return core.Event{
		ID:          core.EventID{ChainID: raw.ChainID, TransactionHash: raw.TransactionHash, LogIndex: raw.LogIndex},
		BlockNumber: raw.BlockNumber, BlockHash: raw.BlockHash, Address: configured.address.Hex(),
		Name: eventABI.RawName, Signature: eventABI.Sig,
		RawTopics: append([]string(nil), raw.Topics...), RawData: append([]byte(nil), raw.Data...),
		DecodedPayload: payload, ObservedAt: d.now().UTC(),
	}, nil
}

func (d *Decoder) Destinations(event core.Event) ([]core.WebhookDestination, error) {
	if !common.IsHexAddress(event.Address) {
		return nil, fmt.Errorf("invalid event address %q", event.Address)
	}
	configured, ok := d.contracts[common.HexToAddress(event.Address)]
	if !ok {
		return nil, ErrUnconfiguredEvent
	}
	destinations, ok := configured.routes[event.Signature]
	if !ok || len(destinations) == 0 {
		return nil, fmt.Errorf("event %s has no configured webhook route", event.Signature)
	}
	return append([]core.WebhookDestination(nil), destinations...), nil
}

func jsonSafeValue(value any) any {
	switch typed := value.(type) {
	case *big.Int:
		if typed == nil {
			return nil
		}
		return typed.String()
	case big.Int:
		return typed.String()
	case common.Address:
		return typed.Hex()
	case common.Hash:
		return typed.Hex()
	default:
		return jsonSafeReflect(reflect.ValueOf(value))
	}
}

func jsonSafeReflect(value reflect.Value) any {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		return jsonSafeValue(value.Interface())
	}
	if (value.Kind() == reflect.Array || value.Kind() == reflect.Slice) && value.Type().Elem().Kind() == reflect.Uint8 {
		bytes := make([]byte, value.Len())
		for index := range bytes {
			bytes[index] = byte(value.Index(index).Uint())
		}
		return "0x" + hex.EncodeToString(bytes)
	}
	switch value.Kind() {
	case reflect.Array, reflect.Slice:
		result := make([]any, value.Len())
		for index := range result {
			result[index] = jsonSafeValue(value.Index(index).Interface())
		}
		return result
	case reflect.Struct:
		result := make(map[string]any, value.NumField())
		for index := 0; index < value.NumField(); index++ {
			field := value.Type().Field(index)
			if !field.IsExported() {
				continue
			}
			name := field.Name
			if name != "" {
				name = strings.ToLower(name[:1]) + name[1:]
			}
			result[name] = jsonSafeValue(value.Field(index).Interface())
		}
		return result
	default:
		return value.Interface()
	}
}

func argumentName(name string, position int) string {
	if name == "" {
		return fmt.Sprintf("_%d", position)
	}
	return name
}

func (d *Decoder) Addresses() []common.Address {
	addresses := make([]common.Address, 0, len(d.contracts))
	for address := range d.contracts {
		addresses = append(addresses, address)
	}
	return addresses
}

func (d *Decoder) Topic0() []common.Hash {
	seen := make(map[common.Hash]struct{})
	for _, contract := range d.contracts {
		for topic := range contract.events {
			seen[topic] = struct{}{}
		}
	}
	topics := make([]common.Hash, 0, len(seen))
	for topic := range seen {
		topics = append(topics, topic)
	}
	return topics
}

var _ core.Decoder = (*Decoder)(nil)
