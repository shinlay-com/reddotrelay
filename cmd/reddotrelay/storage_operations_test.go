package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStorageAndRetentionOperationsRequireAPIAndConfirmation(t *testing.T) {
	store, secret, _ := managementFixture(t, "admin")
	handler := healthHandler(store)
	if response := authenticatedRequest(handler, secret, http.MethodGet, "/api/v1/rpc-listeners"); response.Code != http.StatusOK {
		t.Fatalf("canonical RPC listener API = %d %s", response.Code, response.Body.String())
	}
	for _, path := range []string{"/api/v1/storage/status", "/api/v1/retention/status"} {
		if response := authenticatedRequest(handler, "", http.MethodGet, path); response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s = %d", path, response.Code)
		}
		if response := authenticatedRequest(handler, secret, http.MethodGet, path); response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, response.Code, response.Body.String())
		}
	}
	preview := operationRequest(t, handler, secret, "/api/v1/retention/preview", map[string]any{"olderThan": "720h", "batchSize": 100})
	if preview.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", preview.Code, preview.Body.String())
	}
	refused := operationRequest(t, handler, secret, "/api/v1/retention/prune", map[string]any{"olderThan": "720h", "confirm": false})
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed prune = %d", refused.Code)
	}
	optimize := operationRequest(t, handler, secret, "/api/v1/storage/optimize", map[string]any{"confirm": true})
	if optimize.Code != http.StatusNoContent {
		t.Fatalf("optimize = %d %s", optimize.Code, optimize.Body.String())
	}
}

func TestBuildInfoRequiresAuthentication(t *testing.T) {
	store, secret, _ := managementFixture(t, "admin")
	handler := healthHandler(store)
	if response := authenticatedRequest(handler, "", http.MethodGet, "/api/v1/build-info"); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated build info = %d %s", response.Code, response.Body.String())
	}
	if response := authenticatedRequest(handler, secret, http.MethodGet, "/api/v1/build-info"); response.Code != http.StatusOK {
		t.Fatalf("authenticated build info = %d %s", response.Code, response.Body.String())
	}
}

func TestManagementAPIRoutesRequireAuthentication(t *testing.T) {
	store, _, _ := managementFixture(t, "admin")
	handler := healthHandler(store)
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/users"},
		{http.MethodPost, "/api/v1/users/user-1/disable"},
		{http.MethodGet, "/api/v1/api-keys"},
		{http.MethodPost, "/api/v1/api-keys/key-1/revoke"},
		{http.MethodPost, "/api/v1/connection-tests/rpc"},
		{http.MethodPost, "/api/v1/connection-tests/webhook"},
		{http.MethodGet, "/api/v1/rpc-listeners"},
		{http.MethodGet, "/api/v1/rpc-listeners/listener-1"},
		{http.MethodGet, "/api/v1/rpc-listener-status"},
		{http.MethodGet, "/api/v1/rpc-listener-export"},
		{http.MethodPost, "/api/v1/rpc-listener-import"},
		{http.MethodGet, "/api/v1/rpc-listener-audit"},
		{http.MethodGet, "/api/v1/operational-events"},
		{http.MethodGet, "/api/v1/operational-summary"},
		{http.MethodGet, "/api/v1/events"},
		{http.MethodGet, "/api/v1/events/event-1/deliveries"},
		{http.MethodPost, "/api/v1/deliveries/delivery-1/requeue"},
		{http.MethodGet, "/api/v1/delivery-requeue-audit"},
		{http.MethodGet, "/api/v1/scanner-progress"},
		{http.MethodGet, "/api/v1/build-info"},
		{http.MethodGet, "/api/v1/storage/status"},
		{http.MethodPost, "/api/v1/storage/optimize"},
		{http.MethodGet, "/api/v1/retention/status"},
		{http.MethodPost, "/api/v1/retention/preview"},
		{http.MethodPost, "/api/v1/retention/prune"},
		{http.MethodPost, "/api/v1/retention/config"},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnauthorized, response.Body.String())
			}
		})
	}
}

func operationRequest(t *testing.T, handler http.Handler, secret, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
