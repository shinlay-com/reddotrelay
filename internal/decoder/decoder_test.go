package decoder

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"reddotrelay/internal/core"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

const testABI = `[
  {"anonymous":false,"inputs":[
    {"indexed":true,"name":"from","type":"address"},
    {"indexed":true,"name":"to","type":"address"},
    {"indexed":false,"name":"value","type":"uint256"}
  ],"name":"Transfer","type":"event"},
  {"anonymous":false,"inputs":[
    {"indexed":true,"name":"owner","type":"address"},
    {"indexed":true,"name":"spender","type":"address"},
    {"indexed":false,"name":"value","type":"uint256"}
  ],"name":"Approval","type":"event"}
]`

func TestJSONSafeValueNormalizesNestedABIValues(t *testing.T) {
	type sample struct {
		Owner  common.Address
		Amount *big.Int
		Data   [4]byte
	}
	value := jsonSafeValue([]sample{{
		Owner:  common.HexToAddress("0x1000000000000000000000000000000000000001"),
		Amount: new(big.Int).Exp(big.NewInt(2), big.NewInt(200), nil),
		Data:   [4]byte{0xde, 0xad, 0xbe, 0xef},
	}})
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, `"owner":"0x1000000000000000000000000000000000000001"`) ||
		!strings.Contains(text, `"amount":"1606938044258990275541962092341162602522202993782792835301376"`) ||
		!strings.Contains(text, `"data":"0xdeadbeef"`) {
		t.Fatalf("normalized ABI value = %s", text)
	}
}

func TestLoadAndDecodeSelectedEvent(t *testing.T) {
	address := common.HexToAddress("0x1000000000000000000000000000000000000001")
	decoder := loadTestDecoder(t, address, []string{"Transfer"})
	fixedTime := time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC)
	decoder.now = func() time.Time { return fixedTime }
	from := common.HexToAddress("0x2000000000000000000000000000000000000002")
	to := common.HexToAddress("0x3000000000000000000000000000000000000003")
	raw := transferLog(t, address, from, to, 42)

	event, err := decoder.Decode(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if event.Name != "Transfer" || event.Signature != "Transfer(address,address,uint256)" {
		t.Fatalf("decoded identity = %s %s", event.Name, event.Signature)
	}
	if event.ID.ChainID != raw.ChainID || event.ID.TransactionHash != raw.TransactionHash ||
		event.BlockNumber != raw.BlockNumber || event.BlockHash != raw.BlockHash {
		t.Fatalf("raw chain metadata was not preserved: %#v", event)
	}
	if event.ObservedAt != fixedTime || string(event.RawData) != string(raw.Data) || len(event.RawTopics) != 3 {
		t.Fatalf("raw event metadata was not preserved: %#v", event)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(event.DecodedPayload, &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["from"]) != `"`+from.Hex()+`"` || string(payload["to"]) != `"`+to.Hex()+`"` || string(payload["value"]) != `"42"` {
		t.Fatalf("decoded payload = %s", event.DecodedPayload)
	}
}

func TestDecodeRejectsUnselectedEventAndAddress(t *testing.T) {
	address := common.HexToAddress("0x1000000000000000000000000000000000000001")
	decoder := loadTestDecoder(t, address, []string{"Approval(address,address,uint256)"})
	raw := transferLog(t, address, common.Address{}, common.Address{}, 1)
	if _, err := decoder.Decode(context.Background(), raw); !errors.Is(err, ErrUnconfiguredEvent) {
		t.Fatalf("unselected event error = %v", err)
	}
	raw.Address = common.HexToAddress("0x9999999999999999999999999999999999999999").Hex()
	if _, err := decoder.Decode(context.Background(), raw); !errors.Is(err, ErrUnconfiguredEvent) {
		t.Fatalf("unconfigured address error = %v", err)
	}
}

func TestDecodeRejectsMalformedLogData(t *testing.T) {
	address := common.HexToAddress("0x1000000000000000000000000000000000000001")
	decoder := loadTestDecoder(t, address, []string{"Transfer"})
	raw := transferLog(t, address, common.Address{}, common.Address{}, 1)
	raw.Data = []byte{1, 2, 3}
	if _, err := decoder.Decode(context.Background(), raw); err == nil {
		t.Fatal("Decode() error = nil, want malformed data error")
	}
}

func TestLoadMultipleContractSubscriptions(t *testing.T) {
	path := writeABI(t)
	first := common.HexToAddress("0x1000000000000000000000000000000000000001")
	second := common.HexToAddress("0x2000000000000000000000000000000000000002")
	decoder, err := Load(filepath.Dir(path), []ContractSpec{
		{Address: first.Hex(), ABIPath: filepath.Base(path), Events: eventSpecs("Transfer")},
		{Address: second.Hex(), ABIPath: filepath.Base(path), Events: eventSpecs("Approval(address,address,uint256)")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(decoder.Addresses()) != 2 || len(decoder.Topic0()) != 2 {
		t.Fatalf("subscription filters = addresses:%v topics:%v", decoder.Addresses(), decoder.Topic0())
	}
}

func TestLoadInlineABI(t *testing.T) {
	address := common.HexToAddress("0x1000000000000000000000000000000000000001")
	decoded, err := Load("", []ContractSpec{{
		Address: address.Hex(), ABIJSON: []byte(testABI), Events: eventSpecs("Transfer"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Addresses()) != 1 || len(decoded.Topic0()) != 1 {
		t.Fatalf("inline ABI subscription filters = addresses:%v topics:%v", decoded.Addresses(), decoded.Topic0())
	}
	_, err = Load("", []ContractSpec{{
		Address: address.Hex(), ABIPath: "contract.json", ABIJSON: []byte(testABI), Events: eventSpecs("Transfer"),
	}})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("path plus inline ABI error = %v", err)
	}
}

func TestLoadRejectsUnknownSelector(t *testing.T) {
	path := writeABI(t)
	_, err := Load(filepath.Dir(path), []ContractSpec{{
		Address: common.HexToAddress("0x1").Hex(), ABIPath: filepath.Base(path), Events: eventSpecs("Missing"),
	}})
	if err == nil {
		t.Fatal("Load() error = nil, want unknown-selector error")
	}
}

func TestLoadRejectsSameABIEventSelectedTwice(t *testing.T) {
	path := writeABI(t)
	_, err := Load(filepath.Dir(path), []ContractSpec{{
		Address: common.HexToAddress("0x1").Hex(), ABIPath: filepath.Base(path),
		Events: append(eventSpecs("Transfer"), eventSpecs("Transfer(address,address,uint256)")...),
	}})
	if err == nil || !strings.Contains(err.Error(), "selected more than once") {
		t.Fatalf("Load() error = %v, want duplicate ABI event error", err)
	}
}

func TestProcessorPersistsDecodedBatchAndSkipsUnselectedLogs(t *testing.T) {
	address := common.HexToAddress("0x1000000000000000000000000000000000000001")
	decoder := loadTestDecoder(t, address, []string{"Transfer"})
	store := &recordingStore{}
	processor := NewProcessor(decoder, store)
	processor.now = func() time.Time { return time.Unix(10, 0).UTC() }
	selected := transferLog(t, address, common.Address{}, common.Address{}, 7)
	unselected := selected
	unselected.Topics = append([]string(nil), selected.Topics...)
	unselected.Topics[0] = common.HexToHash("0x999").Hex()
	unselected.LogIndex++
	checkpoint := core.Checkpoint{ChainID: 1, BlockNumber: 20, BlockHash: "0xblock"}

	if err := processor.ProcessBatch(context.Background(), []core.RawLog{selected, unselected}, checkpoint); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 1 || len(store.deliveries) != 1 || store.checkpoint != checkpoint {
		t.Fatalf("persisted batch = events:%d deliveries:%d checkpoint:%#v", len(store.events), len(store.deliveries), store.checkpoint)
	}
}

func TestProcessorPersistsEventSpecificDestinations(t *testing.T) {
	address := common.HexToAddress("0x1000000000000000000000000000000000000001")
	path := writeABI(t)
	decoder, err := Load(filepath.Dir(path), []ContractSpec{{
		Address: address.Hex(), ABIPath: filepath.Base(path),
		Events: []EventSpec{
			{Selector: "Transfer", Destinations: []core.WebhookDestination{{Locator: "https://one.example.test/hook"}, {Locator: "https://two.example.test/hook"}}},
			{Selector: "Approval", Destinations: []core.WebhookDestination{{Locator: "https://approval.example.test/hook"}}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	processor := NewProcessor(decoder, store)
	processor.now = func() time.Time { return time.Unix(10, 0).UTC() }
	transfer := transferLog(t, address, common.Address{}, common.Address{}, 7)
	approval := approvalLog(t, address, common.Address{}, common.Address{}, 8)
	approval.LogIndex++
	checkpoint := core.Checkpoint{ChainID: 1, BlockNumber: 20, BlockHash: "0xblock"}

	if err := processor.ProcessBatch(context.Background(), []core.RawLog{transfer, approval}, checkpoint); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 2 || len(store.deliveries) != 3 {
		t.Fatalf("persisted routed batch = events:%d deliveries:%d", len(store.events), len(store.deliveries))
	}
	got := []string{store.deliveries[0].Destination, store.deliveries[1].Destination, store.deliveries[2].Destination}
	want := []string{"https://one.example.test/hook", "https://two.example.test/hook", "https://approval.example.test/hook"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery routes = %#v, want %#v", got, want)
	}
}

func loadTestDecoder(t *testing.T, address common.Address, events []string) *Decoder {
	t.Helper()
	path := writeABI(t)
	decoder, err := Load(filepath.Dir(path), []ContractSpec{{
		Address: address.Hex(), ABIPath: filepath.Base(path), Events: eventSpecs(events...),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return decoder
}

func eventSpecs(selectors ...string) []EventSpec {
	specs := make([]EventSpec, len(selectors))
	for i, selector := range selectors {
		specs[i] = EventSpec{Selector: selector, Destinations: []core.WebhookDestination{{Locator: "https://example.test/hook"}}}
	}
	return specs
}

func writeABI(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "contract.abi.json")
	if err := os.WriteFile(path, []byte(testABI), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func transferLog(t *testing.T, address, from, to common.Address, value int64) core.RawLog {
	t.Helper()
	parsed, err := abi.JSON(strings.NewReader(testABI))
	if err != nil {
		t.Fatal(err)
	}
	event := parsed.Events["Transfer"]
	data, err := event.Inputs.NonIndexed().Pack(big.NewInt(value))
	if err != nil {
		t.Fatal(err)
	}
	return core.RawLog{
		ChainID: 1, BlockNumber: 20, BlockHash: common.HexToHash("0x20").Hex(),
		TransactionHash: common.HexToHash("0xabc").Hex(), LogIndex: 3, Address: address.Hex(),
		Topics: []string{event.ID.Hex(), common.BytesToHash(from.Bytes()).Hex(), common.BytesToHash(to.Bytes()).Hex()},
		Data:   data,
	}
}

func approvalLog(t *testing.T, address, owner, spender common.Address, value int64) core.RawLog {
	t.Helper()
	parsed, err := abi.JSON(strings.NewReader(testABI))
	if err != nil {
		t.Fatal(err)
	}
	event := parsed.Events["Approval"]
	data, err := event.Inputs.NonIndexed().Pack(big.NewInt(value))
	if err != nil {
		t.Fatal(err)
	}
	return core.RawLog{
		ChainID: 1, BlockNumber: 20, BlockHash: common.HexToHash("0x20").Hex(),
		TransactionHash: common.HexToHash("0xdef").Hex(), LogIndex: 3, Address: address.Hex(),
		Topics: []string{event.ID.Hex(), common.BytesToHash(owner.Bytes()).Hex(), common.BytesToHash(spender.Bytes()).Hex()},
		Data:   data,
	}
}

type recordingStore struct {
	events     []core.Event
	deliveries []core.Delivery
	checkpoint core.Checkpoint
}

func (s *recordingStore) SaveEventsAndCheckpoint(_ context.Context, events []core.Event, deliveries []core.Delivery, checkpoint core.Checkpoint) error {
	s.events = append([]core.Event(nil), events...)
	s.deliveries = append([]core.Delivery(nil), deliveries...)
	s.checkpoint = checkpoint
	return nil
}
