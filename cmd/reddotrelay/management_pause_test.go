package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"reddotrelay/internal/core"
)

func TestPauseResumeIsDurableAuditedAndIdempotent(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	handler := healthHandler(store)
	created := postManagement(t, handler, secret, "/api/v1/rpc-listeners", `"revision-0"`, validListenerCreateBody())
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	var response struct {
		RPCListener rpcListenerAPIResponse `json:"rpcListener"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	id := response.RPCListener.ID
	paused := transferRequest(t, handler, secret, http.MethodPost, "/api/v1/rpc-listeners/"+id+"/pause", `"revision-1"`, "")
	if paused.Code != http.StatusOK || paused.Header().Get("ETag") != `"revision-2"` {
		t.Fatalf("pause = %d %s", paused.Code, paused.Body.String())
	}
	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil || !snapshot.Listeners[0].Paused {
		t.Fatalf("paused snapshot = %#v, %v", snapshot, err)
	}
	again := transferRequest(t, handler, secret, http.MethodPost, "/api/v1/rpc-listeners/"+id+"/pause", `"revision-2"`, "")
	if again.Code != http.StatusOK || again.Header().Get("ETag") != `"revision-2"` {
		t.Fatalf("idempotent pause = %d %s", again.Code, again.Body.String())
	}
	resumed := transferRequest(t, handler, secret, http.MethodPost, "/api/v1/rpc-listeners/"+id+"/resume", `"revision-2"`, "")
	if resumed.Code != http.StatusOK || resumed.Header().Get("ETag") != `"revision-3"` {
		t.Fatalf("resume = %d %s", resumed.Code, resumed.Body.String())
	}
	audit, err := store.RPCListenerAudit(context.Background(), 10, 0)
	if err != nil || len(audit) != 3 || audit[0].Action != core.AuditActionResume || audit[1].Action != core.AuditActionPause {
		t.Fatalf("pause audit = %#v, %v", audit, err)
	}
}
