package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestManagementAPIPatchesEveryResourceWithoutReplacingUnrelatedFields(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	config, globalID, revision := createPatchFixture(t, store)
	handler := healthHandler(store)
	contract := config.Contracts[0]
	event := contract.Events[0]
	chainWebhook := config.Webhooks[0]
	contractWebhook := contract.Webhooks[0]
	eventWebhook := event.Webhooks[0]
	originalCreatedAt := config.CreatedAt

	response := patchManagement(t, handler, secret, "/api/v1/rpc-listeners/"+config.ID, revisionETag(revision), map[string]any{
		"name": "renamed", "tls": map[string]any{"caPem": nil},
	})
	revision++
	assertPatched(t, response, revision, "rpcListener")
	if strings.Contains(response.Body.String(), "rpc-password") || strings.Contains(response.Body.String(), "rpc-token") {
		t.Fatalf("listener patch response exposed RPC credentials: %s", response.Body.String())
	}

	response = patchManagement(t, handler, secret,
		"/api/v1/rpc-listeners/"+config.ID+"/contracts/"+contract.ID,
		revisionETag(revision), map[string]any{"abi": json.RawMessage(patchFixtureABI)})
	revision++
	assertPatched(t, response, revision, "contract")

	response = patchManagement(t, handler, secret,
		"/api/v1/rpc-listeners/"+config.ID+"/contracts/"+contract.ID+"/events/"+event.ID,
		revisionETag(revision), map[string]any{"selector": "Approval()"})
	revision++
	assertPatched(t, response, revision, "event")

	patches := []struct {
		path, id, url string
	}{
		{"/api/v1/rpc-listeners/webhooks/" + globalID, globalID, "https://new-global.example.test/hook?token=new-global"},
		{"/api/v1/rpc-listeners/" + config.ID + "/webhooks/" + chainWebhook.ID, chainWebhook.ID, "https://new-chain.example.test/hook?token=new-chain"},
		{"/api/v1/rpc-listeners/" + config.ID + "/contracts/" + contract.ID + "/webhooks/" + contractWebhook.ID, contractWebhook.ID, "https://new-contract.example.test/hook?token=new-contract"},
		{"/api/v1/rpc-listeners/" + config.ID + "/contracts/" + contract.ID + "/events/" + event.ID + "/webhooks/" + eventWebhook.ID, eventWebhook.ID, "https://new-event.example.test/hook?token=new-event"},
	}
	for _, patch := range patches {
		response = patchManagement(t, handler, secret, patch.path, revisionETag(revision), map[string]any{"url": patch.url})
		revision++
		assertPatched(t, response, revision, "webhook")
		if strings.Contains(response.Body.String(), "token=") {
			t.Fatalf("webhook patch response exposed URL credentials: %s", response.Body.String())
		}
	}

	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	updated := snapshot.Listeners[0]
	if snapshot.Revision != revision || updated.ID != config.ID || !updated.CreatedAt.Equal(originalCreatedAt) || updated.Name != "renamed" ||
		updated.RPCURL != config.RPCURL || updated.TLS.CAPEM != "" || updated.TLS.ServerName != config.TLS.ServerName ||
		string(updated.Contracts[0].ABI) != patchFixtureABI || updated.Contracts[0].Events[0].Selector != "Approval()" {
		t.Fatalf("targeted patch replaced unrelated data: %#v", snapshot)
	}
	if snapshot.GlobalWebhooks[0].URL != patches[0].url || updated.Webhooks[0].URL != patches[1].url ||
		updated.Contracts[0].Webhooks[0].URL != patches[2].url || updated.Contracts[0].Events[0].Webhooks[0].URL != patches[3].url {
		t.Fatalf("webhook patches not persisted: %#v", snapshot)
	}
	audit, err := store.RPCListenerAudit(context.Background(), 20, 0)
	if err != nil || len(audit) != 7 {
		t.Fatalf("patch audit entries = %d, error %v", len(audit), err)
	}
	for _, entry := range audit {
		if entry.Action != core.AuditActionUpdate || entry.ActorName != "create-client" || (entry.ParentListenerID != config.ID && entry.ParentListenerID != "") {
			t.Fatalf("patch audit entry = %#v", entry)
		}
	}
}

func TestManagementAPIPatchAuthorizationPreconditionsAndMediaType(t *testing.T) {
	adminStore, adminSecret := emptyManagementFixture(t, core.APIKeyAdmin)
	config, _, revision := createPatchFixture(t, adminStore)
	handler := healthHandler(adminStore)

	tests := []struct {
		name, etag, contentType, body string
		want                          int
	}{
		{"missing revision", "", "application/merge-patch+json", `{"name":"other"}`, http.StatusPreconditionRequired},
		{"stale revision", `"revision-0"`, "application/merge-patch+json", `{"name":"other"}`, http.StatusPreconditionFailed},
		{"wrong media type", revisionETag(revision), "application/json", `{"name":"other"}`, http.StatusUnsupportedMediaType},
		{"unknown field", revisionETag(revision), "application/merge-patch+json", `{"id":"` + core.NewConfigID() + `"}`, http.StatusBadRequest},
		{"immutable timestamp", revisionETag(revision), "application/merge-patch+json", `{"createdAt":"2026-01-01T00:00:00Z"}`, http.StatusBadRequest},
		{"unknown TLS field", revisionETag(revision), "application/merge-patch+json", `{"tls":{"privateKey":"secret"}}`, http.StatusBadRequest},
		{"top-level null", revisionETag(revision), "application/merge-patch+json", `null`, http.StatusBadRequest},
		{"empty patch", revisionETag(revision), "application/merge-patch+json", `{}`, http.StatusBadRequest},
		{"required null", revisionETag(revision), "application/merge-patch+json", `{"name":null}`, http.StatusUnprocessableEntity},
		{"nested required null", revisionETag(revision), "application/merge-patch+json", `{"tls":{"insecureSkipVerify":null}}`, http.StatusUnprocessableEntity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPatch, "/api/v1/rpc-listeners/"+config.ID, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+adminSecret)
			request.Header.Set("Content-Type", test.contentType)
			if test.etag != "" {
				request.Header.Set("If-Match", test.etag)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.want, response.Body.String())
			}
		})
	}

	readOnlyStore, readOnlySecret := emptyManagementFixture(t, core.APIKeyReadOnly)
	readOnlyConfig, _, readOnlyRevision := createPatchFixture(t, readOnlyStore)
	response := patchManagement(t, healthHandler(readOnlyStore), readOnlySecret, "/api/v1/rpc-listeners/"+readOnlyConfig.ID, revisionETag(readOnlyRevision), map[string]any{"name": "other"})
	if response.Code != http.StatusForbidden {
		t.Fatalf("read-only patch = %d %s", response.Code, response.Body.String())
	}
}

func TestManagementAPIInvalidPatchRollsBackAndPreservesSecrets(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	config, _, revision := createPatchFixture(t, store)
	contract := config.Contracts[0]
	before, err := store.RPCListenerSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	response := patchManagement(t, healthHandler(store), secret,
		"/api/v1/rpc-listeners/"+config.ID+"/contracts/"+contract.ID,
		revisionETag(revision), map[string]any{"abi": json.RawMessage(`[]`)})
	if response.Code != http.StatusUnprocessableEntity || strings.Contains(response.Body.String(), "rpc-password") || strings.Contains(response.Body.String(), "old-event") {
		t.Fatalf("invalid patch = %d %s", response.Code, response.Body.String())
	}
	after, err := store.RPCListenerSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("invalid patch mutated store\nbefore: %s\nafter:  %s", beforeJSON, afterJSON)
	}
}

func TestManagementAPIPatchPreservesOmittedWriteOnlyRPCCredentials(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	if err := store.ConfigureRPCSecrets([]byte("test RPC credential encryption key")); err != nil {
		t.Fatal(err)
	}
	body := validListenerCreateBody()
	body["rpcAuthentication"] = map[string]any{
		"type": "provider-jwt", "tokenUrl": "https://provider.example/token",
		"tokenApiKey": "old-api-key", "secret": "old-signature",
	}
	created := postManagement(t, healthHandler(store), secret, "/api/v1/rpc-listeners", revisionETag(0), body)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	var result struct {
		Revision    uint64                 `json:"revision"`
		RPCListener rpcListenerAPIResponse `json:"rpcListener"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	patched := patchManagement(t, healthHandler(store), secret, "/api/v1/rpc-listeners/"+result.RPCListener.ID, revisionETag(result.Revision), map[string]any{
		"rpcAuthentication": map[string]any{
			"type": "provider-jwt", "tokenUrl": "https://provider.example/token",
			"tokenApiKey": "new-api-key", "secret": "",
		},
	})
	if patched.Code != http.StatusOK || strings.Contains(patched.Body.String(), "new-api-key") || strings.Contains(patched.Body.String(), "old-signature") {
		t.Fatalf("patch = %d %s", patched.Code, patched.Body.String())
	}
	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	authentication := snapshot.Listeners[0].RPCAuthentication
	if authentication.Secret != "old-signature" || authentication.TokenAPIKey != "new-api-key" {
		t.Fatalf("credentials after patch = %#v", authentication)
	}
}

func TestManagementAPIContractPatchReconcilesEventsAtomically(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	config, _, revision := createPatchFixture(t, store)
	contract := config.Contracts[0]
	originalEvent := contract.Events[0]
	updatedABI := json.RawMessage(`[{"type":"event","name":"Transfer","inputs":[]},{"type":"event","name":"Mint","inputs":[]}]`)

	response := patchManagement(t, healthHandler(store), secret,
		"/api/v1/rpc-listeners/"+config.ID+"/contracts/"+contract.ID,
		revisionETag(revision), map[string]any{"abi": updatedABI, "eventSelectors": []string{"Transfer()", "Mint()"}})
	assertPatched(t, response, revision+1, "contract")

	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	updated := snapshot.Listeners[0].Contracts[0]
	if string(updated.ABI) != string(updatedABI) || len(updated.Events) != 2 {
		t.Fatalf("contract patch not persisted atomically: %#v", updated)
	}
	if updated.Events[0].ID != originalEvent.ID || len(updated.Events[0].Webhooks) != 1 || updated.Events[0].Webhooks[0].ID != originalEvent.Webhooks[0].ID {
		t.Fatalf("retained event lost identity or nested webhooks: %#v", updated.Events[0])
	}
	if updated.Events[1].ID == "" || updated.Events[1].ID == originalEvent.ID || updated.Events[1].Selector != "Mint()" || len(updated.Events[1].Webhooks) != 0 {
		t.Fatalf("new event was not initialized safely: %#v", updated.Events[1])
	}
}

func TestManagementAPIContractPatchRejectsInvalidEventSelections(t *testing.T) {
	for _, selectors := range [][]string{{"Transfer()", "Transfer()"}, {""}, {"Missing()"}} {
		store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
		config, _, revision := createPatchFixture(t, store)
		response := patchManagement(t, healthHandler(store), secret,
			"/api/v1/rpc-listeners/"+config.ID+"/contracts/"+config.Contracts[0].ID,
			revisionETag(revision), map[string]any{"eventSelectors": selectors})
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("eventSelectors %#v = %d, want 422, body %s", selectors, response.Code, response.Body.String())
		}
		snapshot, err := store.RPCListenerSnapshot(context.Background())
		if err != nil || snapshot.Revision != revision || snapshot.Listeners[0].Contracts[0].Events[0].ID != config.Contracts[0].Events[0].ID {
			t.Fatalf("invalid selectors mutated configuration: revision %d, error %v", snapshot.Revision, err)
		}
	}
}

func TestManagementAPIPatchRejectsInvalidResourceIDsAndMissingResources(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	config, _, revision := createPatchFixture(t, store)
	handler := healthHandler(store)
	tests := []struct {
		path string
		want int
	}{
		{"/api/v1/rpc-listeners/not-a-uuid", http.StatusBadRequest},
		{"/api/v1/rpc-listeners/webhooks/not-a-uuid", http.StatusBadRequest},
		{"/api/v1/rpc-listeners/" + config.ID + "/contracts/not-a-uuid", http.StatusBadRequest},
		{"/api/v1/rpc-listeners/" + config.ID + "/contracts/" + core.NewConfigID(), http.StatusNotFound},
	}
	for _, test := range tests {
		response := patchManagement(t, handler, secret, test.path, revisionETag(revision), map[string]any{"name": "other"})
		if response.Code != test.want {
			t.Fatalf("PATCH %s = %d, want %d, body = %s", test.path, response.Code, test.want, response.Body.String())
		}
	}
}

func patchManagement(t *testing.T, handler http.Handler, secret, path, etag string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(string(encoded)))
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Content-Type", "application/merge-patch+json")
	if etag != "" {
		request.Header.Set("If-Match", etag)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertPatched(t *testing.T, response *httptest.ResponseRecorder, revision uint64, field string) {
	t.Helper()
	if response.Code != http.StatusOK || response.Header().Get("ETag") != revisionETag(revision) {
		t.Fatalf("patch %s = %d, ETag %q, body %s", field, response.Code, response.Header().Get("ETag"), response.Body.String())
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result[field]) == 0 {
		t.Fatalf("patch %s response = %s, error %v", field, response.Body.String(), err)
	}
}

const patchFixtureABI = `[{"type":"event","name":"Transfer","inputs":[]},{"type":"event","name":"Approval","inputs":[]}]`

func createPatchFixture(t *testing.T, store interface {
	CreateRPCListener(context.Context, core.RPCListener, uint64, time.Time) (uint64, error)
	ReplaceGlobalWebhooks(context.Context, []core.WebhookConfig, uint64, time.Time) (uint64, error)
}) (core.RPCListener, string, uint64) {
	t.Helper()
	now := time.Now().UTC().Add(-time.Hour)
	config := core.RPCListener{
		ID: core.NewConfigID(), Name: "private", ChainID: 1,
		RPCURL:    "https://rpc-user:rpc-password@rpc.example.test/path?token=rpc-token",
		BatchSize: 100, PollInterval: time.Second, ReorgDepth: 5,
		RPCRetryAttempts: 2, RPCRetryBackoff: time.Second, RPCTimeout: 5 * time.Second,
		TLS:      core.ListenerTLSConfig{CAPEM: "old-ca", ServerName: "rpc.internal"},
		Webhooks: []core.WebhookConfig{{ID: core.NewConfigID(), URL: "https://chain.example.test/hook?token=old-chain"}},
		Contracts: []core.ContractConfig{{
			ID: core.NewConfigID(), Address: "0x0000000000000000000000000000000000000001", ABI: json.RawMessage(patchFixtureABI),
			Webhooks: []core.WebhookConfig{{ID: core.NewConfigID(), URL: "https://contract.example.test/hook?token=old-contract"}},
			Events: []core.EventConfig{{
				ID: core.NewConfigID(), Selector: "Transfer()",
				Webhooks: []core.WebhookConfig{{ID: core.NewConfigID(), URL: "https://event.example.test/hook?token=old-event"}},
			}},
		}},
	}
	revision, err := store.CreateRPCListener(context.Background(), config, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	globalID := core.NewConfigID()
	revision, err = store.ReplaceGlobalWebhooks(context.Background(), []core.WebhookConfig{{ID: globalID, URL: "https://global.example.test/hook?token=old-global"}}, revision, now)
	if err != nil {
		t.Fatal(err)
	}
	config.CreatedAt = now
	return config, globalID, revision
}
