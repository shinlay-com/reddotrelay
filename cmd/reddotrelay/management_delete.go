package main

import (
	"net/http"
	"strconv"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
)

func handleRPCListenerDeleteResource(store *sqlite.Store, segments []string, writer http.ResponseWriter, request *http.Request) {
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
		deleteRPCListener(store, segments[0], writer, request)
	case len(segments) == 2 && segments[0] == "webhooks":
		deleteGlobalWebhook(store, segments[1], writer, request)
	case len(segments) == 3 && segments[1] == "webhooks":
		deleteNestedWebhook(store, segments[0], "", "", segments[2], writer, request)
	case len(segments) == 3 && segments[1] == "contracts":
		deleteContract(store, segments[0], segments[2], writer, request)
	case len(segments) == 5 && segments[1] == "contracts" && segments[3] == "webhooks":
		deleteNestedWebhook(store, segments[0], segments[2], "", segments[4], writer, request)
	case len(segments) == 5 && segments[1] == "contracts" && segments[3] == "events":
		deleteEvent(store, segments[0], segments[2], segments[4], writer, request)
	case len(segments) == 7 && segments[1] == "contracts" && segments[3] == "events" && segments[5] == "webhooks":
		deleteNestedWebhook(store, segments[0], segments[2], segments[4], segments[6], writer, request)
	default:
		writeAPIError(writer, http.StatusNotFound, "resource not found")
	}
}

func deleteRPCListener(store *sqlite.Store, listenerID string, writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	snapshot, ok := loadExpectedSnapshot(store, expected, writer, request)
	if !ok {
		return
	}
	_, index := findListener(snapshot, listenerID)
	if index < 0 {
		writeAPIError(writer, http.StatusNotFound, "RPC listener not found")
		return
	}
	snapshot.Listeners = append(snapshot.Listeners[:index], snapshot.Listeners[index+1:]...)
	if !validateDeletion(snapshot, writer) {
		return
	}
	revision, err := store.DeleteRPCListenerAudited(request.Context(), listenerID, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionDelete, core.AuditResourceRPCListener, listenerID, ""))
	if !writeMutationError(writer, revision, err) {
		return
	}
	writeDeleted(writer, revision)
}

func deleteContract(store *sqlite.Store, listenerID, contractID string, writer http.ResponseWriter, request *http.Request) {
	expected, snapshot, listener, ok := loadListenerMutation(store, listenerID, writer, request)
	if !ok {
		return
	}
	index := contractIndex(listener.Contracts, contractID)
	if index < 0 {
		writeAPIError(writer, http.StatusNotFound, "contract configuration not found")
		return
	}
	listener.Contracts = append(listener.Contracts[:index], listener.Contracts[index+1:]...)
	if !validateDeletion(snapshot, writer) {
		return
	}
	revision, err := store.ReplaceRPCListenerAudited(request.Context(), *listener, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionDelete, core.AuditResourceContract, contractID, listenerID))
	if !writeMutationError(writer, revision, err) {
		return
	}
	writeDeleted(writer, revision)
}

func deleteEvent(store *sqlite.Store, listenerID, contractID, eventID string, writer http.ResponseWriter, request *http.Request) {
	expected, snapshot, listener, ok := loadListenerMutation(store, listenerID, writer, request)
	if !ok {
		return
	}
	contract := findContract(listener, contractID)
	if contract == nil {
		writeAPIError(writer, http.StatusNotFound, "contract configuration not found")
		return
	}
	index := eventIndex(contract.Events, eventID)
	if index < 0 {
		writeAPIError(writer, http.StatusNotFound, "event configuration not found")
		return
	}
	contract.Events = append(contract.Events[:index], contract.Events[index+1:]...)
	if !validateDeletion(snapshot, writer) {
		return
	}
	revision, err := store.ReplaceRPCListenerAudited(request.Context(), *listener, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionDelete, core.AuditResourceEvent, eventID, listenerID))
	if !writeMutationError(writer, revision, err) {
		return
	}
	writeDeleted(writer, revision)
}

func deleteGlobalWebhook(store *sqlite.Store, webhookID string, writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	snapshot, ok := loadExpectedSnapshot(store, expected, writer, request)
	if !ok {
		return
	}
	index := webhookIndex(snapshot.GlobalWebhooks, webhookID)
	if index < 0 {
		writeAPIError(writer, http.StatusNotFound, "webhook configuration not found")
		return
	}
	snapshot.GlobalWebhooks = append(snapshot.GlobalWebhooks[:index], snapshot.GlobalWebhooks[index+1:]...)
	if !validateDeletion(snapshot, writer) {
		return
	}
	revision, err := store.ReplaceGlobalWebhooksAudited(request.Context(), snapshot.GlobalWebhooks, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionDelete, core.AuditResourceWebhook, webhookID, ""))
	if !writeMutationError(writer, revision, err) {
		return
	}
	writeDeleted(writer, revision)
}

func deleteNestedWebhook(store *sqlite.Store, listenerID, contractID, eventID, webhookID string, writer http.ResponseWriter, request *http.Request) {
	expected, snapshot, listener, ok := loadListenerMutation(store, listenerID, writer, request)
	if !ok {
		return
	}
	webhooks, ok := nestedWebhooks(listener, contractID, eventID, writer)
	if !ok {
		return
	}
	index := webhookIndex(*webhooks, webhookID)
	if index < 0 {
		writeAPIError(writer, http.StatusNotFound, "webhook configuration not found")
		return
	}
	*webhooks = append((*webhooks)[:index], (*webhooks)[index+1:]...)
	if !validateDeletion(snapshot, writer) {
		return
	}
	revision, err := store.ReplaceRPCListenerAudited(request.Context(), *listener, expected, time.Now().UTC(),
		mutationAudit(request, core.AuditActionDelete, core.AuditResourceWebhook, webhookID, listenerID))
	if !writeMutationError(writer, revision, err) {
		return
	}
	writeDeleted(writer, revision)
}

func nestedWebhooks(listener *core.RPCListener, contractID, eventID string, writer http.ResponseWriter) (*[]core.WebhookConfig, bool) {
	if contractID == "" {
		return &listener.Webhooks, true
	}
	contract := findContract(listener, contractID)
	if contract == nil {
		writeAPIError(writer, http.StatusNotFound, "contract configuration not found")
		return nil, false
	}
	if eventID == "" {
		return &contract.Webhooks, true
	}
	event := findEvent(contract, eventID)
	if event == nil {
		writeAPIError(writer, http.StatusNotFound, "event configuration not found")
		return nil, false
	}
	return &event.Webhooks, true
}

func validateDeletion(snapshot core.RPCListenerSnapshot, writer http.ResponseWriter) bool {
	if err := validateRPCListenerSnapshot(snapshot); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return false
	}
	return true
}

func contractIndex(contracts []core.ContractConfig, id string) int {
	for index := range contracts {
		if contracts[index].ID == id {
			return index
		}
	}
	return -1
}

func eventIndex(events []core.EventConfig, id string) int {
	for index := range events {
		if events[index].ID == id {
			return index
		}
	}
	return -1
}

func webhookIndex(webhooks []core.WebhookConfig, id string) int {
	for index := range webhooks {
		if webhooks[index].ID == id {
			return index
		}
	}
	return -1
}

func writeDeleted(writer http.ResponseWriter, revision uint64) {
	writer.Header().Set("ETag", revisionETag(revision))
	writer.Header().Set("X-Config-Revision", revisionString(revision))
	writer.WriteHeader(http.StatusNoContent)
}

func revisionString(revision uint64) string {
	return strconv.FormatUint(revision, 10)
}
