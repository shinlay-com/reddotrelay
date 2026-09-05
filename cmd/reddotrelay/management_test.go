package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reddotrelay/internal/auth"
	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
)

func TestManagementAPIListsAndGetsRPCListenersWithReadOnlyKey(t *testing.T) {
	store, secret, config := managementFixture(t, core.APIKeyReadOnly)
	server := httptest.NewServer(healthHandler(store))
	defer server.Close()

	for _, path := range []string{"/api/v1/rpc-listeners", "/api/v1/rpc-listeners/" + config.ID} {
		request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get("ETag") != `"revision-1"` {
			t.Fatalf("GET %s status = %d, ETag = %q, body = %#v", path, response.StatusCode, response.Header.Get("ETag"), body)
		}
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("GET %s Cache-Control = %q", path, response.Header.Get("Cache-Control"))
		}
		encoded, _ := json.Marshal(body)
		text := string(encoded)
		if strings.Contains(text, "rpc-user") || strings.Contains(text, "rpc-password") || strings.Contains(text, "rpc-path-secret") || strings.Contains(text, "api_key") || strings.Contains(text, "webhook-path-secret") || strings.Contains(text, "webhook-secret") || strings.Contains(text, secret) {
			t.Fatalf("GET %s exposed a secret: %s", path, text)
		}
		if !strings.Contains(text, `"rpcUrl":"https://rpc.example.test"`) || !strings.Contains(text, `"webhookSource":"contract"`) {
			t.Fatalf("GET %s response missing redacted URL or effective route: %s", path, text)
		}
	}
}

func TestRedactConfiguredURLReturnsOnlyOrigin(t *testing.T) {
	got := redactConfiguredURL("https://user:password@example.test/path-secret?query-secret=value#fragment-secret")
	if got != "https://example.test" {
		t.Fatalf("redactConfiguredURL() = %q", got)
	}
}

func TestManagementAPIAuthenticationAndRouting(t *testing.T) {
	store, secret, config := managementFixture(t, core.APIKeyAdmin)
	handler := healthHandler(store)

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("unauthenticated health status = %d", health.Code)
	}

	tests := []struct {
		name, method, path, authorization string
		want                              int
	}{
		{"missing key", http.MethodGet, "/api/v1/rpc-listeners", "", http.StatusUnauthorized},
		{"malformed header", http.MethodGet, "/api/v1/rpc-listeners", "Basic abc", http.StatusUnauthorized},
		{"unknown key", http.MethodGet, "/api/v1/rpc-listeners", "Bearer api_key_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", http.StatusUnauthorized},
		{"malformed id", http.MethodGet, "/api/v1/rpc-listeners/not-a-uuid", "Bearer " + secret, http.StatusBadRequest},
		{"noncanonical id", http.MethodGet, "/api/v1/rpc-listeners/" + strings.ToUpper(config.ID), "Bearer " + secret, http.StatusBadRequest},
		{"missing id", http.MethodGet, "/api/v1/rpc-listeners/" + core.NewConfigID(), "Bearer " + secret, http.StatusNotFound},
		{"unsupported method", http.MethodPut, "/api/v1/rpc-listeners", "Bearer " + secret, http.StatusMethodNotAllowed},
		{"extra path", http.MethodGet, "/api/v1/rpc-listeners/" + config.ID + "/events", "Bearer " + secret, http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Header.Set("Authorization", test.authorization)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.want, response.Body.String())
			}
			if response.Code == http.StatusUnauthorized && response.Body.String() != "{\"error\":\"unauthorized\"}\n" {
				t.Fatalf("authentication response leaked detail: %s", response.Body.String())
			}
		})
	}

	if err := store.RevokeAPIKey(context.Background(), keyIDForSecret(t, store), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/rpc-listeners", nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key status = %d", response.Code)
	}
}

func TestManagementAPICreatesCompleteRPCListener(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	handler := healthHandler(store)
	body := validListenerCreateBody()
	body["paused"] = true
	body["rpcUrl"] = "https://rpc-user:rpc-password@rpc.example.test/path?api_key=secret"
	body["webhooks"] = []any{map[string]any{"url": "https://chain.example.test/hook?signature=webhook-secret"}}
	body["contracts"] = []any{map[string]any{
		"address": "0x0000000000000000000000000000000000000001",
		"abi":     json.RawMessage(`[ {"type":"event","name":"Transfer","inputs":[]} ]`),
		"events": []any{map[string]any{
			"selector": "Transfer()",
			"webhooks": []any{map[string]any{"url": "https://event.example.test/hook?token=event-secret"}},
		}},
	}}

	response := postManagement(t, handler, secret, "/api/v1/rpc-listeners", `"revision-0"`, body)
	if response.Code != http.StatusCreated || response.Header().Get("ETag") != `"revision-1"` {
		t.Fatalf("create status = %d, ETag = %q, body = %s", response.Code, response.Header().Get("ETag"), response.Body.String())
	}
	var result struct {
		Revision    uint64                 `json:"revision"`
		RPCListener rpcListenerAPIResponse `json:"rpcListener"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Revision != 1 || !result.RPCListener.Paused || !canonicalUUID(result.RPCListener.ID) || len(result.RPCListener.Contracts) != 1 ||
		!canonicalUUID(result.RPCListener.Contracts[0].ID) || len(result.RPCListener.Contracts[0].Events) != 1 ||
		!canonicalUUID(result.RPCListener.Contracts[0].Events[0].ID) || len(result.RPCListener.Webhooks) != 1 ||
		!canonicalUUID(result.RPCListener.Webhooks[0].ID) || len(result.RPCListener.Contracts[0].Events[0].Webhooks) != 1 ||
		!canonicalUUID(result.RPCListener.Contracts[0].Events[0].Webhooks[0].ID) {
		t.Fatalf("created response has missing server IDs: %#v", result)
	}
	if response.Header().Get("Location") != "/api/v1/rpc-listeners/"+result.RPCListener.ID {
		t.Fatalf("Location = %q", response.Header().Get("Location"))
	}
	if strings.Contains(response.Body.String(), "rpc-password") || strings.Contains(response.Body.String(), "webhook-secret") || strings.Contains(response.Body.String(), "event-secret") {
		t.Fatalf("creation response exposed a URL credential: %s", response.Body.String())
	}
	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 1 || len(snapshot.Listeners) != 1 || snapshot.Listeners[0].RPCURL != body["rpcUrl"] ||
		len(snapshot.Listeners[0].Contracts) != 1 || snapshot.Listeners[0].Contracts[0].Events[0].Selector != "Transfer()" {
		t.Fatalf("persisted configuration = %#v", snapshot)
	}
}

func TestManagementAPIPersistsAndReturnsSecretReferences(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	body := validListenerCreateBody()
	delete(body, "rpcUrl")
	body["rpcUrlRef"] = "env://CHAIN_RPC_URL"
	body["webhooks"] = []any{map[string]any{"urlRef": "file:///run/secrets/webhook_url", "authentication": map[string]any{"type": "hmac-sha256", "secretRef": "env://WEBHOOK_HMAC_KEY", "keyId": "receiver-1"}}}
	response := postManagement(t, healthHandler(store), secret, "/api/v1/rpc-listeners", `"revision-0"`, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"rpcUrlRef":"env://CHAIN_RPC_URL"`) || !strings.Contains(response.Body.String(), `"urlRef":"file:///run/secrets/webhook_url"`) || !strings.Contains(response.Body.String(), `"secretRef":"env://WEBHOOK_HMAC_KEY"`) {
		t.Fatalf("reference response = %s", response.Body.String())
	}
	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Listeners[0].RPCURL != "" || snapshot.Listeners[0].RPCURLRef != "env://CHAIN_RPC_URL" || snapshot.Listeners[0].Webhooks[0].URL != "" || snapshot.Listeners[0].Webhooks[0].URLRef != "file:///run/secrets/webhook_url" || snapshot.Listeners[0].Webhooks[0].Authentication.SecretRef != "env://WEBHOOK_HMAC_KEY" {
		t.Fatalf("persisted references = %#v", snapshot.Listeners[0])
	}
}

func TestManagementAPIExplainsMissingRPCEncryptionKey(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	body := validListenerCreateBody()
	body["rpcAuthentication"] = map[string]any{
		"type": "provider-jwt", "tokenUrl": "https://provider.example/token",
		"tokenApiKey": "api-key", "secret": "signature",
	}
	response := postManagement(t, healthHandler(store), secret, "/api/v1/rpc-listeners", `"revision-0"`, body)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "rpc_credentials_key_ref") {
		t.Fatalf("create credentialed listener = %d: %s", response.Code, response.Body.String())
	}
}

func TestManagementAPIRejectsAmbiguousSecretReferences(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	body := validListenerCreateBody()
	body["rpcUrlRef"] = "env://CHAIN_RPC_URL"
	response := postManagement(t, healthHandler(store), secret, "/api/v1/rpc-listeners", `"revision-0"`, body)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
}

func TestManagementAPICreatesNestedResourcesAtEveryLevel(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	handler := healthHandler(store)
	top := validListenerCreateBody()
	top["webhooks"] = []any{map[string]any{"url": "https://initial-chain.example.test/hook"}}
	created := postManagement(t, handler, secret, "/api/v1/rpc-listeners", `"revision-0"`, top)
	if created.Code != http.StatusCreated {
		t.Fatalf("create listener: %d %s", created.Code, created.Body.String())
	}
	var topResult struct {
		RPCListener rpcListenerAPIResponse `json:"rpcListener"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &topResult); err != nil {
		t.Fatal(err)
	}
	listenerID := topResult.RPCListener.ID
	revision := uint64(1)

	global := postManagement(t, handler, secret, "/api/v1/rpc-listeners/webhooks", revisionETag(revision), map[string]any{"url": "https://global.example.test/hook"})
	revision++
	globalID := createdResourceID(t, global, "webhook", revision)
	if global.Header().Get("Location") != "/api/v1/rpc-listeners/webhooks/"+globalID {
		t.Fatalf("global Location = %q", global.Header().Get("Location"))
	}

	chain := postManagement(t, handler, secret, "/api/v1/rpc-listeners/"+listenerID+"/webhooks", revisionETag(revision), map[string]any{"url": "https://second-chain.example.test/hook"})
	revision++
	_ = createdResourceID(t, chain, "webhook", revision)

	contract := postManagement(t, handler, secret, "/api/v1/rpc-listeners/"+listenerID+"/contracts", revisionETag(revision), map[string]any{
		"address": "0x0000000000000000000000000000000000000002",
		"abi":     json.RawMessage(`[{"type":"event","name":"Approval","inputs":[]},{"type":"event","name":"Transfer","inputs":[]}]`),
	})
	revision++
	contractID := createdResourceID(t, contract, "contract", revision)

	contractWebhook := postManagement(t, handler, secret, "/api/v1/rpc-listeners/"+listenerID+"/contracts/"+contractID+"/webhooks", revisionETag(revision), map[string]any{"url": "https://contract.example.test/hook"})
	revision++
	_ = createdResourceID(t, contractWebhook, "webhook", revision)

	event := postManagement(t, handler, secret, "/api/v1/rpc-listeners/"+listenerID+"/contracts/"+contractID+"/events", revisionETag(revision), map[string]any{"selector": "Approval()"})
	revision++
	eventID := createdResourceID(t, event, "event", revision)
	var eventResult struct {
		Event eventAPIResponse `json:"event"`
	}
	if err := json.Unmarshal(event.Body.Bytes(), &eventResult); err != nil {
		t.Fatal(err)
	}
	if eventResult.Event.WebhookSource != "contract" || len(eventResult.Event.EffectiveWebhooks) != 1 {
		t.Fatalf("new event inheritance = %#v", eventResult.Event)
	}

	eventWebhook := postManagement(t, handler, secret, "/api/v1/rpc-listeners/"+listenerID+"/contracts/"+contractID+"/events/"+eventID+"/webhooks", revisionETag(revision), map[string]any{"url": "https://event.example.test/hook"})
	revision++
	_ = createdResourceID(t, eventWebhook, "webhook", revision)

	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != revision || len(snapshot.GlobalWebhooks) != 1 || len(snapshot.Listeners[0].Webhooks) != 2 ||
		len(snapshot.Listeners[0].Contracts) != 1 || len(snapshot.Listeners[0].Contracts[0].Webhooks) != 1 ||
		len(snapshot.Listeners[0].Contracts[0].Events) != 1 || len(snapshot.Listeners[0].Contracts[0].Events[0].Webhooks) != 1 {
		t.Fatalf("nested persisted configuration = %#v", snapshot)
	}
	audit, err := store.RPCListenerAudit(context.Background(), 20, 0)
	if err != nil || len(audit) != int(revision) {
		t.Fatalf("nested create audit entries = %d, error %v", len(audit), err)
	}
	for _, entry := range audit {
		if entry.Action != core.AuditActionCreate || entry.ActorName != "create-client" || entry.ActorRole != core.APIKeyAdmin {
			t.Fatalf("nested create audit entry = %#v", entry)
		}
	}
}

func TestManagementAPICreateAuthorizationAndRevisionPreconditions(t *testing.T) {
	readOnlyStore, readOnlySecret := emptyManagementFixture(t, core.APIKeyReadOnly)
	readOnly := postManagement(t, healthHandler(readOnlyStore), readOnlySecret, "/api/v1/rpc-listeners", `"revision-0"`, validListenerCreateBody())
	if readOnly.Code != http.StatusForbidden || readOnly.Body.String() != "{\"error\":\"admin API key required\"}\n" {
		t.Fatalf("read-only create = %d %s", readOnly.Code, readOnly.Body.String())
	}

	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	handler := healthHandler(store)
	tests := []struct {
		name, etag string
		want       int
	}{
		{"missing", "", http.StatusPreconditionRequired},
		{"malformed", "revision-0", http.StatusBadRequest},
		{"weak", `W/"revision-0"`, http.StatusBadRequest},
		{"stale", `"revision-1"`, http.StatusPreconditionFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := postManagement(t, handler, secret, "/api/v1/rpc-listeners", test.etag, validListenerCreateBody())
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.want, response.Body.String())
			}
			if test.want == http.StatusPreconditionFailed && response.Header().Get("ETag") != `"revision-0"` {
				t.Fatalf("conflict ETag = %q", response.Header().Get("ETag"))
			}
		})
	}
	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil || snapshot.Revision != 0 || len(snapshot.Listeners) != 0 {
		t.Fatalf("precondition failures mutated store: %#v, %v", snapshot, err)
	}
}

func TestManagementAPICreateValidatesWholeAggregateAtomically(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"RPC scheme", func(body map[string]any) { body["rpcUrl"] = "ws://rpc.example.test" }},
		{"numeric range", func(body map[string]any) { body["batchSize"] = 0 }},
		{"invalid address", func(body map[string]any) {
			body["contracts"] = []any{map[string]any{"address": "0x1234", "abi": json.RawMessage(`[]`)}}
		}},
		{"missing ABI event", func(body map[string]any) {
			body["webhooks"] = []any{map[string]any{"url": "https://hook.example.test"}}
			body["contracts"] = []any{map[string]any{"address": "0x0000000000000000000000000000000000000001", "abi": json.RawMessage(`[]`), "events": []any{map[string]any{"selector": "Transfer()"}}}}
		}},
		{"anonymous ABI event", func(body map[string]any) {
			body["webhooks"] = []any{map[string]any{"url": "https://hook.example.test"}}
			body["contracts"] = []any{map[string]any{"address": "0x0000000000000000000000000000000000000001", "abi": json.RawMessage(`[{"type":"event","name":"Transfer","anonymous":true,"inputs":[]}]`), "events": []any{map[string]any{"selector": "Transfer()"}}}}
		}},
		{"duplicate ABI event", func(body map[string]any) {
			body["webhooks"] = []any{map[string]any{"url": "https://hook.example.test"}}
			body["contracts"] = []any{map[string]any{"address": "0x0000000000000000000000000000000000000001", "abi": json.RawMessage(`[{"type":"event","name":"Transfer","inputs":[]}]`), "events": []any{map[string]any{"selector": "Transfer()"}, map[string]any{"selector": "Transfer()"}}}}
		}},
		{"missing effective webhook", func(body map[string]any) {
			body["contracts"] = []any{map[string]any{"address": "0x0000000000000000000000000000000000000001", "abi": json.RawMessage(`[{"type":"event","name":"Transfer","inputs":[]}]`), "events": []any{map[string]any{"selector": "Transfer()"}}}}
		}},
		{"invalid webhook URL", func(body map[string]any) { body["webhooks"] = []any{map[string]any{"url": "file:///tmp/hook"}} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
			body := validListenerCreateBody()
			test.mutate(body)
			response := postManagement(t, healthHandler(store), secret, "/api/v1/rpc-listeners", `"revision-0"`, body)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			snapshot, err := store.RPCListenerSnapshot(context.Background())
			if err != nil || snapshot.Revision != 0 || len(snapshot.Listeners) != 0 {
				t.Fatalf("invalid create mutated store: %#v, %v", snapshot, err)
			}
		})
	}
}

func TestManagementAPIInvalidNestedCreateDoesNotAdvanceRevision(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	handler := healthHandler(store)
	body := validListenerCreateBody()
	body["webhooks"] = []any{map[string]any{"url": "https://chain.example.test/hook"}}
	body["contracts"] = []any{map[string]any{"address": "0x0000000000000000000000000000000000000001", "abi": json.RawMessage(`[{"type":"event","name":"Transfer","inputs":[]}]`)}}
	created := postManagement(t, handler, secret, "/api/v1/rpc-listeners", `"revision-0"`, body)
	var result struct {
		RPCListener rpcListenerAPIResponse `json:"rpcListener"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil || created.Code != http.StatusCreated {
		t.Fatalf("create base = %d, %v, %s", created.Code, err, created.Body.String())
	}
	contractID := result.RPCListener.Contracts[0].ID
	response := postManagement(t, handler, secret,
		"/api/v1/rpc-listeners/"+result.RPCListener.ID+"/contracts/"+contractID+"/events",
		`"revision-1"`, map[string]any{"selector": "Approval()"})
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid nested create = %d %s", response.Code, response.Body.String())
	}
	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil || snapshot.Revision != 1 || len(snapshot.Listeners[0].Contracts[0].Events) != 0 {
		t.Fatalf("invalid nested create mutated store: %#v, %v", snapshot, err)
	}
}

func validListenerCreateBody() map[string]any {
	return map[string]any{
		"name": "private", "chainId": 1, "rpcUrl": "https://rpc.example.test", "startBlock": 0,
		"batchSize": 100, "pollInterval": "1s", "confirmations": 0, "reorgDepth": 5,
		"rpcRetryAttempts": 2, "rpcRetryBackoff": "500ms", "rpcTimeout": "5s",
	}
}

func emptyManagementFixture(t *testing.T, role core.APIKeyRole) (*sqlite.Store, string) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "management-create.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	secret, err := auth.GenerateAPIKeySecret()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashAPIKeySecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	key := core.APIKey{ID: core.NewConfigID(), Name: "create-client", Role: role, Prefix: auth.APIKeyPrefix(secret), CreatedAt: time.Now().UTC()}
	if err := store.CreateAPIKey(context.Background(), key, hash); err != nil {
		t.Fatal(err)
	}
	return store, secret
}

func postManagement(t *testing.T, handler http.Handler, secret, path, etag string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Content-Type", "application/json")
	if etag != "" {
		request.Header.Set("If-Match", etag)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func createdResourceID(t *testing.T, response *httptest.ResponseRecorder, field string, revision uint64) string {
	t.Helper()
	if response.Code != http.StatusCreated || response.Header().Get("ETag") != revisionETag(revision) {
		t.Fatalf("create %s = %d, ETag %q, body %s", field, response.Code, response.Header().Get("ETag"), response.Body.String())
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var resource struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result[field], &resource); err != nil || !canonicalUUID(resource.ID) {
		t.Fatalf("created %s id = %q, error %v, body %s", field, resource.ID, err, response.Body.String())
	}
	return resource.ID
}

func managementFixture(t *testing.T, role core.APIKeyRole) (*sqlite.Store, string, core.RPCListener) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "management.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	secret, err := auth.GenerateAPIKeySecret()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashAPIKeySecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	key := core.APIKey{ID: core.NewConfigID(), Name: "test-client", Role: role, Prefix: auth.APIKeyPrefix(secret), CreatedAt: time.Now().UTC()}
	if err := store.CreateAPIKey(ctx, key, hash); err != nil {
		t.Fatal(err)
	}
	config := core.RPCListener{
		ID: core.NewConfigID(), Name: "private", ChainID: 1, RPCURL: "https://rpc-user:rpc-password@rpc.example.test/rpc-path-secret?api_key=secret", BatchSize: 100,
		PollInterval: time.Second, ReorgDepth: 5, RPCRetryAttempts: 2, RPCRetryBackoff: time.Second, RPCTimeout: 5 * time.Second,
		Contracts: []core.ContractConfig{{ID: core.NewConfigID(), Address: "0x0000000000000000000000000000000000000001", ABI: json.RawMessage(`[]`),
			Webhooks: []core.WebhookConfig{{ID: core.NewConfigID(), URL: "https://contract.example/webhook-path-secret?signature=webhook-secret"}},
			Events:   []core.EventConfig{{ID: core.NewConfigID(), Selector: "Transfer()"}},
		}},
	}
	if _, err := store.CreateRPCListener(ctx, config, 0, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return store, secret, config
}

func keyIDForSecret(t *testing.T, store *sqlite.Store) string {
	t.Helper()
	keys, err := store.APIKeys(context.Background())
	if err != nil || len(keys) != 1 {
		t.Fatalf("APIKeys() = %#v, %v", keys, err)
	}
	return keys[0].ID
}
