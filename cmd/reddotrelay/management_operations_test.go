package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestRPCListenerAuditEndpointAttributionPaginationAndSecretSafety(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	handler := healthHandler(store)
	body := validListenerCreateBody()
	body["rpcUrl"] = "https://rpc-user:rpc-password@rpc.example.test/path?api_key=rpc-secret"
	body["webhooks"] = []map[string]any{{"url": "https://hooks.example.test/receive?signature=webhook-secret"}}
	body["contracts"] = []map[string]any{{
		"address": "0x0000000000000000000000000000000000000001",
		"abi":     []map[string]any{{"type": "event", "name": "SensitiveABIName", "inputs": []any{}}},
	}}
	created := postManagement(t, handler, secret, "/api/v1/rpc-listeners", `"revision-0"`, body)
	listenerID := createdResourceID(t, created, "rpcListener", 1)
	firstPatch := patchManagement(t, handler, secret, "/api/v1/rpc-listeners/"+listenerID, `"revision-1"`, map[string]any{"name": "renamed-once"})
	assertPatched(t, firstPatch, 2, "rpcListener")
	secondPatch := patchManagement(t, handler, secret, "/api/v1/rpc-listeners/"+listenerID, `"revision-2"`, map[string]any{"name": "renamed-twice"})
	assertPatched(t, secondPatch, 3, "rpcListener")

	pageOne := authenticatedRequest(handler, secret, http.MethodGet, "/api/v1/rpc-listener-audit?limit=2")
	if pageOne.Code != http.StatusOK {
		t.Fatalf("audit page one = %d, body %s", pageOne.Code, pageOne.Body.String())
	}
	for _, secretValue := range []string{"rpc-password", "rpc-secret", "webhook-secret", "SensitiveABIName", secret} {
		if strings.Contains(pageOne.Body.String(), secretValue) {
			t.Fatalf("audit response contains secret %q: %s", secretValue, pageOne.Body.String())
		}
	}
	var first struct {
		Entries []struct {
			ActorID          string          `json:"actorId"`
			ActorName        string          `json:"actorName"`
			ActorRole        core.APIKeyRole `json:"actorRole"`
			Action           string          `json:"action"`
			ResourceKind     string          `json:"resourceKind"`
			ResourceID       string          `json:"resourceId"`
			ParentListenerID string          `json:"parentListenerId"`
			PreviousRevision uint64          `json:"previousRevision"`
			NewRevision      uint64          `json:"newRevision"`
		} `json:"entries"`
		NextBefore string `json:"nextBefore"`
	}
	if err := json.Unmarshal(pageOne.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 2 || first.NextBefore == "" || first.Entries[0].ActorName != "create-client" ||
		first.Entries[0].ActorRole != core.APIKeyAdmin || first.Entries[0].Action != core.AuditActionUpdate ||
		first.Entries[0].ResourceKind != core.AuditResourceRPCListener || first.Entries[0].ResourceID != listenerID ||
		first.Entries[0].ParentListenerID != "" || first.Entries[0].PreviousRevision != 2 || first.Entries[0].NewRevision != 3 {
		t.Fatalf("audit page one = %#v", first)
	}
	if first.Entries[0].ActorID != keyIDForSecret(t, store) {
		t.Fatalf("audit actor ID = %q", first.Entries[0].ActorID)
	}
	pageTwo := authenticatedRequest(handler, secret, http.MethodGet, "/api/v1/rpc-listener-audit?limit=2&before="+first.NextBefore)
	var second struct {
		Entries []auditAPIResponse `json:"entries"`
	}
	if pageTwo.Code != http.StatusOK || json.Unmarshal(pageTwo.Body.Bytes(), &second) != nil || len(second.Entries) != 1 || second.Entries[0].Action != core.AuditActionCreate {
		t.Fatalf("audit page two = %d, body %s", pageTwo.Code, pageTwo.Body.String())
	}

	readStore, readSecret := emptyManagementFixture(t, core.APIKeyReadOnly)
	forbidden := authenticatedRequest(healthHandler(readStore), readSecret, http.MethodGet, "/api/v1/rpc-listener-audit")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("read-only audit status = %d", forbidden.Code)
	}
}

func TestManagementSecurityHeadersIncludeAuthenticationFailures(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyReadOnly)
	handler := healthHandler(store)
	for _, response := range []*httptest.ResponseRecorder{
		authenticatedRequest(handler, secret, http.MethodGet, "/api/v1/rpc-listeners"),
		authenticatedRequest(handler, "", http.MethodGet, "/api/v1/rpc-listeners"),
	} {
		if response.Header().Get("Cache-Control") != "no-store" ||
			response.Header().Get("X-Content-Type-Options") != "nosniff" ||
			response.Header().Get("Referrer-Policy") != "no-referrer" ||
			response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("security headers = %#v", response.Header())
		}
	}
}

func TestRuntimeStatusAndReadinessTransitions(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyReadOnly)
	listener := runtimeListenerFixture()
	source := newFakeRPCListenerSource(core.RPCListenerSnapshot{Revision: 1, Listeners: []core.RPCListener{listener}})
	factory := newFakeRuntimeFactory()
	manager := testRuntimeManager(t, source, factory.build)
	manager.retryBase = time.Hour
	manager.retryMax = time.Hour
	handler := healthHandler(store, manager)

	if response := authenticatedRequest(handler, "", http.MethodGet, "/readyz"); response.Code != http.StatusServiceUnavailable || response.Body.String() != `{"status":"not_ready"}` {
		t.Fatalf("initial readiness = %d %s", response.Code, response.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntimeManager(manager, ctx)
	waitFor(t, manager.Ready, "initial runtime readiness")
	if response := authenticatedRequest(handler, "", http.MethodGet, "/readyz"); response.Code != http.StatusOK || response.Body.String() != `{"status":"ready"}` {
		t.Fatalf("ready response = %d %s", response.Code, response.Body.String())
	}
	status := authenticatedRequest(handler, secret, http.MethodGet, "/api/v1/rpc-listener-status")
	if status.Code != http.StatusOK || strings.Contains(status.Body.String(), listener.RPCURL) || !strings.Contains(status.Body.String(), `"state":"running"`) {
		t.Fatalf("runtime status = %d %s", status.Code, status.Body.String())
	}

	factory.failNext.Store(100)
	listener.RPCURL = "https://user:password@failed.example.test?token=signed-secret"
	source.set(core.RPCListenerSnapshot{Revision: 2, Listeners: []core.RPCListener{listener}})
	waitFor(t, func() bool { return !manager.Ready() && manager.Status().DesiredRevision == 2 }, "failed runtime readiness")
	status = authenticatedRequest(handler, secret, http.MethodGet, "/api/v1/rpc-listener-status")
	for _, forbidden := range []string{"password", "signed-secret", listener.RPCURL} {
		if strings.Contains(status.Body.String(), forbidden) {
			t.Fatalf("runtime status leaked %q: %s", forbidden, status.Body.String())
		}
	}
	if !strings.Contains(status.Body.String(), `"state":"build-failed"`) || !strings.Contains(status.Body.String(), `"lastError":"runtime construction failed"`) {
		t.Fatalf("failed runtime status = %s", status.Body.String())
	}
	if response := authenticatedRequest(handler, "", http.MethodGet, "/readyz"); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed readiness = %d", response.Code)
	}

	factory.failNext.Store(0)
	listener.RPCURL = "https://recovered.example.test"
	source.set(core.RPCListenerSnapshot{Revision: 3, Listeners: []core.RPCListener{listener}})
	waitFor(t, func() bool { return manager.Ready() && manager.Status().DesiredRevision == 3 }, "recovered runtime readiness")

	var readers sync.WaitGroup
	for i := 0; i < 20; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				_ = manager.Status()
			}
		}()
	}
	readers.Wait()
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("manager Run() = %v", err)
	}
}

func TestReadinessFailsWhenDatabaseIsUnavailable(t *testing.T) {
	store, _ := emptyManagementFixture(t, core.APIKeyReadOnly)
	source := newFakeRPCListenerSource(core.RPCListenerSnapshot{})
	manager := testRuntimeManager(t, source, newFakeRuntimeFactory().build)
	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntimeManager(manager, ctx)
	waitFor(t, manager.Ready, "empty desired state readiness")
	handler := healthHandler(store, manager)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if response := authenticatedRequest(handler, "", http.MethodGet, "/readyz"); response.Code != http.StatusServiceUnavailable || response.Body.String() != `{"status":"not_ready"}` {
		t.Fatalf("database readiness = %d %s", response.Code, response.Body.String())
	}
	cancel()
	<-done
}

func authenticatedRequest(handler http.Handler, secret, method, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
