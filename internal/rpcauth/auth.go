// Package rpcauth supplies the outbound HTTP authentication used for EVM RPC
// requests. It deliberately contains no provider-specific OAuth flow.
package rpcauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	TypeNone        = ""
	TypeBasic       = "basic"
	TypeBearer      = "bearer"
	TypeHeader      = "header"
	TypeEngineJWT   = "engine-jwt"
	TypeProviderJWT = "provider-jwt"
)

// Config's Secret is runtime-only. It must never be serialized in an API
// response, audit record, export, metric, or log entry.
type Config struct {
	Type        string
	Username    string
	HeaderName  string
	Secret      string
	TokenURL    string
	TokenAPIKey string
}

func (c Config) Validate() error {
	switch c.Type {
	case TypeNone:
		if c.Username != "" || c.HeaderName != "" || c.Secret != "" || c.TokenURL != "" || c.TokenAPIKey != "" {
			return errors.New("RPC authentication fields require a type")
		}
	case TypeBasic:
		if c.Username == "" || c.Secret == "" {
			return errors.New("RPC Basic authentication requires username and password")
		}
	case TypeBearer:
		if c.Secret == "" {
			return errors.New("RPC bearer authentication requires a token")
		}
	case TypeHeader:
		if !validHeaderName(c.HeaderName) || c.Secret == "" {
			return errors.New("RPC header authentication requires a valid header name and value")
		}
	case TypeEngineJWT:
		key, err := hex.DecodeString(strings.TrimPrefix(c.Secret, "0x"))
		if err != nil || len(key) != 32 {
			return errors.New("RPC Engine JWT secret must be 32-byte hexadecimal")
		}
	case TypeProviderJWT:
		parsed, err := url.Parse(c.TokenURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || c.Secret == "" || c.TokenAPIKey == "" {
			return errors.New("RPC provider JWT requires HTTPS token URL, signature, and API key")
		}
	default:
		return fmt.Errorf("unsupported RPC authentication type %q", c.Type)
	}
	return nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}

// Transport keeps normal bearer/header/basic credentials inexpensive and
// caches an Engine API JWT for 30 seconds rather than signing per RPC call.
type Transport struct {
	base     http.RoundTripper
	config   Config
	now      func() time.Time
	mu       sync.Mutex
	jwt      string
	jwtUntil time.Time
	client   *http.Client
}

func NewTransport(base http.RoundTripper, config Config) (*Transport, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{base: base, config: config, now: time.Now, client: &http.Client{Transport: base, Timeout: 10 * time.Second}}, nil
}

func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	switch t.config.Type {
	case TypeBasic:
		clone.SetBasicAuth(t.config.Username, t.config.Secret)
	case TypeBearer:
		clone.Header.Set("Authorization", "Bearer "+t.config.Secret)
	case TypeHeader:
		clone.Header.Set(t.config.HeaderName, t.config.Secret)
	case TypeEngineJWT:
		token, err := t.engineJWT()
		if err != nil {
			return nil, err
		}
		clone.Header.Set("Authorization", "Bearer "+token)
	case TypeProviderJWT:
		token, err := t.providerJWT(clone.Context())
		if err != nil {
			return nil, err
		}
		clone.Header.Set("Authorization", "Bearer "+token)
		response, err := t.base.RoundTrip(clone)
		if err != nil || response.StatusCode != http.StatusUnauthorized || request.GetBody == nil {
			return response, err
		}
		_ = response.Body.Close()
		t.invalidateProviderJWT(token)
		body, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		retry := request.Clone(request.Context())
		retry.Body = body
		fresh, err := t.providerJWT(retry.Context())
		if err != nil {
			return nil, err
		}
		retry.Header.Set("Authorization", "Bearer "+fresh)
		return t.base.RoundTrip(retry)
	}
	return t.base.RoundTrip(clone)
}

func (t *Transport) invalidateProviderJWT(token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.jwt == token {
		t.jwt, t.jwtUntil = "", time.Time{}
	}
}

func (t *Transport) providerJWT(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now().UTC()
	if t.jwt != "" && now.Before(t.jwtUntil) {
		return t.jwt, nil
	}
	body, _ := json.Marshal(map[string]string{"signature": t.config.Secret})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.config.TokenURL, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", t.config.TokenAPIKey)
	response, err := t.client.Do(request)
	if err != nil {
		return "", errors.New("RPC provider JWT token request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("RPC provider JWT token request returned HTTP %d", response.StatusCode)
	}
	var result struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil || result.Data == "" {
		return "", errors.New("RPC provider JWT token response is invalid")
	}
	expires, err := jwtExpiry(result.Data)
	if err != nil {
		return "", err
	}
	if !expires.After(now) {
		return "", errors.New("RPC provider token is expired")
	}
	margin := time.Minute
	if expires.Sub(now) > 2*time.Hour {
		margin = 5 * time.Minute
	}
	t.jwt, t.jwtUntil = result.Data, expires.Add(-margin)
	if !t.jwtUntil.After(now) {
		return "", errors.New("RPC provider token expires too soon")
	}
	return t.jwt, nil
}

func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("RPC provider token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, errors.New("RPC provider token payload is invalid")
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, errors.New("RPC provider token has no expiry")
	}
	return time.Unix(claims.Exp, 0).UTC(), nil
}

func (t *Transport) engineJWT() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now().UTC()
	if t.jwt != "" && now.Before(t.jwtUntil) {
		return t.jwt, nil
	}
	key, err := hex.DecodeString(strings.TrimPrefix(t.config.Secret, "0x"))
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"iat":%d}`, now.Unix())))
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(header + "." + payload))
	t.jwt = header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	t.jwtUntil = now.Add(30 * time.Second)
	return t.jwt, nil
}
