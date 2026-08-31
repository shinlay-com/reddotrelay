package rpcauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testJWT(expiry time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiry.Unix())))
	return "eyJhbGciOiJSUzI1NiJ9." + payload + ".signature"
}

func TestTransportAppliesConfiguredAuthentication(t *testing.T) {
	tests := []struct {
		name       string
		config     Config
		wantHeader string
		wantValue  string
	}{
		{"basic", Config{Type: TypeBasic, Username: "admin", Secret: "password"}, "Authorization", "Basic YWRtaW46cGFzc3dvcmQ="},
		{"bearer", Config{Type: TypeBearer, Secret: "token"}, "Authorization", "Bearer token"},
		{"header", Config{Type: TypeHeader, HeaderName: "X-API-Key", Secret: "token"}, "X-API-Key", "token"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get(test.wantHeader); got != test.wantValue {
					t.Errorf("%s = %q", test.wantHeader, got)
				}
			}))
			defer server.Close()
			transport, err := NewTransport(nil, test.config)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: transport}
			response, err := client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
		})
	}
}

func TestEngineJWTIsCached(t *testing.T) {
	transport, err := NewTransport(nil, Config{Type: TypeEngineJWT, Secret: strings.Repeat("ab", 32)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000, 0)
	transport.now = func() time.Time { return now }
	first, err := transport.engineJWT()
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(29 * time.Second)
	second, err := transport.engineJWT()
	if err != nil || second != first {
		t.Fatalf("cached JWT = %q, %v", second, err)
	}
	now = now.Add(2 * time.Second)
	third, err := transport.engineJWT()
	if err != nil || third == first {
		t.Fatalf("refreshed JWT = %q, %v", third, err)
	}
}

func TestConfigRejectsUnsupportedOrIncompleteAuthentication(t *testing.T) {
	for _, config := range []Config{{Type: TypeBearer}, {Type: TypeHeader, HeaderName: "bad header", Secret: "x"}, {Type: TypeEngineJWT, Secret: "x"}, {Type: "oauth2", Secret: "x"}} {
		if err := config.Validate(); err == nil {
			t.Errorf("Validate(%#v) succeeded", config)
		}
	}
}

func TestProviderJWTUsesExpectedRequestAndRefreshesAtExpiryMargin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	issued := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/external/generate-access-token" {
			t.Errorf("token request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "api-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("token headers = %#v", r.Header)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["signature"] != "precomputed-signature" || len(body) != 1 {
			t.Errorf("token body = %#v, %v", body, err)
		}
		issued++
		_ = json.NewEncoder(w).Encode(map[string]string{"data": testJWT(now.Add(time.Hour))})
	}))
	defer server.Close()
	transport, err := NewTransport(server.Client().Transport, Config{Type: TypeProviderJWT, TokenURL: server.URL + "/api/external/generate-access-token", TokenAPIKey: "api-key", Secret: "precomputed-signature"})
	if err != nil {
		t.Fatal(err)
	}
	transport.now = func() time.Time { return now }
	first, err := transport.providerJWT(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := transport.providerJWT(t.Context())
	if err != nil || second != first || issued != 1 {
		t.Fatalf("cached token = %q, calls = %d, error = %v", second, issued, err)
	}
	now = now.Add(59 * time.Minute)
	if _, err := transport.providerJWT(t.Context()); err != nil || issued != 2 {
		t.Fatalf("proactive refresh calls = %d, error = %v", issued, err)
	}
}

func TestProviderJWTConcurrentRequestsShareRefresh(t *testing.T) {
	now := time.Now().UTC()
	issued := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		issued++
		_ = json.NewEncoder(w).Encode(map[string]string{"data": testJWT(now.Add(time.Hour))})
	}))
	defer server.Close()
	transport, err := NewTransport(server.Client().Transport, Config{Type: TypeProviderJWT, TokenURL: server.URL, TokenAPIKey: "api-key", Secret: "signature"})
	if err != nil {
		t.Fatal(err)
	}
	transport.now = func() time.Time { return now }
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := transport.providerJWT(t.Context()); err != nil {
				t.Errorf("providerJWT: %v", err)
			}
		}()
	}
	group.Wait()
	if issued != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", issued)
	}
}

func TestProviderJWTRefreshesAndReplaysOnceAfterUnauthorized(t *testing.T) {
	now := time.Now().UTC()
	issued, rpcCalls := 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			issued++
			token := testJWT(now.Add(time.Hour))
			token = strings.TrimSuffix(token, "signature") + fmt.Sprintf("signature%d", issued)
			_ = json.NewEncoder(w).Encode(map[string]string{"data": token})
		case "/rpc":
			rpcCalls++
			if r.Header.Get("Authorization") == "" {
				t.Error("RPC bearer token missing")
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["id"] != float64(1) {
				t.Errorf("RPC replay body = %#v, %v", body, err)
			}
			if rpcCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	transport, err := NewTransport(server.Client().Transport, Config{Type: TypeProviderJWT, TokenURL: server.URL + "/token", TokenAPIKey: "api-key", Secret: "signature"})
	if err != nil {
		t.Fatal(err)
	}
	transport.now = func() time.Time { return now }
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/rpc", strings.NewReader(`{"id":1}`))
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || issued != 2 || rpcCalls != 2 {
		t.Fatalf("status=%d issued=%d RPC calls=%d", response.StatusCode, issued, rpcCalls)
	}
}

func TestProviderJWTRejectsInvalidResponses(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name   string
		status int
		body   any
	}{
		{"non-success", http.StatusUnauthorized, map[string]string{"data": testJWT(now.Add(time.Hour))}},
		{"missing token", http.StatusOK, map[string]string{}},
		{"not JWT", http.StatusOK, map[string]string{"data": "opaque"}},
		{"expired", http.StatusOK, map[string]string{"data": testJWT(now.Add(-time.Minute))}},
		{"expires too soon", http.StatusOK, map[string]string{"data": testJWT(now.Add(30 * time.Second))}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(test.body)
			}))
			defer server.Close()
			transport, err := NewTransport(server.Client().Transport, Config{Type: TypeProviderJWT, TokenURL: server.URL, TokenAPIKey: "api-key", Secret: "signature"})
			if err != nil {
				t.Fatal(err)
			}
			transport.now = func() time.Time { return now }
			if _, err := transport.providerJWT(t.Context()); err == nil {
				t.Fatal("invalid provider response was accepted")
			}
		})
	}
}
