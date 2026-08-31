package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

type connectionTestResolver struct{ values map[string]string }

func (resolver connectionTestResolver) Resolve(_ context.Context, reference string) (string, error) {
	return resolver.values[reference], nil
}

func TestRPCConnectionTestReturnsChainIDWithoutURL(t *testing.T) {
	rpc := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input["method"] != "eth_chainId" {
			t.Fatalf("method = %#v", input["method"])
		}
		writeJSON(writer, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": input["id"], "result": "0x7a69"})
	}))
	defer rpc.Close()

	request := adminConnectionTestRequest(t, `{"rpcUrl":"`+rpc.URL+`"}`)
	response := httptest.NewRecorder()
	handleRPCConnectionTest(connectionTestResolver{}, response, request)
	if response.Code != http.StatusOK || response.Body.String() != "{\"chainId\":31337,\"reachable\":true}\n" {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), rpc.URL) {
		t.Fatal("RPC test response disclosed URL")
	}
}

func TestWebhookConnectionTestSignsSyntheticPayloadWithoutDisclosingSecret(t *testing.T) {
	secret := "connection-test-secret"
	receiver := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.Type != "reddotrelay.test" {
			t.Fatalf("payload = %s, err %v", body, err)
		}
		timestamp := request.Header.Get("RedDotRelay-Timestamp")
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(timestamp + "."))
		_, _ = mac.Write(body)
		if request.Header.Get("RedDotRelay-Signature") != "v1="+hex.EncodeToString(mac.Sum(nil)) || request.Header.Get("RedDotRelay-Key-Id") != "test-key" {
			t.Fatalf("signature headers = %#v", request.Header)
		}
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer receiver.Close()

	resolver := connectionTestResolver{values: map[string]string{"env://DESTINATION": receiver.URL, "env://HMAC_KEY": secret}}
	request := adminConnectionTestRequest(t, `{"urlRef":"env://DESTINATION","authentication":{"type":"hmac-sha256","secretRef":"env://HMAC_KEY","keyId":"test-key"}}`)
	response := httptest.NewRecorder()
	handleWebhookConnectionTest(resolver, connectionTestHTTPClient(), func() time.Time { return time.Unix(123, 0) }, response, request)
	if response.Code != http.StatusOK || response.Body.String() != "{\"accepted\":true,\"reachable\":true,\"statusCode\":202}\n" {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) || strings.Contains(response.Body.String(), receiver.URL) {
		t.Fatal("webhook test response disclosed protected data")
	}
}

func TestConnectionTestsRejectAmbiguousLocatorAndReadOnlyRole(t *testing.T) {
	request := adminConnectionTestRequest(t, `{"rpcUrl":"https://rpc.example","rpcUrlRef":"env://RPC_URL"}`)
	response := httptest.NewRecorder()
	handleRPCConnectionTest(connectionTestResolver{}, response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("ambiguous locator = %d", response.Code)
	}
	request = request.WithContext(context.WithValue(request.Context(), apiKeyPrincipalContextKey{}, core.APIKeyPrincipal{Role: core.APIKeyReadOnly}))
	response = httptest.NewRecorder()
	handleRPCConnectionTest(connectionTestResolver{}, response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("read-only test = %d", response.Code)
	}
}

func adminConnectionTestRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/connection-tests/rpc", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request.WithContext(context.WithValue(request.Context(), apiKeyPrincipalContextKey{}, core.APIKeyPrincipal{Role: core.APIKeyAdmin}))
}
