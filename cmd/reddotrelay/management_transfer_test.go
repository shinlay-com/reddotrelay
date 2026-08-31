package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reddotrelay/internal/core"
)

func TestConfigurationExportImportRoundTripIsAtomicAndAudited(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	handler := healthHandler(store)
	body := validListenerCreateBody()
	delete(body, "rpcUrl")
	body["rpcUrlRef"] = "env://RPC_URL"
	body["webhooks"] = []any{map[string]any{"urlRef": "file:///run/secrets/webhook_url", "authentication": map[string]any{"type": "hmac-sha256", "secretRef": "env://HMAC_KEY", "keyId": "receiver-1"}}}
	created := postManagement(t, handler, secret, "/api/v1/rpc-listeners", `"revision-0"`, body)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}

	first := transferRequest(t, handler, secret, http.MethodGet, "/api/v1/rpc-listener-export", "", "")
	second := transferRequest(t, handler, secret, http.MethodGet, "/api/v1/rpc-listener-export", "", "")
	if first.Code != http.StatusOK || first.Header().Get("ETag") != `"revision-1"` {
		t.Fatalf("export = %d %s", first.Code, first.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatal("unchanged configuration export is not deterministic")
	}
	if strings.Contains(first.Body.String(), "createdAt") || strings.Contains(first.Body.String(), "revision\"") {
		t.Fatalf("export contains server metadata: %s", first.Body.String())
	}
	var document configurationDocument
	if err := json.Unmarshal(first.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.SchemaVersion != configurationSchemaVersion || len(document.RPCListeners) != 1 {
		t.Fatalf("document = %#v", document)
	}
	originalID := document.RPCListeners[0].ID
	document.RPCListeners[0].Name = "imported-name"
	document.RPCListeners[0].Paused = true
	encoded, _ := json.Marshal(document)
	imported := transferRequest(t, handler, secret, http.MethodPut, "/api/v1/rpc-listener-import", `"revision-1"`, string(encoded))
	if imported.Code != http.StatusOK || imported.Header().Get("ETag") != `"revision-2"` {
		t.Fatalf("import = %d %s", imported.Code, imported.Body.String())
	}
	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 2 || len(snapshot.Listeners) != 1 || snapshot.Listeners[0].ID != originalID || snapshot.Listeners[0].Name != "imported-name" || !snapshot.Listeners[0].Paused || snapshot.Listeners[0].RPCURLRef != "env://RPC_URL" || snapshot.Listeners[0].Webhooks[0].Authentication.SecretRef != "env://HMAC_KEY" {
		t.Fatalf("imported snapshot = %#v", snapshot)
	}
	audit, err := store.RPCListenerAudit(context.Background(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if audit[0].Action != core.AuditActionImport || audit[0].ResourceKind != core.AuditResourceConfiguration || audit[0].PreviousRevision != 1 || audit[0].NewRevision != 2 {
		t.Fatalf("import audit = %#v", audit[0])
	}
}

func TestConfigurationExportRefusesCredentialBearingDirectURLs(t *testing.T) {
	store, secret, _ := managementFixture(t, core.APIKeyAdmin)
	response := transferRequest(t, healthHandler(store), secret, http.MethodGet, "/api/v1/rpc-listener-export", "", "")
	if response.Code != http.StatusConflict {
		t.Fatalf("export = %d %s", response.Code, response.Body.String())
	}
	for _, secretValue := range []string{"rpc-password", "rpc-path-secret", "webhook-secret"} {
		if strings.Contains(response.Body.String(), secretValue) {
			t.Fatalf("export error leaked %q", secretValue)
		}
	}
}

func TestConfigurationExportRefusesAdminManagedRPCCredentials(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	if err := store.ConfigureRPCSecrets([]byte("test RPC credential encryption key")); err != nil {
		t.Fatal(err)
	}
	body := validListenerCreateBody()
	body["rpcAuthentication"] = map[string]any{
		"type": "provider-jwt", "tokenUrl": "https://provider.example/token",
		"tokenApiKey": "private-api-key", "secret": "private-signature",
	}
	created := postManagement(t, healthHandler(store), secret, "/api/v1/rpc-listeners", revisionETag(0), body)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	response := transferRequest(t, healthHandler(store), secret, http.MethodGet, "/api/v1/rpc-listener-export", "", "")
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "private-api-key") || strings.Contains(response.Body.String(), "private-signature") {
		t.Fatalf("export = %d %s", response.Code, response.Body.String())
	}
}

func TestConfigurationImportValidationAndConflictDoNotMutate(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	handler := healthHandler(store)
	document := `{"schemaVersion":"reddotrelay.config/v1","globalWebhooks":[],"rpcListeners":[]}`
	stale := transferRequest(t, handler, secret, http.MethodPut, "/api/v1/rpc-listener-import", `"revision-1"`, document)
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale = %d %s", stale.Code, stale.Body.String())
	}
	invalid := transferRequest(t, handler, secret, http.MethodPut, "/api/v1/rpc-listener-import", `"revision-0"`, `{"schemaVersion":"wrong","globalWebhooks":[],"rpcListeners":[]}`)
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid = %d %s", invalid.Code, invalid.Body.String())
	}
	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil || snapshot.Revision != 0 || len(snapshot.Listeners) != 0 {
		t.Fatalf("failed import mutated state: %#v %v", snapshot, err)
	}
}

func transferRequest(t *testing.T, handler http.Handler, secret, method, path, etag, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+secret)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		request.Header.Set("If-Match", etag)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
