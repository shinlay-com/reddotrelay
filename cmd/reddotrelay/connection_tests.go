package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/secrets"
)

const connectionTestTimeout = 10 * time.Second

type rpcConnectionTestRequest struct {
	RPCURL            string                   `json:"rpcUrl"`
	RPCURLRef         string                   `json:"rpcUrlRef"`
	RPCAuthentication rpcAuthenticationRequest `json:"rpcAuthentication,omitempty"`
	TLS               listenerTLSCreateRequest `json:"tls,omitempty"`
}

type webhookConnectionTestRequest struct {
	URL            string                       `json:"url"`
	URLRef         string                       `json:"urlRef"`
	Authentication webhookAuthenticationRequest `json:"authentication,omitempty"`
}

func handleRPCConnectionTest(resolver secretValueResolver, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireAdmin(writer, request) {
		return
	}
	var input rpcConnectionTestRequest
	if !decodeCreateRequest(writer, request, &input) {
		return
	}
	rpcURL, err := resolveTestLocator(request.Context(), resolver, input.RPCURL, input.RPCURLRef)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), connectionTestTimeout)
	defer cancel()
	authentication := core.RPCAuthentication{Type: input.RPCAuthentication.Type, Username: input.RPCAuthentication.Username, HeaderName: input.RPCAuthentication.HeaderName, Secret: input.RPCAuthentication.Secret, TokenURL: input.RPCAuthentication.TokenURL, TokenAPIKey: input.RPCAuthentication.TokenAPIKey}
	client, err := dialListenerRPC(ctx, rpcURL, core.ListenerTLSConfig{CAPEM: input.TLS.CAPEM, ServerName: input.TLS.ServerName, InsecureSkipVerify: input.TLS.InsecureSkipVerify}, authentication)
	if err != nil {
		writeAPIError(writer, http.StatusBadGateway, "RPC endpoint is unreachable")
		return
	}
	defer client.Close()
	chainID, err := client.ChainID(ctx)
	if err != nil || chainID == nil || chainID.Sign() <= 0 || !chainID.IsUint64() {
		writeAPIError(writer, http.StatusBadGateway, "RPC endpoint did not return a valid chain ID")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"reachable": true, "chainId": chainID.Uint64()})
}

func handleWebhookConnectionTest(resolver secretValueResolver, client *http.Client, now func() time.Time, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireAdmin(writer, request) {
		return
	}
	var input webhookConnectionTestRequest
	if !decodeCreateRequest(writer, request, &input) {
		return
	}
	destination, err := resolveTestLocator(request.Context(), resolver, input.URL, input.URLRef)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if input.Authentication.Type != "" && input.Authentication.Type != "hmac-sha256" {
		writeAPIError(writer, http.StatusUnprocessableEntity, "unsupported webhook authentication")
		return
	}
	if input.Authentication.Type == "hmac-sha256" && input.Authentication.SecretRef == "" {
		writeAPIError(writer, http.StatusUnprocessableEntity, "webhook HMAC secret reference is required")
		return
	}
	if input.Authentication.SecretRef != "" {
		if err := secrets.ValidateReference(input.Authentication.SecretRef); err != nil {
			writeAPIError(writer, http.StatusUnprocessableEntity, "webhook HMAC secret reference is invalid")
			return
		}
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
		return
	}
	sentAt := now().UTC()
	payload, _ := json.Marshal(map[string]any{"type": "reddotrelay.test", "sentAt": sentAt, "nonce": hex.EncodeToString(nonce)})
	ctx, cancel := context.WithTimeout(request.Context(), connectionTestTimeout)
	defer cancel()
	outbound, err := http.NewRequestWithContext(ctx, http.MethodPost, destination, bytes.NewReader(payload))
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, "webhook destination is invalid")
		return
	}
	outbound.Header.Set("Content-Type", "application/json")
	outbound.Header.Set("Idempotency-Key", "test-"+hex.EncodeToString(nonce))
	if input.Authentication.Type == "hmac-sha256" {
		secret, err := resolver.Resolve(ctx, input.Authentication.SecretRef)
		if err != nil {
			writeAPIError(writer, http.StatusUnprocessableEntity, "webhook HMAC secret is unavailable")
			return
		}
		timestamp := strconv.FormatInt(sentAt.Unix(), 10)
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(timestamp + "."))
		_, _ = mac.Write(payload)
		outbound.Header.Set("RedDotRelay-Timestamp", timestamp)
		outbound.Header.Set("RedDotRelay-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
		if input.Authentication.KeyID != "" {
			outbound.Header.Set("RedDotRelay-Key-Id", input.Authentication.KeyID)
		}
	}
	response, err := client.Do(outbound)
	if err != nil {
		writeAPIError(writer, http.StatusBadGateway, "webhook destination is unreachable")
		return
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	writeJSON(writer, http.StatusOK, map[string]any{"reachable": true, "accepted": response.StatusCode >= 200 && response.StatusCode < 300, "statusCode": response.StatusCode})
}

func resolveTestLocator(ctx context.Context, resolver secretValueResolver, direct, reference string) (string, error) {
	if (direct == "") == (reference == "") {
		return "", errors.New("exactly one direct URL or secret reference is required")
	}
	if reference != "" {
		if err := secrets.ValidateReference(reference); err != nil {
			return "", errors.New("secret reference is invalid")
		}
		resolved, err := resolver.Resolve(ctx, reference)
		if err != nil {
			return "", errors.New("referenced URL is unavailable")
		}
		direct = resolved
	}
	if !validAbsoluteHTTPURL(direct) {
		return "", errors.New("URL must be an absolute HTTP(S) URL")
	}
	return direct, nil
}

func connectionTestHTTPClient() *http.Client {
	return &http.Client{Timeout: connectionTestTimeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}
