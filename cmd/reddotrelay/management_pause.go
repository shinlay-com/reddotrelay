package main

import (
	"net/http"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
)

func setListenerPaused(store *sqlite.Store, listenerID string, paused bool, writer http.ResponseWriter, request *http.Request) {
	expected, snapshot, listener, ok := loadListenerMutation(store, listenerID, writer, request)
	if !ok {
		return
	}
	if listener.Paused == paused {
		writer.Header().Set("ETag", revisionETag(expected))
		writeJSON(writer, http.StatusOK, map[string]any{"revision": expected, "rpcListener": rpcListenerResponse(*listener, snapshot.GlobalWebhooks)})
		return
	}
	listener.Paused = paused
	action := core.AuditActionPause
	if !paused {
		action = core.AuditActionResume
	}
	revision, err := store.ReplaceRPCListenerAudited(request.Context(), *listener, expected, time.Now().UTC(), mutationAudit(request, action, core.AuditResourceRPCListener, listenerID, ""))
	if !writeMutationError(writer, revision, err) {
		return
	}
	updated, ok := loadPatchedListener(store, listenerID, writer, request)
	if !ok {
		return
	}
	writer.Header().Set("ETag", revisionETag(revision))
	writeJSON(writer, http.StatusOK, map[string]any{"revision": revision, "rpcListener": rpcListenerResponse(*updated, snapshot.GlobalWebhooks)})
}
