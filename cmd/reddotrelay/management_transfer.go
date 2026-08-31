package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
)

const (
	configurationSchemaVersion = "reddotrelay.config/v1"
	configurationAuditID       = "00000000-0000-0000-0000-000000000000"
)

type configurationDocument struct {
	SchemaVersion  string                  `json:"schemaVersion"`
	GlobalWebhooks []configurationWebhook  `json:"globalWebhooks"`
	RPCListeners   []configurationListener `json:"rpcListeners"`
}

type configurationWebhook struct {
	ID             string                        `json:"id"`
	URL            string                        `json:"url,omitempty"`
	URLRef         string                        `json:"urlRef,omitempty"`
	Authentication *webhookAuthenticationRequest `json:"authentication,omitempty"`
}

type configurationEvent struct {
	ID       string                 `json:"id"`
	Selector string                 `json:"selector"`
	Webhooks []configurationWebhook `json:"webhooks"`
}
type configurationContract struct {
	ID       string                 `json:"id"`
	Address  string                 `json:"address"`
	ABI      json.RawMessage        `json:"abi"`
	Webhooks []configurationWebhook `json:"webhooks"`
	Events   []configurationEvent   `json:"events"`
}
type configurationListener struct {
	ID               string                   `json:"id"`
	Name             string                   `json:"name"`
	Paused           bool                     `json:"paused"`
	ChainID          uint64                   `json:"chainId"`
	RPCURL           string                   `json:"rpcUrl,omitempty"`
	RPCURLRef        string                   `json:"rpcUrlRef,omitempty"`
	StartBlock       uint64                   `json:"startBlock"`
	BatchSize        uint64                   `json:"batchSize"`
	PollInterval     string                   `json:"pollInterval"`
	Confirmations    uint64                   `json:"confirmations"`
	ReorgDepth       uint64                   `json:"reorgDepth"`
	RPCRetryAttempts int                      `json:"rpcRetryAttempts"`
	RPCRetryBackoff  string                   `json:"rpcRetryBackoff"`
	RPCTimeout       string                   `json:"rpcTimeout"`
	TLS              listenerTLSCreateRequest `json:"tls"`
	Webhooks         []configurationWebhook   `json:"webhooks"`
	Contracts        []configurationContract  `json:"contracts"`
}

func handleRPCListenerExport(store *sqlite.Store, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireAdmin(writer, request) {
		return
	}
	snapshot, err := store.RPCListenerSnapshot(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
		return
	}
	document, err := exportConfiguration(snapshot)
	if err != nil {
		writeAPIError(writer, http.StatusConflict, "configuration contains credentials that cannot be safely exported")
		return
	}
	writer.Header().Set("ETag", revisionETag(snapshot.Revision))
	writeJSON(writer, http.StatusOK, document)
}

func handleRPCListenerImport(store *sqlite.Store, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPut {
		writer.Header().Set("Allow", http.MethodPut)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireAdmin(writer, request) {
		return
	}
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var document configurationDocument
	if !decodeCreateRequest(writer, request, &document) {
		return
	}
	if document.SchemaVersion != configurationSchemaVersion {
		writeAPIError(writer, http.StatusUnprocessableEntity, "unsupported configuration schemaVersion")
		return
	}
	snapshot, err := importConfiguration(document)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	current, ok := loadExpectedSnapshot(store, expected, writer, request)
	if !ok {
		return
	}
	snapshot.Revision = current.Revision
	if err := validateRPCListenerSnapshot(snapshot); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	revision, err := store.ReplaceRPCListenerSnapshotAudited(request.Context(), snapshot, expected, time.Now().UTC(), mutationAudit(request, core.AuditActionImport, core.AuditResourceConfiguration, configurationAuditID, ""))
	if !writeMutationError(writer, revision, err) {
		return
	}
	writer.Header().Set("ETag", revisionETag(revision))
	writeJSON(writer, http.StatusOK, map[string]any{"revision": revision, "rpcListeners": len(snapshot.Listeners), "globalWebhooks": len(snapshot.GlobalWebhooks)})
}

func exportConfiguration(snapshot core.RPCListenerSnapshot) (configurationDocument, error) {
	document := configurationDocument{SchemaVersion: configurationSchemaVersion, GlobalWebhooks: []configurationWebhook{}, RPCListeners: []configurationListener{}}
	var err error
	if document.GlobalWebhooks, err = exportWebhooks(snapshot.GlobalWebhooks); err != nil {
		return configurationDocument{}, err
	}
	for _, listener := range snapshot.Listeners {
		if listener.RPCAuthentication.Type != "" {
			return configurationDocument{}, errors.New("RPC authentication credentials cannot be exported")
		}
		if listener.RPCURL != "" && !safeExportURL(listener.RPCURL) {
			return configurationDocument{}, errors.New("unsafe direct RPC URL")
		}
		exported := configurationListener{ID: listener.ID, Name: listener.Name, Paused: listener.Paused, ChainID: listener.ChainID, RPCURL: listener.RPCURL, RPCURLRef: listener.RPCURLRef, StartBlock: listener.StartBlock, BatchSize: listener.BatchSize, PollInterval: listener.PollInterval.String(), Confirmations: listener.Confirmations, ReorgDepth: listener.ReorgDepth, RPCRetryAttempts: listener.RPCRetryAttempts, RPCRetryBackoff: listener.RPCRetryBackoff.String(), RPCTimeout: listener.RPCTimeout.String(), TLS: listenerTLSCreateRequest{CAPEM: listener.TLS.CAPEM, ServerName: listener.TLS.ServerName, InsecureSkipVerify: listener.TLS.InsecureSkipVerify}, Webhooks: []configurationWebhook{}, Contracts: []configurationContract{}}
		if exported.Webhooks, err = exportWebhooks(listener.Webhooks); err != nil {
			return configurationDocument{}, err
		}
		for _, contract := range listener.Contracts {
			item := configurationContract{ID: contract.ID, Address: contract.Address, ABI: append(json.RawMessage(nil), contract.ABI...), Webhooks: []configurationWebhook{}, Events: []configurationEvent{}}
			if item.Webhooks, err = exportWebhooks(contract.Webhooks); err != nil {
				return configurationDocument{}, err
			}
			for _, event := range contract.Events {
				exportedEvent := configurationEvent{ID: event.ID, Selector: event.Selector, Webhooks: []configurationWebhook{}}
				if exportedEvent.Webhooks, err = exportWebhooks(event.Webhooks); err != nil {
					return configurationDocument{}, err
				}
				item.Events = append(item.Events, exportedEvent)
			}
			exported.Contracts = append(exported.Contracts, item)
		}
		document.RPCListeners = append(document.RPCListeners, exported)
	}
	return document, nil
}

func exportWebhooks(webhooks []core.WebhookConfig) ([]configurationWebhook, error) {
	result := make([]configurationWebhook, 0, len(webhooks))
	for _, webhook := range webhooks {
		if webhook.URL != "" && !safeExportURL(webhook.URL) {
			return nil, errors.New("unsafe direct webhook URL")
		}
		item := configurationWebhook{ID: webhook.ID, URL: webhook.URL, URLRef: webhook.URLRef}
		if webhook.Authentication.Type != "" {
			item.Authentication = &webhookAuthenticationRequest{Type: webhook.Authentication.Type, SecretRef: webhook.Authentication.SecretRef, KeyID: webhook.Authentication.KeyID}
		}
		result = append(result, item)
	}
	return result, nil
}

func safeExportURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && (parsed.Path == "" || parsed.Path == "/")
}

func importConfiguration(document configurationDocument) (core.RPCListenerSnapshot, error) {
	snapshot := core.RPCListenerSnapshot{GlobalWebhooks: []core.WebhookConfig{}, Listeners: []core.RPCListener{}}
	ids := map[string]map[string]struct{}{"listener": {}, "contract": {}, "event": {}, "webhook": {}}
	var err error
	if snapshot.GlobalWebhooks, err = importWebhooks(document.GlobalWebhooks, ids["webhook"]); err != nil {
		return core.RPCListenerSnapshot{}, err
	}
	for _, input := range document.RPCListeners {
		if err := uniqueImportID("listener", input.ID, ids["listener"]); err != nil {
			return core.RPCListenerSnapshot{}, err
		}
		if input.RPCURL != "" && !safeExportURL(input.RPCURL) {
			return core.RPCListenerSnapshot{}, errors.New("rpcUrl is not secret-safe; use rpcUrlRef")
		}
		poll, err := positiveDuration("pollInterval", input.PollInterval)
		if err != nil {
			return core.RPCListenerSnapshot{}, err
		}
		backoff, err := positiveDuration("rpcRetryBackoff", input.RPCRetryBackoff)
		if err != nil {
			return core.RPCListenerSnapshot{}, err
		}
		timeout, err := positiveDuration("rpcTimeout", input.RPCTimeout)
		if err != nil {
			return core.RPCListenerSnapshot{}, err
		}
		listener := core.RPCListener{ID: input.ID, Name: input.Name, Paused: input.Paused, ChainID: input.ChainID, RPCURL: input.RPCURL, RPCURLRef: input.RPCURLRef, StartBlock: input.StartBlock, BatchSize: input.BatchSize, PollInterval: poll, Confirmations: input.Confirmations, ReorgDepth: input.ReorgDepth, RPCRetryAttempts: input.RPCRetryAttempts, RPCRetryBackoff: backoff, RPCTimeout: timeout, TLS: core.ListenerTLSConfig{CAPEM: input.TLS.CAPEM, ServerName: input.TLS.ServerName, InsecureSkipVerify: input.TLS.InsecureSkipVerify}, Webhooks: []core.WebhookConfig{}, Contracts: []core.ContractConfig{}}
		if listener.Webhooks, err = importWebhooks(input.Webhooks, ids["webhook"]); err != nil {
			return core.RPCListenerSnapshot{}, err
		}
		for _, contractInput := range input.Contracts {
			if err := uniqueImportID("contract", contractInput.ID, ids["contract"]); err != nil {
				return core.RPCListenerSnapshot{}, err
			}
			contract := core.ContractConfig{ID: contractInput.ID, Address: contractInput.Address, ABI: append(json.RawMessage(nil), contractInput.ABI...), Webhooks: []core.WebhookConfig{}, Events: []core.EventConfig{}}
			if contract.Webhooks, err = importWebhooks(contractInput.Webhooks, ids["webhook"]); err != nil {
				return core.RPCListenerSnapshot{}, err
			}
			for _, eventInput := range contractInput.Events {
				if err := uniqueImportID("event", eventInput.ID, ids["event"]); err != nil {
					return core.RPCListenerSnapshot{}, err
				}
				event := core.EventConfig{ID: eventInput.ID, Selector: eventInput.Selector, Webhooks: []core.WebhookConfig{}}
				if event.Webhooks, err = importWebhooks(eventInput.Webhooks, ids["webhook"]); err != nil {
					return core.RPCListenerSnapshot{}, err
				}
				contract.Events = append(contract.Events, event)
			}
			listener.Contracts = append(listener.Contracts, contract)
		}
		snapshot.Listeners = append(snapshot.Listeners, listener)
	}
	return snapshot, nil
}

func importWebhooks(inputs []configurationWebhook, seen map[string]struct{}) ([]core.WebhookConfig, error) {
	result := make([]core.WebhookConfig, 0, len(inputs))
	for _, input := range inputs {
		if err := uniqueImportID("webhook", input.ID, seen); err != nil {
			return nil, err
		}
		if input.URL != "" && !safeExportURL(input.URL) {
			return nil, errors.New("webhook url is not secret-safe; use urlRef")
		}
		item := core.WebhookConfig{ID: input.ID, URL: input.URL, URLRef: input.URLRef}
		if input.Authentication != nil {
			item.Authentication = core.WebhookAuthentication{Type: input.Authentication.Type, SecretRef: input.Authentication.SecretRef, KeyID: input.Authentication.KeyID}
		}
		result = append(result, item)
	}
	return result, nil
}

func uniqueImportID(kind, id string, seen map[string]struct{}) error {
	if !canonicalUUID(id) {
		return fmt.Errorf("%s id must be a canonical UUID", kind)
	}
	if _, exists := seen[id]; exists {
		return fmt.Errorf("%s id must be unique", kind)
	}
	seen[id] = struct{}{}
	return nil
}
