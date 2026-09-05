package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/rpc"
	"io"
	"net/http"
	"net/http/httptest"
	"reddotrelay/internal/core"
	"strings"
	"testing"
	"time"
)

func TestScannerSkipPreviewConfirmation(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	listener := core.RPCListener{ID: core.NewConfigID(), Name: "skip", ChainID: 1, RPCURL: "http://localhost:8545", StartBlock: 10, BatchSize: 10, PollInterval: time.Second, Confirmations: 2, ReorgDepth: 12, RPCRetryAttempts: 1, RPCRetryBackoff: time.Second, RPCTimeout: time.Second, Paused: true}
	rev, err := store.CreateRPCListener(context.Background(), listener, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	service := newScannerSkipService(store, nil)
	stopped := false
	service.stopped = func(string, uint64) bool { return stopped }
	target := core.CanonicalBlock{ChainID: 1, Number: 100, Hash: "0x" + strings.Repeat("1", 64), ParentHash: "0x" + strings.Repeat("2", 64)}
	service.fetch = func(context.Context, core.RPCListener, *uint64) (core.CanonicalBlock, error) { return target, nil }
	handler := authenticateAPIKey(store, http.HandlerFunc(service.handle))
	path := "/api/v1/rpc-listeners/" + listener.ID + "/skip-to-head"
	post := func(suffix string, body any) int {
		return postManagement(t, handler, secret, path+suffix, revisionETag(rev), body).Code
	}
	if code := post("/preview", map[string]string{}); code != 409 {
		t.Fatalf("running: %d", code)
	}
	stopped = true
	response := postManagement(t, handler, secret, path+"/preview", revisionETag(rev), map[string]string{})
	if response.Code != 200 {
		t.Fatalf("preview: %d %s", response.Code, response.Body.String())
	}
	var preview scannerSkipPreview
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.FromBlock != 10 || preview.Blocks != 91 {
		t.Fatalf("preview: %+v", preview)
	}
	if _, err := store.Checkpoint(context.Background(), 1); err == nil {
		t.Fatal("preview mutated checkpoint")
	}
	if code := post("", map[string]string{"token": preview.Token, "confirmation": "wrong"}); code != 422 {
		t.Fatalf("phrase: %d", code)
	}
	if code := post("", map[string]string{"token": preview.Token + "x", "confirmation": preview.Confirmation}); code != 409 {
		t.Fatalf("tamper: %d", code)
	}
	original := target.Hash
	target.Hash = "0x" + strings.Repeat("3", 64)
	if code := post("", map[string]string{"token": preview.Token, "confirmation": preview.Confirmation}); code != 409 {
		t.Fatalf("reorg: %d", code)
	}
	target.Hash = original
	if code := post("", map[string]string{"token": preview.Token, "confirmation": preview.Confirmation}); code != 200 {
		t.Fatalf("confirm: %d", code)
	}
	cp, err := store.Checkpoint(context.Background(), 1)
	if err != nil || cp.BlockNumber != 100 {
		t.Fatalf("checkpoint: %+v %v", cp, err)
	}
	if code := post("", map[string]string{"token": preview.Token, "confirmation": preview.Confirmation}); code != 412 {
		t.Fatalf("replay: %d", code)
	}
	service.now = func() time.Time { return preview.ExpiresAt }
	if _, err := service.verify(preview.Token); err == nil {
		t.Fatal("expired preview accepted")
	}
	if _, err := newScannerSkipService(store, nil).verify(preview.Token); err == nil {
		t.Fatal("preview survived restart")
	}
}

func TestSkipReaderReusesJWTAndPerCallBudget(t *testing.T) {
	var tokens, calls int
	hash := "0x" + strings.Repeat("1", 64)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			tokens++
			claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
			_ = json.NewEncoder(w).Encode(map[string]string{"data": "header." + claims + ".signature"})
			return
		}
		calls++
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("missing bearer token")
		}
		// Each call fits the configured second, but the sequence does not.
		time.Sleep(350 * time.Millisecond)
		var input struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		var result any = "0x1"
		if input.Method == "eth_getBlockByNumber" {
			var tag string
			_ = json.Unmarshal(input.Params[0], &tag)
			if tag == "latest" {
				tag = "0x64"
			}
			result = map[string]string{"number": tag, "hash": hash, "parentHash": hash}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": input.ID, "result": result})
	}))
	defer server.Close()
	listener := core.RPCListener{ChainID: 1, RPCURL: server.URL, Confirmations: 2, RPCTimeout: time.Second, TLS: core.ListenerTLSConfig{InsecureSkipVerify: true}, RPCAuthentication: core.RPCAuthentication{Type: "provider-jwt", TokenURL: server.URL + "/token", TokenAPIKey: "test-key", Secret: "test-signature"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fetch, closeClient, err := openSkipReader(ctx, listener)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	if block, err := fetch(ctx, listener, nil); err != nil || block.Number != 98 {
		t.Fatalf("head: %+v %v", block, err)
	}
	previous := uint64(10)
	if block, err := fetch(ctx, listener, &previous); err != nil || block.Number != 10 {
		t.Fatalf("checkpoint: %+v %v", block, err)
	}
	if tokens != 1 || calls != 4 {
		t.Fatalf("token calls=%d RPC calls=%d; want 1 and 4", tokens, calls)
	}
}

func TestSkipErrorsAreSpecificAndSecretSafe(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timed out"},
		{rpc.HTTPError{StatusCode: 401, Body: []byte("secret-token")}, "authentication was rejected"},
		{rpc.HTTPError{StatusCode: 429, Body: []byte("secret-token")}, "rate limit"},
		{errors.New("RPC provider JWT token request failed secret-token"), "JWT authentication failed"},
		{errors.New("incomplete canonical block"), "missing or invalid"},
		{errors.New("https://user:secret-token@example.test"), "RPC request failed"},
	} {
		w := httptest.NewRecorder()
		writeSkipRPCError(w, context.Background(), "verifying existing checkpoint block 10", tt.err)
		body := w.Body.String()
		if w.Code != 502 || !strings.Contains(body, tt.want) || strings.Contains(body, "secret-token") || !strings.Contains(body, "block 10") {
			t.Fatalf("unsafe/inaccurate error: %s", body)
		}
	}
}

func TestSkipResponseSurvivesNormalWriteDeadline(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	listener := core.RPCListener{ID: core.NewConfigID(), Name: "skip", ChainID: 1, RPCURL: "http://localhost:8545", StartBlock: 10, BatchSize: 10, PollInterval: time.Second, Confirmations: 2, ReorgDepth: 12, RPCRetryAttempts: 1, RPCRetryBackoff: time.Second, RPCTimeout: time.Second, Paused: true}
	revision, err := store.CreateRPCListener(context.Background(), listener, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	service := newScannerSkipService(store, nil)
	service.stopped = func(string, uint64) bool { return true }
	service.fetch = func(ctx context.Context, _ core.RPCListener, _ *uint64) (core.CanonicalBlock, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 50*time.Second {
			t.Error("operation still uses a short shared deadline")
		}
		time.Sleep(100 * time.Millisecond)
		return core.CanonicalBlock{}, context.DeadlineExceeded
	}
	server := httptest.NewUnstartedServer(authenticateAPIKey(store, http.HandlerFunc(service.handle)))
	server.Config.WriteTimeout = 20 * time.Millisecond
	server.Start()
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/rpc-listeners/"+listener.ID+"/skip-to-head/preview", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("If-Match", revisionETag(revision))
	req.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 502 || !strings.Contains(string(body), "timed out") {
		t.Fatalf("response %s: %v", body, err)
	}
	if _, err := store.Checkpoint(context.Background(), 1); err != core.ErrCheckpointNotFound {
		t.Fatalf("checkpoint changed: %v", err)
	}
	entries, err := store.ScannerSkipAudit(context.Background(), listener.ID, 10, 0)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed verification recorded a skip")
	}
}

func TestSkipHeadUsesConfiguredRPCAndVerifiesNumericTarget(t *testing.T) {
	hash := "0x" + strings.Repeat("1", 64)
	parent := "0x" + strings.Repeat("2", 64)
	wrongNumber := false
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		var result any = "0x1"
		if input.Method == "eth_getBlockByNumber" {
			var tag string
			_ = json.Unmarshal(input.Params[0], &tag)
			number := "0x64"
			if tag != "latest" {
				if tag != "0x62" {
					t.Errorf("unexpected target %s", tag)
				}
				number = "0x62"
			}
			if wrongNumber && tag != "latest" {
				number = "0x63"
			}
			result = map[string]string{"number": number, "hash": hash, "parentHash": parent}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": input.ID, "result": result})
	}))
	defer rpc.Close()
	listener := core.RPCListener{ChainID: 1, RPCURL: rpc.URL, Confirmations: 2}
	target, err := fetchSkipHead(context.Background(), listener, nil)
	if err != nil || target.Number != 98 || target.Hash != hash || target.ParentHash != parent {
		t.Fatalf("target %+v %v", target, err)
	}
	above := uint64(99)
	if _, err := fetchSkipHead(context.Background(), listener, &above); err == nil {
		t.Fatal("unconfirmed target accepted")
	}
	listener.ChainID = 2
	if _, err := fetchSkipHead(context.Background(), listener, nil); err == nil {
		t.Fatal("wrong chain accepted")
	}
	listener.ChainID = 1
	wrongNumber = true
	if _, err := fetchSkipHead(context.Background(), listener, nil); err == nil {
		t.Fatal("wrong number accepted")
	}
}

func TestScannerSkipAuthorizationAndBounds(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyReadOnly)
	service := newScannerSkipService(store, nil)
	handler := authenticateAPIKey(store, http.HandlerFunc(service.handle))
	path := "/api/v1/rpc-listeners/" + core.NewConfigID() + "/skip-to-head/preview"
	response := postManagement(t, handler, secret, path, revisionETag(0), map[string]string{})
	if response.Code != 403 {
		t.Fatalf("read-only: %d", response.Code)
	}
	response = postManagement(t, handler, "", path, revisionETag(0), map[string]string{})
	if response.Code != 401 {
		t.Fatalf("unauthenticated: %d", response.Code)
	}
	previous := &core.Checkpoint{ChainID: 1, BlockNumber: 10, BlockHash: "old"}
	service.fetch = func(context.Context, core.RPCListener, *uint64) (core.CanonicalBlock, error) {
		return core.CanonicalBlock{ChainID: 1, Number: 10, Hash: "new"}, nil
	}
	w := httptest.NewRecorder()
	if service.verifyPrevious(context.Background(), core.RPCListener{ChainID: 1}, previous, w) || w.Code != 409 {
		t.Fatal("orphaned checkpoint accepted")
	}
}
