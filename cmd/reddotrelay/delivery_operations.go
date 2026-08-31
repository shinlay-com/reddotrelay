package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/secrets"
	"reddotrelay/internal/store/sqlite"

	"github.com/ethereum/go-ethereum/common"
)

const defaultDiagnosticsPageSize = 50
const maxDiagnosticsAPIPageSize = 200

type eventCursor struct {
	ObservedAt      time.Time `json:"observedAt"`
	ChainID         uint64    `json:"chainId"`
	TransactionHash string    `json:"transactionHash"`
	LogIndex        uint64    `json:"logIndex"`
}

type eventHistoryAPIResponse struct {
	ID              string          `json:"id"`
	ChainID         uint64          `json:"chainId"`
	TransactionHash string          `json:"transactionHash"`
	LogIndex        uint64          `json:"logIndex"`
	BlockNumber     uint64          `json:"blockNumber"`
	BlockHash       string          `json:"blockHash"`
	ContractAddress string          `json:"contractAddress"`
	EventName       string          `json:"eventName"`
	EventSignature  string          `json:"eventSignature"`
	DecodedData     json.RawMessage `json:"decodedData"`
	ObservedAt      time.Time       `json:"observedAt"`
	Deliveries      map[string]int  `json:"deliveries"`
}

type deliveryAPIResponse struct {
	ID             string              `json:"id"`
	Destination    string              `json:"destination"`
	Status         core.DeliveryStatus `json:"status"`
	Attempts       int                 `json:"attempts"`
	TotalAttempts  int                 `json:"totalAttempts"`
	NextAttempt    time.Time           `json:"nextAttempt"`
	LastAttemptAt  *time.Time          `json:"lastAttemptAt,omitempty"`
	LastStatusCode int                 `json:"lastStatusCode,omitempty"`
	FailureSummary string              `json:"failureSummary,omitempty"`
	DeliveredAt    *time.Time          `json:"deliveredAt,omitempty"`
	Authentication string              `json:"authentication,omitempty"`
	KeyID          string              `json:"keyId,omitempty"`
}

func handleEventHistory(store *sqlite.Store, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	query := request.URL.Query()
	for key := range query {
		switch key {
		case "limit", "before", "chainId", "transactionHash", "blockNumber", "contractAddress", "eventSignature", "deliveryStatus":
		default:
			writeAPIError(writer, http.StatusBadRequest, "unknown query parameter")
			return
		}
	}
	limit, ok := diagnosticsLimit(writer, query.Get("limit"))
	if !ok {
		return
	}
	filter := core.EventHistoryFilter{TransactionHash: query.Get("transactionHash"), Address: query.Get("contractAddress"), Signature: query.Get("eventSignature"), DeliveryStatus: core.DeliveryStatus(query.Get("deliveryStatus"))}
	if raw := query.Get("chainId"); raw != "" {
		value, valid := diagnosticUint(writer, "chainId", raw)
		if !valid || value == 0 {
			if valid {
				writeAPIError(writer, http.StatusBadRequest, "chainId must be greater than zero")
			}
			return
		}
		filter.ChainID = &value
	}
	if raw := query.Get("blockNumber"); raw != "" {
		value, valid := diagnosticUint(writer, "blockNumber", raw)
		if !valid {
			return
		}
		filter.BlockNumber = &value
	}
	if filter.TransactionHash != "" && !regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`).MatchString(filter.TransactionHash) {
		writeAPIError(writer, http.StatusBadRequest, "transactionHash must be a 32-byte hexadecimal value")
		return
	}
	if filter.Address != "" && !common.IsHexAddress(filter.Address) {
		writeAPIError(writer, http.StatusBadRequest, "contractAddress must be an EVM address")
		return
	}
	if filter.DeliveryStatus != "" && filter.DeliveryStatus != core.DeliveryPending && filter.DeliveryStatus != core.DeliveryDelivered && filter.DeliveryStatus != core.DeliveryDead {
		writeAPIError(writer, http.StatusBadRequest, "deliveryStatus must be pending, delivered, or dead")
		return
	}
	if raw := query.Get("before"); raw != "" {
		cursor, err := decodeEventCursor(raw)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, "before must be a valid event cursor")
			return
		}
		filter.Before = &core.EventHistoryCursor{ObservedAt: cursor.ObservedAt, ChainID: cursor.ChainID, TransactionHash: cursor.TransactionHash, LogIndex: cursor.LogIndex}
	}
	entries, err := store.EventHistory(request.Context(), filter, limit+1)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
		return
	}
	deliverySummary, err := store.DeliveryStatusSummary(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
		return
	}
	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}
	responses := make([]eventHistoryAPIResponse, len(entries))
	for i, entry := range entries {
		decoded := json.RawMessage(entry.Event.DecodedPayload)
		if !json.Valid(decoded) {
			decoded = json.RawMessage(`{}`)
		}
		responses[i] = eventHistoryAPIResponse{ID: entry.EventGUID, ChainID: entry.Event.ID.ChainID, TransactionHash: entry.Event.ID.TransactionHash, LogIndex: entry.Event.ID.LogIndex, BlockNumber: entry.Event.BlockNumber, BlockHash: entry.Event.BlockHash, ContractAddress: entry.Event.Address, EventName: entry.Event.Name, EventSignature: entry.Event.Signature, DecodedData: decoded, ObservedAt: entry.Event.ObservedAt, Deliveries: map[string]int{"pending": entry.Pending, "delivered": entry.Delivered, "dead": entry.Dead}}
	}
	response := map[string]any{"entries": responses, "deliverySummary": map[string]int{
		"pending": deliverySummary[core.DeliveryPending], "delivered": deliverySummary[core.DeliveryDelivered], "dead": deliverySummary[core.DeliveryDead],
	}}
	if hasMore && len(entries) > 0 {
		last := entries[len(entries)-1]
		response["nextBefore"] = encodeEventCursor(eventCursor{ObservedAt: last.Event.ObservedAt, ChainID: last.Event.ID.ChainID, TransactionHash: last.Event.ID.TransactionHash, LogIndex: last.Event.ID.LogIndex})
	}
	writeJSON(writer, http.StatusOK, response)
}

func handleEventDeliveries(store *sqlite.Store, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	segments := strings.Split(strings.Trim(strings.TrimPrefix(request.URL.Path, "/api/v1/events/"), "/"), "/")
	if len(segments) != 2 || segments[1] != "deliveries" || !canonicalUUID(segments[0]) {
		writeAPIError(writer, http.StatusNotFound, "resource not found")
		return
	}
	query := request.URL.Query()
	for key := range query {
		if key != "limit" && key != "after" {
			writeAPIError(writer, http.StatusBadRequest, "unknown query parameter")
			return
		}
	}
	limit, ok := diagnosticsLimit(writer, query.Get("limit"))
	if !ok {
		return
	}
	after := query.Get("after")
	if after != "" && !canonicalUUID(after) {
		writeAPIError(writer, http.StatusBadRequest, "after must be a valid delivery cursor")
		return
	}
	deliveries, err := store.EventDeliveries(request.Context(), segments[0], after, limit+1)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
		return
	}
	hasMore := len(deliveries) > limit
	if hasMore {
		deliveries = deliveries[:limit]
	}
	responses := make([]deliveryAPIResponse, len(deliveries))
	for i, delivery := range deliveries {
		responses[i] = deliveryResponse(delivery)
	}
	response := map[string]any{"entries": responses}
	if hasMore && len(deliveries) > 0 {
		response["nextAfter"] = deliveries[len(deliveries)-1].ID
	}
	writeJSON(writer, http.StatusOK, response)
}

func handleDeliveryRequeue(store *sqlite.Store, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireAdmin(writer, request) {
		return
	}
	segments := strings.Split(strings.Trim(strings.TrimPrefix(request.URL.Path, "/api/v1/deliveries/"), "/"), "/")
	if len(segments) != 2 || segments[1] != "requeue" || !canonicalUUID(segments[0]) {
		writeAPIError(writer, http.StatusNotFound, "resource not found")
		return
	}
	var input struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeCreateRequest(writer, request, &input) {
		return
	}
	if !input.Confirm {
		writeAPIError(writer, http.StatusUnprocessableEntity, "confirm must be true")
		return
	}
	principal := request.Context().Value(apiKeyPrincipalContextKey{}).(core.APIKeyPrincipal)
	entry, err := store.RequeueDeadDeliveryAudited(request.Context(), segments[0], core.DeliveryRequeueAudit{ActorID: principal.ID, ActorName: principal.Name, ActorRole: principal.Role}, time.Now().UTC())
	if errors.Is(err, sqlite.ErrNotFound) {
		writeAPIError(writer, http.StatusNotFound, "dead delivery not found")
		return
	}
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"deliveryId": entry.DeliveryID, "eventId": entry.EventID, "status": core.DeliveryPending, "auditId": entry.ID})
}

func handleDeliveryRequeueAudit(store *sqlite.Store, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireAdmin(writer, request) {
		return
	}
	query := request.URL.Query()
	for key := range query {
		if key != "limit" && key != "before" {
			writeAPIError(writer, http.StatusBadRequest, "unknown query parameter")
			return
		}
	}
	limit, ok := diagnosticsLimit(writer, query.Get("limit"))
	if !ok {
		return
	}
	var before uint64
	if raw := query.Get("before"); raw != "" {
		value, valid := diagnosticUint(writer, "before", raw)
		if !valid || value == 0 {
			if valid {
				writeAPIError(writer, http.StatusBadRequest, "before must be greater than zero")
			}
			return
		}
		before = value
	}
	entries, err := store.DeliveryRequeueAudit(request.Context(), limit+1, before)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
		return
	}
	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}
	type responseEntry struct {
		ID               string          `json:"id"`
		ActorID          string          `json:"actorId"`
		ActorName        string          `json:"actorName"`
		ActorRole        core.APIKeyRole `json:"actorRole"`
		DeliveryID       string          `json:"deliveryId"`
		EventID          string          `json:"eventId"`
		PreviousAttempts int             `json:"previousAttempts"`
		CreatedAt        time.Time       `json:"createdAt"`
	}
	responses := make([]responseEntry, len(entries))
	for i, entry := range entries {
		responses[i] = responseEntry{ID: entry.ID, ActorID: entry.ActorID, ActorName: entry.ActorName, ActorRole: entry.ActorRole, DeliveryID: entry.DeliveryID, EventID: entry.EventID, PreviousAttempts: entry.PreviousAttempts, CreatedAt: entry.CreatedAt}
	}
	response := map[string]any{"entries": responses}
	if hasMore && len(entries) > 0 {
		response["nextBefore"] = strconv.FormatUint(entries[len(entries)-1].Sequence, 10)
	}
	writeJSON(writer, http.StatusOK, response)
}

func deliveryResponse(delivery core.Delivery) deliveryAPIResponse {
	destination := delivery.Destination
	if !secrets.IsReference(destination) {
		destination = redactOptionalURL(destination)
	}
	response := deliveryAPIResponse{ID: delivery.ID, Destination: destination, Status: delivery.Status, Attempts: delivery.Attempts, TotalAttempts: delivery.TotalAttempts, NextAttempt: delivery.NextAttempt, LastAttemptAt: delivery.LastAttemptAt, LastStatusCode: delivery.LastStatusCode, FailureSummary: safeDeliveryFailure(delivery.LastError), DeliveredAt: delivery.DeliveredAt}
	if delivery.Authentication.Type == "hmac-sha256" {
		response.Authentication = "hmac-sha256"
		response.KeyID = delivery.Authentication.KeyID
	}
	return response
}

func safeDeliveryFailure(value string) string {
	if value == "" {
		return ""
	}
	if value == "webhook request timed out" || value == "webhook request failed" || regexp.MustCompile(`^webhook returned HTTP [1-5][0-9][0-9]$`).MatchString(value) {
		return value
	}
	return "delivery attempt failed"
}
func diagnosticsLimit(writer http.ResponseWriter, raw string) (int, bool) {
	if raw == "" {
		return defaultDiagnosticsPageSize, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > maxDiagnosticsAPIPageSize || strconv.Itoa(value) != raw {
		writeAPIError(writer, http.StatusBadRequest, "limit must be an integer between 1 and 200")
		return 0, false
	}
	return value, true
}
func diagnosticUint(writer http.ResponseWriter, name, raw string) (uint64, bool) {
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || strconv.FormatUint(value, 10) != raw {
		writeAPIError(writer, http.StatusBadRequest, fmt.Sprintf("%s must be an unsigned integer", name))
		return 0, false
	}
	return value, true
}
func encodeEventCursor(cursor eventCursor) string {
	value, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(value)
}
func decodeEventCursor(value string) (eventCursor, error) {
	var cursor eventCursor
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.ObservedAt.IsZero() || cursor.ChainID == 0 || cursor.TransactionHash == "" {
		return cursor, errors.New("invalid cursor")
	}
	return cursor, nil
}
