package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/secrets"
	"reddotrelay/internal/store/sqlite"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

const rpcListenerBodyLimit = 4 << 20

type apiKeyPrincipalContextKey struct{}

type webhookCreateRequest struct {
	URL            string                       `json:"url"`
	URLRef         string                       `json:"urlRef"`
	Authentication webhookAuthenticationRequest `json:"authentication,omitempty"`
}

type webhookAuthenticationRequest struct {
	Type      string `json:"type"`
	SecretRef string `json:"secretRef"`
	KeyID     string `json:"keyId,omitempty"`
}

type eventCreateRequest struct {
	Selector string                 `json:"selector"`
	Webhooks []webhookCreateRequest `json:"webhooks,omitempty"`
}

type contractCreateRequest struct {
	Address  string                 `json:"address"`
	ABI      json.RawMessage        `json:"abi"`
	Webhooks []webhookCreateRequest `json:"webhooks,omitempty"`
	Events   []eventCreateRequest   `json:"events,omitempty"`
}

type listenerTLSCreateRequest struct {
	CAPEM              string `json:"caPem,omitempty"`
	ServerName         string `json:"serverName,omitempty"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty"`
}

type rpcAuthenticationRequest struct {
	Type        string `json:"type"`
	Username    string `json:"username,omitempty"`
	HeaderName  string `json:"headerName,omitempty"`
	Secret      string `json:"secret"`
	TokenURL    string `json:"tokenUrl,omitempty"`
	TokenAPIKey string `json:"tokenApiKey,omitempty"`
}

type rpcListenerCreateRequest struct {
	Name              string                   `json:"name"`
	Paused            bool                     `json:"paused"`
	ChainID           uint64                   `json:"chainId"`
	RPCURL            string                   `json:"rpcUrl"`
	RPCURLRef         string                   `json:"rpcUrlRef"`
	RPCAuthentication rpcAuthenticationRequest `json:"rpcAuthentication,omitempty"`
	StartBlock        uint64                   `json:"startBlock"`
	BatchSize         uint64                   `json:"batchSize"`
	PollInterval      string                   `json:"pollInterval"`
	Confirmations     uint64                   `json:"confirmations"`
	ReorgDepth        uint64                   `json:"reorgDepth"`
	RPCRetryAttempts  int                      `json:"rpcRetryAttempts"`
	RPCRetryBackoff   string                   `json:"rpcRetryBackoff"`
	RPCTimeout        string                   `json:"rpcTimeout"`
	TLS               listenerTLSCreateRequest `json:"tls,omitempty"`
	Webhooks          []webhookCreateRequest   `json:"webhooks,omitempty"`
	Contracts         []contractCreateRequest  `json:"contracts,omitempty"`
}

func handleRPCListenersCollection(store *sqlite.Store, writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	switch request.Method {
	case http.MethodGet:
		snapshot, err := store.RPCListenerSnapshot(request.Context())
		if err != nil {
			writeAPIError(writer, http.StatusInternalServerError, "internal server error")
			return
		}
		writer.Header().Set("ETag", revisionETag(snapshot.Revision))
		writeJSON(writer, http.StatusOK, rpcListenerListResponse(snapshot))
	case http.MethodPost:
		if !requireAdmin(writer, request) {
			return
		}
		createRPCListener(store, writer, request)
	default:
		writer.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleRPCListenersResource(store *sqlite.Store, writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	segments := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/v1/rpc-listeners/"), "/")
	if request.Method == http.MethodGet {
		if len(segments) != 1 || !canonicalUUID(segments[0]) {
			writeAPIError(writer, http.StatusBadRequest, "invalid RPC listener id")
			return
		}
		getRPCListener(store, segments[0], writer, request)
		return
	}
	if request.Method == http.MethodPatch {
		if !requireAdmin(writer, request) {
			return
		}
		handleRPCListenerPatchResource(store, segments, writer, request)
		return
	}
	if request.Method == http.MethodDelete {
		if !requireAdmin(writer, request) {
			return
		}
		handleRPCListenerDeleteResource(store, segments, writer, request)
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodGet+", "+http.MethodPost+", "+http.MethodPatch+", "+http.MethodDelete)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireAdmin(writer, request) {
		return
	}
	handleRPCListenerCreateResource(store, segments, writer, request)
}

func getRPCListener(store *sqlite.Store, id string, writer http.ResponseWriter, request *http.Request) {
	snapshot, err := store.RPCListenerSnapshot(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
		return
	}
	config, _ := findListener(snapshot, id)
	if config == nil {
		writeAPIError(writer, http.StatusNotFound, "RPC listener not found")
		return
	}
	writer.Header().Set("ETag", revisionETag(snapshot.Revision))
	writeJSON(writer, http.StatusOK, map[string]any{"revision": snapshot.Revision, "rpcListener": rpcListenerResponse(*config, snapshot.GlobalWebhooks)})
}

func handleRPCListenerCreateResource(store *sqlite.Store, segments []string, writer http.ResponseWriter, request *http.Request) {
	for _, index := range []int{0, 2, 4} {
		if index < len(segments) && segments[index] != "webhooks" && !canonicalUUID(segments[index]) {
			writeAPIError(writer, http.StatusBadRequest, "invalid resource id")
			return
		}
	}
	switch {
	case len(segments) == 2 && segments[1] == "pause":
		setListenerPaused(store, segments[0], true, writer, request)
	case len(segments) == 2 && segments[1] == "resume":
		setListenerPaused(store, segments[0], false, writer, request)
	case len(segments) == 1 && segments[0] == "webhooks":
		createGlobalWebhook(store, writer, request)
	case len(segments) == 2 && segments[1] == "webhooks":
		createChainWebhook(store, segments[0], writer, request)
	case len(segments) == 2 && segments[1] == "contracts":
		createContract(store, segments[0], writer, request)
	case len(segments) == 4 && segments[1] == "contracts" && segments[3] == "webhooks":
		createContractWebhook(store, segments[0], segments[2], writer, request)
	case len(segments) == 4 && segments[1] == "contracts" && segments[3] == "events":
		createEvent(store, segments[0], segments[2], writer, request)
	case len(segments) == 6 && segments[1] == "contracts" && segments[3] == "events" && segments[5] == "webhooks":
		createEventWebhook(store, segments[0], segments[2], segments[4], writer, request)
	default:
		writeAPIError(writer, http.StatusNotFound, "resource not found")
	}
}

func requireAdmin(writer http.ResponseWriter, request *http.Request) bool {
	principal, ok := request.Context().Value(apiKeyPrincipalContextKey{}).(core.APIKeyPrincipal)
	if !ok || principal.Role != core.APIKeyAdmin {
		writeAPIError(writer, http.StatusForbidden, "admin API key required")
		return false
	}
	return true
}

func mutationAudit(request *http.Request, action, resourceKind, resourceID, parentListenerID string) *core.RPCListenerAudit {
	principal := request.Context().Value(apiKeyPrincipalContextKey{}).(core.APIKeyPrincipal)
	return &core.RPCListenerAudit{
		ActorID: principal.ID, ActorName: principal.Name, ActorRole: principal.Role,
		Action: action, ResourceKind: resourceKind, ResourceID: resourceID,
		ParentListenerID: parentListenerID,
	}
}

func createRPCListener(store *sqlite.Store, writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input rpcListenerCreateRequest
	if !decodeCreateRequest(writer, request, &input) {
		return
	}
	config, err := buildRPCListener(input)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	snapshot, ok := loadExpectedSnapshot(store, expected, writer, request)
	if !ok {
		return
	}
	snapshot.Listeners = append(snapshot.Listeners, config)
	if err := validateRPCListenerSnapshot(snapshot); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	revision, err := store.CreateRPCListenerAudited(request.Context(), config, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionCreate, core.AuditResourceRPCListener, config.ID, ""))
	if !writeMutationError(writer, revision, err) {
		return
	}
	created, ok := loadCreatedListener(store, config.ID, writer, request)
	if !ok {
		return
	}
	writeCreated(writer, revision, "/api/v1/rpc-listeners/"+config.ID, map[string]any{
		"revision": revision, "rpcListener": rpcListenerResponse(*created, snapshot.GlobalWebhooks),
	})
}

func createContract(store *sqlite.Store, listenerID string, writer http.ResponseWriter, request *http.Request) {
	expected, snapshot, listener, ok := loadListenerMutation(store, listenerID, writer, request)
	if !ok {
		return
	}
	var input contractCreateRequest
	if !decodeCreateRequest(writer, request, &input) {
		return
	}
	contract, err := buildContract(input)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	listener.Contracts = append(listener.Contracts, contract)
	if err := validateRPCListenerSnapshot(snapshot); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	revision, err := store.ReplaceRPCListenerAudited(request.Context(), *listener, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionCreate, core.AuditResourceContract, contract.ID, listenerID))
	if !writeMutationError(writer, revision, err) {
		return
	}
	createdListener, ok := loadCreatedListener(store, listenerID, writer, request)
	if !ok {
		return
	}
	response := rpcListenerResponse(*createdListener, snapshot.GlobalWebhooks)
	created := findContractResponse(response.Contracts, contract.ID)
	writeCreated(writer, revision, fmt.Sprintf("/api/v1/rpc-listeners/%s/contracts/%s", listenerID, contract.ID), map[string]any{"revision": revision, "contract": created})
}

func createEvent(store *sqlite.Store, listenerID, contractID string, writer http.ResponseWriter, request *http.Request) {
	expected, snapshot, listener, ok := loadListenerMutation(store, listenerID, writer, request)
	if !ok {
		return
	}
	contract := findContract(listener, contractID)
	if contract == nil {
		writeAPIError(writer, http.StatusNotFound, "contract configuration not found")
		return
	}
	var input eventCreateRequest
	if !decodeCreateRequest(writer, request, &input) {
		return
	}
	event := buildEvent(input)
	contract.Events = append(contract.Events, event)
	if err := validateRPCListenerSnapshot(snapshot); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	revision, err := store.ReplaceRPCListenerAudited(request.Context(), *listener, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionCreate, core.AuditResourceEvent, event.ID, listenerID))
	if !writeMutationError(writer, revision, err) {
		return
	}
	createdListener, ok := loadCreatedListener(store, listenerID, writer, request)
	if !ok {
		return
	}
	response := rpcListenerResponse(*createdListener, snapshot.GlobalWebhooks)
	createdContract := findContractResponse(response.Contracts, contractID)
	created := findEventResponse(createdContract.Events, event.ID)
	writeCreated(writer, revision, fmt.Sprintf("/api/v1/rpc-listeners/%s/contracts/%s/events/%s", listenerID, contractID, event.ID), map[string]any{"revision": revision, "event": created})
}

func createGlobalWebhook(store *sqlite.Store, writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input webhookCreateRequest
	if !decodeCreateRequest(writer, request, &input) {
		return
	}
	snapshot, ok := loadExpectedSnapshot(store, expected, writer, request)
	if !ok {
		return
	}
	created := buildWebhook(input)
	snapshot.GlobalWebhooks = append(snapshot.GlobalWebhooks, created)
	if err := validateRPCListenerSnapshot(snapshot); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	revision, err := store.ReplaceGlobalWebhooksAudited(request.Context(), snapshot.GlobalWebhooks, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionCreate, core.AuditResourceWebhook, created.ID, ""))
	if !writeMutationError(writer, revision, err) {
		return
	}
	latest, err := store.RPCListenerSnapshot(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "configuration was created but could not be loaded")
		return
	}
	webhook := findWebhook(latest.GlobalWebhooks, created.ID)
	writeCreated(writer, revision, "/api/v1/rpc-listeners/webhooks/"+created.ID, map[string]any{"revision": revision, "webhook": webhookResponses([]core.WebhookConfig{*webhook})[0]})
}

func createChainWebhook(store *sqlite.Store, listenerID string, writer http.ResponseWriter, request *http.Request) {
	createNestedWebhook(store, listenerID, "", "", "chain", writer, request)
}

func createContractWebhook(store *sqlite.Store, listenerID, contractID string, writer http.ResponseWriter, request *http.Request) {
	createNestedWebhook(store, listenerID, contractID, "", "contract", writer, request)
}

func createEventWebhook(store *sqlite.Store, listenerID, contractID, eventID string, writer http.ResponseWriter, request *http.Request) {
	createNestedWebhook(store, listenerID, contractID, eventID, "event", writer, request)
}

func createNestedWebhook(store *sqlite.Store, listenerID, contractID, eventID, level string, writer http.ResponseWriter, request *http.Request) {
	expected, snapshot, listener, ok := loadListenerMutation(store, listenerID, writer, request)
	if !ok {
		return
	}
	var target *[]core.WebhookConfig
	location := "/api/v1/rpc-listeners/" + listenerID + "/webhooks/"
	switch level {
	case "chain":
		target = &listener.Webhooks
	case "contract":
		contract := findContract(listener, contractID)
		if contract == nil {
			writeAPIError(writer, http.StatusNotFound, "contract configuration not found")
			return
		}
		target = &contract.Webhooks
		location = fmt.Sprintf("/api/v1/rpc-listeners/%s/contracts/%s/webhooks/", listenerID, contractID)
	case "event":
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
		target = &event.Webhooks
		location = fmt.Sprintf("/api/v1/rpc-listeners/%s/contracts/%s/events/%s/webhooks/", listenerID, contractID, eventID)
	}
	var input webhookCreateRequest
	if !decodeCreateRequest(writer, request, &input) {
		return
	}
	created := buildWebhook(input)
	*target = append(*target, created)
	if err := validateRPCListenerSnapshot(snapshot); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	revision, err := store.ReplaceRPCListenerAudited(request.Context(), *listener, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionCreate, core.AuditResourceWebhook, created.ID, listenerID))
	if !writeMutationError(writer, revision, err) {
		return
	}
	createdListener, ok := loadCreatedListener(store, listenerID, writer, request)
	if !ok {
		return
	}
	webhook := findNestedWebhook(createdListener, contractID, eventID, created.ID)
	writeCreated(writer, revision, location+created.ID, map[string]any{"revision": revision, "webhook": webhookResponses([]core.WebhookConfig{*webhook})[0]})
}

func requireRevision(writer http.ResponseWriter, request *http.Request) (uint64, bool) {
	value := request.Header.Get("If-Match")
	if value == "" {
		writeAPIError(writer, http.StatusPreconditionRequired, "If-Match revision is required")
		return 0, false
	}
	if !strings.HasPrefix(value, `"revision-`) || !strings.HasSuffix(value, `"`) {
		writeAPIError(writer, http.StatusBadRequest, "If-Match must be a revision ETag")
		return 0, false
	}
	number := strings.TrimSuffix(strings.TrimPrefix(value, `"revision-`), `"`)
	revision, err := strconv.ParseUint(number, 10, 64)
	if err != nil || strconv.FormatUint(revision, 10) != number || revision > math.MaxInt64 {
		writeAPIError(writer, http.StatusBadRequest, "If-Match must be a revision ETag")
		return 0, false
	}
	return revision, true
}

func loadExpectedSnapshot(store *sqlite.Store, expected uint64, writer http.ResponseWriter, request *http.Request) (core.RPCListenerSnapshot, bool) {
	snapshot, err := store.RPCListenerSnapshot(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
		return core.RPCListenerSnapshot{}, false
	}
	if snapshot.Revision != expected {
		writer.Header().Set("ETag", revisionETag(snapshot.Revision))
		writeAPIError(writer, http.StatusPreconditionFailed, "configuration revision does not match")
		return core.RPCListenerSnapshot{}, false
	}
	return snapshot, true
}

func loadListenerMutation(store *sqlite.Store, listenerID string, writer http.ResponseWriter, request *http.Request) (uint64, core.RPCListenerSnapshot, *core.RPCListener, bool) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return 0, core.RPCListenerSnapshot{}, nil, false
	}
	snapshot, ok := loadExpectedSnapshot(store, expected, writer, request)
	if !ok {
		return 0, core.RPCListenerSnapshot{}, nil, false
	}
	listener, _ := findListener(snapshot, listenerID)
	if listener == nil {
		writeAPIError(writer, http.StatusNotFound, "RPC listener not found")
		return 0, core.RPCListenerSnapshot{}, nil, false
	}
	return expected, snapshot, listener, true
}

func decodeCreateRequest(writer http.ResponseWriter, request *http.Request, destination any) bool {
	mediaType := strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0])
	if mediaType != "application/json" {
		writeAPIError(writer, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, rpcListenerBodyLimit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "request body must be one valid JSON object with known fields")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(writer, http.StatusBadRequest, "request body must contain one JSON object")
		return false
	}
	return true
}

func writeMutationError(writer http.ResponseWriter, revision uint64, err error) bool {
	if err == nil {
		return true
	}
	switch {
	case errors.Is(err, sqlite.ErrRevisionConflict):
		writer.Header().Set("ETag", revisionETag(revision))
		writeAPIError(writer, http.StatusPreconditionFailed, "configuration revision does not match")
	case errors.Is(err, sqlite.ErrNotFound):
		writeAPIError(writer, http.StatusNotFound, "configuration resource not found")
	case errors.Is(err, sqlite.ErrRPCCredentialEncryptionKeyRequired):
		writeAPIError(writer, http.StatusUnprocessableEntity, "RPC credentials require security.rpc_credentials_key_ref")
	default:
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
	}
	return false
}

func writeCreated(writer http.ResponseWriter, revision uint64, location string, body any) {
	writer.Header().Set("ETag", revisionETag(revision))
	writer.Header().Set("Location", location)
	writeJSON(writer, http.StatusCreated, body)
}

func buildRPCListener(input rpcListenerCreateRequest) (core.RPCListener, error) {
	pollInterval, err := positiveDuration("pollInterval", input.PollInterval)
	if err != nil {
		return core.RPCListener{}, err
	}
	retryBackoff, err := positiveDuration("rpcRetryBackoff", input.RPCRetryBackoff)
	if err != nil {
		return core.RPCListener{}, err
	}
	rpcTimeout, err := positiveDuration("rpcTimeout", input.RPCTimeout)
	if err != nil {
		return core.RPCListener{}, err
	}
	config := core.RPCListener{
		ID: core.NewConfigID(), Name: input.Name, Paused: input.Paused, ChainID: input.ChainID, RPCURL: input.RPCURL, RPCURLRef: input.RPCURLRef,
		RPCAuthentication: core.RPCAuthentication{Type: input.RPCAuthentication.Type, Username: input.RPCAuthentication.Username, HeaderName: input.RPCAuthentication.HeaderName, Secret: input.RPCAuthentication.Secret, TokenURL: input.RPCAuthentication.TokenURL, TokenAPIKey: input.RPCAuthentication.TokenAPIKey},
		StartBlock:        input.StartBlock, BatchSize: input.BatchSize, PollInterval: pollInterval,
		Confirmations: input.Confirmations, ReorgDepth: input.ReorgDepth, RPCRetryAttempts: input.RPCRetryAttempts,
		RPCRetryBackoff: retryBackoff, RPCTimeout: rpcTimeout,
		TLS: core.ListenerTLSConfig{CAPEM: input.TLS.CAPEM, ServerName: input.TLS.ServerName, InsecureSkipVerify: input.TLS.InsecureSkipVerify},
	}
	config.Webhooks = buildWebhooks(input.Webhooks)
	for _, contractInput := range input.Contracts {
		contract, err := buildContract(contractInput)
		if err != nil {
			return core.RPCListener{}, err
		}
		config.Contracts = append(config.Contracts, contract)
	}
	return config, nil
}

func buildContract(input contractCreateRequest) (core.ContractConfig, error) {
	if !common.IsHexAddress(input.Address) {
		return core.ContractConfig{}, errors.New("address must be a canonical EVM address")
	}
	checksummed := common.HexToAddress(input.Address).Hex()
	if input.Address != checksummed && input.Address != strings.ToLower(checksummed) {
		return core.ContractConfig{}, errors.New("address must be lowercase or EIP-55 checksummed")
	}
	contract := core.ContractConfig{ID: core.NewConfigID(), Address: checksummed, ABI: append(json.RawMessage(nil), input.ABI...)}
	contract.Webhooks = buildWebhooks(input.Webhooks)
	for _, eventInput := range input.Events {
		contract.Events = append(contract.Events, buildEvent(eventInput))
	}
	return contract, nil
}

func buildEvent(input eventCreateRequest) core.EventConfig {
	return core.EventConfig{ID: core.NewConfigID(), Selector: input.Selector, Webhooks: buildWebhooks(input.Webhooks)}
}

func buildWebhook(input webhookCreateRequest) core.WebhookConfig {
	return core.WebhookConfig{ID: core.NewConfigID(), URL: input.URL, URLRef: input.URLRef, Authentication: core.WebhookAuthentication{Type: input.Authentication.Type, SecretRef: input.Authentication.SecretRef, KeyID: input.Authentication.KeyID}}
}

func buildWebhooks(inputs []webhookCreateRequest) []core.WebhookConfig {
	result := make([]core.WebhookConfig, 0, len(inputs))
	for _, input := range inputs {
		result = append(result, buildWebhook(input))
	}
	return result
}

func positiveDuration(field, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", field)
	}
	return duration, nil
}

func validateRPCListenerSnapshot(snapshot core.RPCListenerSnapshot) error {
	if err := validateConfiguredWebhooks("global", snapshot.GlobalWebhooks); err != nil {
		return err
	}
	chainIDs := make(map[uint64]struct{}, len(snapshot.Listeners))
	for listenerIndex := range snapshot.Listeners {
		listener := &snapshot.Listeners[listenerIndex]
		if strings.TrimSpace(listener.Name) == "" {
			return errors.New("name is required")
		}
		if listener.ChainID == 0 || listener.ChainID > math.MaxInt64 {
			return errors.New("chainId must be between 1 and 9223372036854775807")
		}
		if _, exists := chainIDs[listener.ChainID]; exists {
			return errors.New("chainId must be unique")
		}
		chainIDs[listener.ChainID] = struct{}{}
		if listener.StartBlock > math.MaxInt64 || listener.BatchSize == 0 || listener.BatchSize > math.MaxInt64 || listener.Confirmations > math.MaxInt64 || listener.ReorgDepth == 0 || listener.ReorgDepth > math.MaxInt64 || listener.RPCRetryAttempts <= 0 {
			return errors.New("scanner numeric settings are outside supported ranges")
		}
		if listener.PollInterval <= 0 || listener.RPCRetryBackoff <= 0 || listener.RPCTimeout <= 0 {
			return errors.New("scanner durations must be positive")
		}
		if (listener.RPCURL == "") == (listener.RPCURLRef == "") {
			return errors.New("exactly one of rpcUrl and rpcUrlRef is required")
		}
		if listener.RPCURL != "" && !validAbsoluteHTTPURL(listener.RPCURL) {
			return errors.New("rpcUrl must be an absolute HTTP(S) URL")
		}
		if listener.RPCURLRef != "" {
			if err := secrets.ValidateReference(listener.RPCURLRef); err != nil {
				return fmt.Errorf("rpcUrlRef: %w", err)
			}
		}
		if err := validateConfiguredWebhooks("chain", listener.Webhooks); err != nil {
			return err
		}
		addresses := make(map[common.Address]struct{}, len(listener.Contracts))
		for contractIndex := range listener.Contracts {
			contract := &listener.Contracts[contractIndex]
			if !common.IsHexAddress(contract.Address) {
				return errors.New("contract address must be a canonical EVM address")
			}
			address := common.HexToAddress(contract.Address)
			if _, exists := addresses[address]; exists {
				return errors.New("contract address must be unique within an RPC listener")
			}
			addresses[address] = struct{}{}
			parsedABI, err := abi.JSON(strings.NewReader(string(contract.ABI)))
			if err != nil {
				return errors.New("abi must be a valid Ethereum ABI JSON array")
			}
			if err := validateConfiguredWebhooks("contract", contract.Webhooks); err != nil {
				return err
			}
			selectors := make(map[string]struct{}, len(contract.Events))
			for eventIndex := range contract.Events {
				event := &contract.Events[eventIndex]
				if _, exists := selectors[event.Selector]; exists {
					return errors.New("an ABI event may only be configured once per contract")
				}
				selectors[event.Selector] = struct{}{}
				matches := 0
				anonymous := false
				for _, abiEvent := range parsedABI.Events {
					if abiEvent.Sig == event.Selector {
						matches++
						anonymous = anonymous || abiEvent.Anonymous
					}
				}
				if matches != 1 {
					return errors.New("selector must identify exactly one event in the contract ABI")
				}
				if anonymous {
					return errors.New("anonymous ABI events are not supported")
				}
				if err := validateConfiguredWebhooks("event", event.Webhooks); err != nil {
					return err
				}
				if len(event.Webhooks) == 0 && len(contract.Webhooks) == 0 && len(listener.Webhooks) == 0 && len(snapshot.GlobalWebhooks) == 0 {
					return errors.New("every configured event must have an effective webhook route")
				}
			}
		}
	}
	return nil
}

func validateConfiguredWebhooks(level string, webhooks []core.WebhookConfig) error {
	seen := make(map[string]struct{}, len(webhooks))
	for _, webhook := range webhooks {
		if (webhook.URL == "") == (webhook.URLRef == "") {
			return fmt.Errorf("%s webhook requires exactly one of url and urlRef", level)
		}
		if webhook.URL != "" && !validAbsoluteHTTPURL(webhook.URL) {
			return fmt.Errorf("%s webhook URL must be an absolute HTTP(S) URL", level)
		}
		if webhook.URLRef != "" {
			if err := secrets.ValidateReference(webhook.URLRef); err != nil {
				return fmt.Errorf("%s webhook urlRef: %w", level, err)
			}
		}
		if err := validateWebhookAuthentication(webhook.Authentication); err != nil {
			return fmt.Errorf("%s webhook authentication: %w", level, err)
		}
		locator := webhook.URL
		if locator == "" {
			locator = webhook.URLRef
		}
		if _, exists := seen[locator]; exists {
			return fmt.Errorf("%s webhook URLs must be unique", level)
		}
		seen[locator] = struct{}{}
	}
	return nil
}

var webhookKeyID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func validateWebhookAuthentication(authentication core.WebhookAuthentication) error {
	if authentication.Type == "" {
		if authentication.SecretRef != "" || authentication.KeyID != "" {
			return errors.New("type is required when authentication fields are set")
		}
		return nil
	}
	if authentication.Type != "hmac-sha256" {
		return errors.New("type must be hmac-sha256")
	}
	if err := secrets.ValidateReference(authentication.SecretRef); err != nil {
		return fmt.Errorf("secretRef: %w", err)
	}
	if authentication.KeyID != "" && !webhookKeyID.MatchString(authentication.KeyID) {
		return errors.New("keyId must be 1-128 letters, digits, dots, underscores, or hyphens")
	}
	return nil
}

func validAbsoluteHTTPURL(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Fragment == ""
}

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

func findListener(snapshot core.RPCListenerSnapshot, id string) (*core.RPCListener, int) {
	for index := range snapshot.Listeners {
		if snapshot.Listeners[index].ID == id {
			return &snapshot.Listeners[index], index
		}
	}
	return nil, -1
}

func findContract(listener *core.RPCListener, id string) *core.ContractConfig {
	for index := range listener.Contracts {
		if listener.Contracts[index].ID == id {
			return &listener.Contracts[index]
		}
	}
	return nil
}

func findEvent(contract *core.ContractConfig, id string) *core.EventConfig {
	for index := range contract.Events {
		if contract.Events[index].ID == id {
			return &contract.Events[index]
		}
	}
	return nil
}

func findWebhook(webhooks []core.WebhookConfig, id string) *core.WebhookConfig {
	for index := range webhooks {
		if webhooks[index].ID == id {
			return &webhooks[index]
		}
	}
	return nil
}

func findNestedWebhook(listener *core.RPCListener, contractID, eventID, webhookID string) *core.WebhookConfig {
	if contractID == "" {
		return findWebhook(listener.Webhooks, webhookID)
	}
	contract := findContract(listener, contractID)
	if eventID == "" {
		return findWebhook(contract.Webhooks, webhookID)
	}
	return findWebhook(findEvent(contract, eventID).Webhooks, webhookID)
}

func loadCreatedListener(store *sqlite.Store, id string, writer http.ResponseWriter, request *http.Request) (*core.RPCListener, bool) {
	config, _, err := store.RPCListener(request.Context(), id)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "configuration was created but could not be loaded")
		return nil, false
	}
	return &config, true
}

func findContractResponse(contracts []contractAPIResponse, id string) contractAPIResponse {
	for _, contract := range contracts {
		if contract.ID == id {
			return contract
		}
	}
	return contractAPIResponse{}
}

func findEventResponse(events []eventAPIResponse, id string) eventAPIResponse {
	for _, event := range events {
		if event.ID == id {
			return event
		}
	}
	return eventAPIResponse{}
}
