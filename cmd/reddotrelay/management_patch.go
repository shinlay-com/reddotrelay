package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"

	"github.com/ethereum/go-ethereum/common"
)

type patchValue[T any] struct {
	Present bool
	Null    bool
	Value   T
}

func (value *patchValue[T]) UnmarshalJSON(data []byte) error {
	value.Present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		value.Null = true
		return nil
	}
	return json.Unmarshal(data, &value.Value)
}

type listenerTLSPatchRequest struct {
	CAPEM              patchValue[string] `json:"caPem"`
	ServerName         patchValue[string] `json:"serverName"`
	InsecureSkipVerify patchValue[bool]   `json:"insecureSkipVerify"`
}

func (patch *listenerTLSPatchRequest) UnmarshalJSON(data []byte) error {
	type plain listenerTLSPatchRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*plain)(patch))
}

type rpcListenerPatchRequest struct {
	Name              patchValue[string]                   `json:"name"`
	ChainID           patchValue[uint64]                   `json:"chainId"`
	RPCURL            patchValue[string]                   `json:"rpcUrl"`
	RPCURLRef         patchValue[string]                   `json:"rpcUrlRef"`
	RPCAuthentication patchValue[rpcAuthenticationRequest] `json:"rpcAuthentication"`
	StartBlock        patchValue[uint64]                   `json:"startBlock"`
	BatchSize         patchValue[uint64]                   `json:"batchSize"`
	PollInterval      patchValue[string]                   `json:"pollInterval"`
	Confirmations     patchValue[uint64]                   `json:"confirmations"`
	ReorgDepth        patchValue[uint64]                   `json:"reorgDepth"`
	RPCRetryAttempts  patchValue[int]                      `json:"rpcRetryAttempts"`
	RPCRetryBackoff   patchValue[string]                   `json:"rpcRetryBackoff"`
	RPCTimeout        patchValue[string]                   `json:"rpcTimeout"`
	TLS               patchValue[listenerTLSPatchRequest]  `json:"tls"`
}

type contractPatchRequest struct {
	Address        patchValue[string]          `json:"address"`
	ABI            patchValue[json.RawMessage] `json:"abi"`
	EventSelectors patchValue[[]string]        `json:"eventSelectors"`
}

type eventPatchRequest struct {
	Selector patchValue[string] `json:"selector"`
}

type webhookPatchRequest struct {
	URL            patchValue[string]                       `json:"url"`
	URLRef         patchValue[string]                       `json:"urlRef"`
	Authentication patchValue[webhookAuthenticationRequest] `json:"authentication"`
}

func handleRPCListenerPatchResource(store *sqlite.Store, segments []string, writer http.ResponseWriter, request *http.Request) {
	for _, index := range []int{0, 2, 4, 6} {
		if index < len(segments) && segments[index] != "webhooks" && !canonicalUUID(segments[index]) {
			writeAPIError(writer, http.StatusBadRequest, "invalid resource id")
			return
		}
	}
	if len(segments) == 2 && segments[0] == "webhooks" && !canonicalUUID(segments[1]) {
		writeAPIError(writer, http.StatusBadRequest, "invalid resource id")
		return
	}
	switch {
	case len(segments) == 1 && segments[0] != "webhooks":
		patchRPCListener(store, segments[0], writer, request)
	case len(segments) == 2 && segments[0] == "webhooks":
		patchGlobalWebhook(store, segments[1], writer, request)
	case len(segments) == 3 && segments[1] == "webhooks":
		patchNestedWebhook(store, segments[0], "", "", segments[2], writer, request)
	case len(segments) == 3 && segments[1] == "contracts":
		patchContract(store, segments[0], segments[2], writer, request)
	case len(segments) == 5 && segments[1] == "contracts" && segments[3] == "webhooks":
		patchNestedWebhook(store, segments[0], segments[2], "", segments[4], writer, request)
	case len(segments) == 5 && segments[1] == "contracts" && segments[3] == "events":
		patchEvent(store, segments[0], segments[2], segments[4], writer, request)
	case len(segments) == 7 && segments[1] == "contracts" && segments[3] == "events" && segments[5] == "webhooks":
		patchNestedWebhook(store, segments[0], segments[2], segments[4], segments[6], writer, request)
	default:
		writeAPIError(writer, http.StatusNotFound, "resource not found")
	}
}

func patchRPCListener(store *sqlite.Store, listenerID string, writer http.ResponseWriter, request *http.Request) {
	expected, snapshot, listener, ok := loadListenerMutation(store, listenerID, writer, request)
	if !ok {
		return
	}
	var input rpcListenerPatchRequest
	if !decodePatchRequest(writer, request, &input) {
		return
	}
	if err := applyRPCListenerPatch(listener, input); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := validateRPCListenerSnapshot(snapshot); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	revision, err := store.ReplaceRPCListenerAudited(request.Context(), *listener, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionUpdate, core.AuditResourceRPCListener, listenerID, ""))
	if !writeMutationError(writer, revision, err) {
		return
	}
	updated, ok := loadPatchedListener(store, listenerID, writer, request)
	if !ok {
		return
	}
	writePatched(writer, revision, map[string]any{"revision": revision, "rpcListener": rpcListenerResponse(*updated, snapshot.GlobalWebhooks)})
}

func patchContract(store *sqlite.Store, listenerID, contractID string, writer http.ResponseWriter, request *http.Request) {
	expected, snapshot, listener, ok := loadListenerMutation(store, listenerID, writer, request)
	if !ok {
		return
	}
	contract := findContract(listener, contractID)
	if contract == nil {
		writeAPIError(writer, http.StatusNotFound, "contract configuration not found")
		return
	}
	var input contractPatchRequest
	if !decodePatchRequest(writer, request, &input) {
		return
	}
	if err := applyContractPatch(contract, input); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := validateRPCListenerSnapshot(snapshot); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	revision, err := store.ReplaceRPCListenerAudited(request.Context(), *listener, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionUpdate, core.AuditResourceContract, contractID, listenerID))
	if !writeMutationError(writer, revision, err) {
		return
	}
	updated, ok := loadPatchedListener(store, listenerID, writer, request)
	if !ok {
		return
	}
	response := rpcListenerResponse(*updated, snapshot.GlobalWebhooks)
	writePatched(writer, revision, map[string]any{"revision": revision, "contract": findContractResponse(response.Contracts, contractID)})
}

func patchEvent(store *sqlite.Store, listenerID, contractID, eventID string, writer http.ResponseWriter, request *http.Request) {
	expected, snapshot, listener, ok := loadListenerMutation(store, listenerID, writer, request)
	if !ok {
		return
	}
	contract := findContract(listener, contractID)
	if contract == nil {
		writeAPIError(writer, http.StatusNotFound, "contract configuration not found")
		return
	}
	event := findEvent(contract, eventID)
	if event == nil {
		writeAPIError(writer, http.StatusNotFound, "event configuration not found")
		return
	}
	var input eventPatchRequest
	if !decodePatchRequest(writer, request, &input) {
		return
	}
	if err := applyRequiredString("selector", &event.Selector, input.Selector); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := validateRPCListenerSnapshot(snapshot); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	revision, err := store.ReplaceRPCListenerAudited(request.Context(), *listener, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionUpdate, core.AuditResourceEvent, eventID, listenerID))
	if !writeMutationError(writer, revision, err) {
		return
	}
	updated, ok := loadPatchedListener(store, listenerID, writer, request)
	if !ok {
		return
	}
	response := rpcListenerResponse(*updated, snapshot.GlobalWebhooks)
	updatedContract := findContractResponse(response.Contracts, contractID)
	writePatched(writer, revision, map[string]any{"revision": revision, "event": findEventResponse(updatedContract.Events, eventID)})
}

func patchGlobalWebhook(store *sqlite.Store, webhookID string, writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	snapshot, ok := loadExpectedSnapshot(store, expected, writer, request)
	if !ok {
		return
	}
	webhook := findWebhook(snapshot.GlobalWebhooks, webhookID)
	if webhook == nil {
		writeAPIError(writer, http.StatusNotFound, "webhook configuration not found")
		return
	}
	if !decodeAndApplyWebhookPatch(writer, request, webhook) {
		return
	}
	if err := validateRPCListenerSnapshot(snapshot); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	revision, err := store.ReplaceGlobalWebhooksAudited(request.Context(), snapshot.GlobalWebhooks, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionUpdate, core.AuditResourceWebhook, webhookID, ""))
	if !writeMutationError(writer, revision, err) {
		return
	}
	latest, err := store.RPCListenerSnapshot(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "configuration was updated but could not be loaded")
		return
	}
	updated := findWebhook(latest.GlobalWebhooks, webhookID)
	writePatched(writer, revision, map[string]any{"revision": revision, "webhook": webhookResponses([]core.WebhookConfig{*updated})[0]})
}

func patchNestedWebhook(store *sqlite.Store, listenerID, contractID, eventID, webhookID string, writer http.ResponseWriter, request *http.Request) {
	expected, snapshot, listener, ok := loadListenerMutation(store, listenerID, writer, request)
	if !ok {
		return
	}
	var webhooks *[]core.WebhookConfig
	if contractID == "" {
		webhooks = &listener.Webhooks
	} else {
		contract := findContract(listener, contractID)
		if contract == nil {
			writeAPIError(writer, http.StatusNotFound, "contract configuration not found")
			return
		}
		if eventID == "" {
			webhooks = &contract.Webhooks
		} else {
			event := findEvent(contract, eventID)
			if event == nil {
				writeAPIError(writer, http.StatusNotFound, "event configuration not found")
				return
			}
			webhooks = &event.Webhooks
		}
	}
	webhook := findWebhook(*webhooks, webhookID)
	if webhook == nil {
		writeAPIError(writer, http.StatusNotFound, "webhook configuration not found")
		return
	}
	if !decodeAndApplyWebhookPatch(writer, request, webhook) {
		return
	}
	if err := validateRPCListenerSnapshot(snapshot); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	revision, err := store.ReplaceRPCListenerAudited(request.Context(), *listener, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionUpdate, core.AuditResourceWebhook, webhookID, listenerID))
	if !writeMutationError(writer, revision, err) {
		return
	}
	updated, ok := loadPatchedListener(store, listenerID, writer, request)
	if !ok {
		return
	}
	result := findNestedWebhook(updated, contractID, eventID, webhookID)
	writePatched(writer, revision, map[string]any{"revision": revision, "webhook": webhookResponses([]core.WebhookConfig{*result})[0]})
}

func decodeAndApplyWebhookPatch(writer http.ResponseWriter, request *http.Request, webhook *core.WebhookConfig) bool {
	var input webhookPatchRequest
	if !decodePatchRequest(writer, request, &input) {
		return false
	}
	if input.URL.Present && input.URLRef.Present {
		writeAPIError(writer, http.StatusUnprocessableEntity, "url and urlRef cannot be changed together")
		return false
	}
	if err := applyRequiredString("url", &webhook.URL, input.URL); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return false
	}
	if input.URL.Present {
		webhook.URLRef = ""
	}
	if err := applyRequiredString("urlRef", &webhook.URLRef, input.URLRef); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return false
	}
	if input.URLRef.Present {
		webhook.URL = ""
	}
	if input.Authentication.Present {
		if input.Authentication.Null {
			webhook.Authentication = core.WebhookAuthentication{}
		} else {
			value := input.Authentication.Value
			webhook.Authentication = core.WebhookAuthentication{Type: value.Type, SecretRef: value.SecretRef, KeyID: value.KeyID}
		}
	}
	return true
}

func applyRPCListenerPatch(listener *core.RPCListener, input rpcListenerPatchRequest) error {
	if err := applyRequiredString("name", &listener.Name, input.Name); err != nil {
		return err
	}
	if input.RPCURL.Present && input.RPCURLRef.Present {
		return errors.New("rpcUrl and rpcUrlRef cannot be changed together")
	}
	if err := applyRequiredString("rpcUrl", &listener.RPCURL, input.RPCURL); err != nil {
		return err
	}
	if input.RPCURL.Present {
		listener.RPCURLRef = ""
	}
	if err := applyRequiredString("rpcUrlRef", &listener.RPCURLRef, input.RPCURLRef); err != nil {
		return err
	}
	if input.RPCURLRef.Present {
		listener.RPCURL = ""
	}
	if input.RPCAuthentication.Present {
		if input.RPCAuthentication.Null {
			listener.RPCAuthentication = core.RPCAuthentication{}
		} else {
			value := input.RPCAuthentication.Value
			next := core.RPCAuthentication{Type: value.Type, Username: value.Username, HeaderName: value.HeaderName, Secret: value.Secret, TokenURL: value.TokenURL, TokenAPIKey: value.TokenAPIKey}
			// Secrets are write-only in the management API. Empty secret fields on a
			// patch keep the existing value when the authentication method is unchanged.
			if next.Type == listener.RPCAuthentication.Type {
				if next.Secret == "" {
					next.Secret = listener.RPCAuthentication.Secret
				}
				if next.TokenAPIKey == "" {
					next.TokenAPIKey = listener.RPCAuthentication.TokenAPIKey
				}
			}
			listener.RPCAuthentication = next
		}
	}
	if err := applyRequiredValue("chainId", &listener.ChainID, input.ChainID); err != nil {
		return err
	}
	if err := applyRequiredValue("startBlock", &listener.StartBlock, input.StartBlock); err != nil {
		return err
	}
	if err := applyRequiredValue("batchSize", &listener.BatchSize, input.BatchSize); err != nil {
		return err
	}
	if err := applyRequiredValue("confirmations", &listener.Confirmations, input.Confirmations); err != nil {
		return err
	}
	if err := applyRequiredValue("reorgDepth", &listener.ReorgDepth, input.ReorgDepth); err != nil {
		return err
	}
	if err := applyRequiredValue("rpcRetryAttempts", &listener.RPCRetryAttempts, input.RPCRetryAttempts); err != nil {
		return err
	}
	if err := applyDurationPatch("pollInterval", &listener.PollInterval, input.PollInterval); err != nil {
		return err
	}
	if err := applyDurationPatch("rpcRetryBackoff", &listener.RPCRetryBackoff, input.RPCRetryBackoff); err != nil {
		return err
	}
	if err := applyDurationPatch("rpcTimeout", &listener.RPCTimeout, input.RPCTimeout); err != nil {
		return err
	}
	if input.TLS.Present {
		if input.TLS.Null {
			return errors.New("tls cannot be null")
		}
		tls := input.TLS.Value
		if tls.CAPEM.Present {
			if tls.CAPEM.Null {
				listener.TLS.CAPEM = ""
			} else {
				listener.TLS.CAPEM = tls.CAPEM.Value
			}
		}
		if tls.ServerName.Present {
			if tls.ServerName.Null {
				listener.TLS.ServerName = ""
			} else {
				listener.TLS.ServerName = tls.ServerName.Value
			}
		}
		if err := applyRequiredValue("tls.insecureSkipVerify", &listener.TLS.InsecureSkipVerify, tls.InsecureSkipVerify); err != nil {
			return err
		}
	}
	return nil
}

func applyContractPatch(contract *core.ContractConfig, input contractPatchRequest) error {
	if input.Address.Present {
		if input.Address.Null {
			return errors.New("address cannot be null")
		}
		if !common.IsHexAddress(input.Address.Value) {
			return errors.New("address must be a canonical EVM address")
		}
		checksummed := common.HexToAddress(input.Address.Value).Hex()
		if input.Address.Value != checksummed && input.Address.Value != strings.ToLower(checksummed) {
			return errors.New("address must be lowercase or EIP-55 checksummed")
		}
		contract.Address = checksummed
	}
	if input.ABI.Present {
		if input.ABI.Null {
			return errors.New("abi cannot be null")
		}
		contract.ABI = append(json.RawMessage(nil), input.ABI.Value...)
	}
	if input.EventSelectors.Present {
		if input.EventSelectors.Null {
			return errors.New("eventSelectors cannot be null")
		}
		existing := make(map[string]core.EventConfig, len(contract.Events))
		for _, event := range contract.Events {
			existing[event.Selector] = event
		}
		seen := make(map[string]struct{}, len(input.EventSelectors.Value))
		events := make([]core.EventConfig, 0, len(input.EventSelectors.Value))
		for _, selector := range input.EventSelectors.Value {
			if strings.TrimSpace(selector) == "" {
				return errors.New("eventSelectors cannot contain an empty selector")
			}
			if _, duplicate := seen[selector]; duplicate {
				return fmt.Errorf("eventSelectors contains duplicate selector %q", selector)
			}
			seen[selector] = struct{}{}
			if event, ok := existing[selector]; ok {
				events = append(events, event)
			} else {
				events = append(events, core.EventConfig{ID: core.NewConfigID(), Selector: selector, Webhooks: []core.WebhookConfig{}})
			}
		}
		contract.Events = events
	}
	return nil
}

func applyRequiredString(field string, target *string, value patchValue[string]) error {
	if !value.Present {
		return nil
	}
	if value.Null {
		return fmt.Errorf("%s cannot be null", field)
	}
	*target = value.Value
	return nil
}

func applyRequiredValue[T any](field string, target *T, value patchValue[T]) error {
	if !value.Present {
		return nil
	}
	if value.Null {
		return fmt.Errorf("%s cannot be null", field)
	}
	*target = value.Value
	return nil
}

func applyDurationPatch(field string, target *time.Duration, value patchValue[string]) error {
	if !value.Present {
		return nil
	}
	if value.Null {
		return fmt.Errorf("%s cannot be null", field)
	}
	duration, err := positiveDuration(field, value.Value)
	if err != nil {
		return err
	}
	*target = duration
	return nil
}

func decodePatchRequest(writer http.ResponseWriter, request *http.Request, destination any) bool {
	mediaType := strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0])
	if mediaType != "application/merge-patch+json" {
		writeAPIError(writer, http.StatusUnsupportedMediaType, "Content-Type must be application/merge-patch+json")
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, rpcListenerBodyLimit)
	decoder := json.NewDecoder(request.Body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		writeAPIError(writer, http.StatusBadRequest, "request body must be one valid JSON object with known mutable fields")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(writer, http.StatusBadRequest, "request body must contain one JSON object")
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		writeAPIError(writer, http.StatusBadRequest, "request body must contain at least one mutable field")
		return false
	}
	patchDecoder := json.NewDecoder(bytes.NewReader(raw))
	patchDecoder.DisallowUnknownFields()
	if err := patchDecoder.Decode(destination); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "request body must be one valid JSON object with known mutable fields")
		return false
	}
	return true
}

func loadPatchedListener(store *sqlite.Store, id string, writer http.ResponseWriter, request *http.Request) (*core.RPCListener, bool) {
	config, _, err := store.RPCListener(request.Context(), id)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "configuration was updated but could not be loaded")
		return nil, false
	}
	return &config, true
}

func writePatched(writer http.ResponseWriter, revision uint64, body any) {
	writer.Header().Set("ETag", revisionETag(revision))
	writeJSON(writer, http.StatusOK, body)
}
