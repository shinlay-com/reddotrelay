package main

import (
	"net/http"
	"strconv"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
)

const (
	defaultAuditPageSize = 50
	maxAuditPageSize     = 200
)

type auditAPIResponse struct {
	ID               string          `json:"id"`
	ActorID          string          `json:"actorId"`
	ActorName        string          `json:"actorName"`
	ActorRole        core.APIKeyRole `json:"actorRole"`
	Action           string          `json:"action"`
	ResourceKind     string          `json:"resourceKind"`
	ResourceID       string          `json:"resourceId"`
	ParentListenerID string          `json:"parentListenerId,omitempty"`
	PreviousRevision uint64          `json:"previousRevision"`
	NewRevision      uint64          `json:"newRevision"`
	CreatedAt        time.Time       `json:"createdAt"`
}

func handleRPCListenerAudit(store *sqlite.Store, writer http.ResponseWriter, request *http.Request) {
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
	limit := defaultAuditPageSize
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxAuditPageSize || strconv.Itoa(parsed) != raw {
			writeAPIError(writer, http.StatusBadRequest, "limit must be an integer between 1 and 200")
			return
		}
		limit = parsed
	}
	var before uint64
	if raw := query.Get("before"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != raw {
			writeAPIError(writer, http.StatusBadRequest, "before must be a valid audit cursor")
			return
		}
		before = parsed
	}
	entries, err := store.RPCListenerAudit(request.Context(), limit+1, before)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
		return
	}
	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}
	responses := make([]auditAPIResponse, len(entries))
	for i, entry := range entries {
		responses[i] = auditAPIResponse{
			ID: entry.ID, ActorID: entry.ActorID, ActorName: entry.ActorName, ActorRole: entry.ActorRole,
			Action: entry.Action, ResourceKind: entry.ResourceKind, ResourceID: entry.ResourceID,
			ParentListenerID: entry.ParentListenerID, PreviousRevision: entry.PreviousRevision,
			NewRevision: entry.NewRevision, CreatedAt: entry.CreatedAt,
		}
	}
	response := map[string]any{"entries": responses}
	if hasMore && len(entries) != 0 {
		response["nextBefore"] = strconv.FormatUint(entries[len(entries)-1].Sequence, 10)
	}
	writeJSON(writer, http.StatusOK, response)
}

func handleRPCListenerStatus(manager *runtimeManager, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if manager == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "runtime status is unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, manager.Status())
}
