package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

const testUIUsername = "session-admin"
const testUIPassword = "correct-horse-battery-staple"

func userSessionFixture(t *testing.T, role core.APIKeyRole) (*uiSessionManager, http.Handler) {
	t.Helper()
	store, _ := emptyManagementFixture(t, core.APIKeyReadOnly)
	if role == core.APIKeyReadOnly {
		if _, err := store.CreateInitialAdmin(context.Background(), "bootstrap-admin", testUIPassword, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateUser(context.Background(), testUIUsername, testUIPassword, core.APIKeyReadOnly, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	} else if _, err := store.CreateInitialAdmin(context.Background(), testUIUsername, testUIPassword, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	sessions := newUISessionManager(store, false)
	return sessions, healthHandlerWithSessions(store, nil, nil, "", sessions)
}

func TestUISessionUserLoginCookieManagementAndLogout(t *testing.T) {
	_, handler := userSessionFixture(t, core.APIKeyAdmin)
	login := uiSessionRequest(t, handler, http.MethodPost, "/api/v1/ui-session", `{"username":"`+testUIUsername+`","password":"`+testUIPassword+`"}`, nil, "http://example.test", "")
	if login.Code != http.StatusCreated {
		t.Fatalf("login = %d %s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != uiSessionCookieName || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Secure {
		t.Fatalf("session cookies = %#v", cookies)
	}
	var session uiSessionResponse
	if err := json.Unmarshal(login.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.Name != testUIUsername || session.Role != core.APIKeyAdmin || session.CSRFToken == "" || session.ExpiresAt.IsZero() {
		t.Fatalf("session response = %#v", session)
	}
	status := uiSessionRequest(t, handler, http.MethodGet, "/api/v1/ui-session", "", cookies[0], "", "")
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
	wrongOrigin := uiSessionRequest(t, handler, http.MethodDelete, "/api/v1/ui-session", "", cookies[0], "https://attacker.test", session.CSRFToken)
	if wrongOrigin.Code != http.StatusForbidden {
		t.Fatalf("cross-origin logout = %d", wrongOrigin.Code)
	}
	logout := uiSessionRequest(t, handler, http.MethodDelete, "/api/v1/ui-session", "", cookies[0], "http://example.test", session.CSRFToken)
	if logout.Code != http.StatusNoContent || len(logout.Result().Cookies()) != 1 || logout.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("logout = %d, cookies %#v", logout.Code, logout.Result().Cookies())
	}
	if after := uiSessionRequest(t, handler, http.MethodGet, "/api/v1/ui-session", "", cookies[0], "", ""); after.Code != http.StatusUnauthorized {
		t.Fatalf("status after logout = %d", after.Code)
	}
}

func TestUISessionRejectsAPIKeyLogin(t *testing.T) {
	_, handler := userSessionFixture(t, core.APIKeyAdmin)
	login := uiSessionRequest(t, handler, http.MethodPost, "/api/v1/ui-session", `{"apiKey":"api_key_legacy"}`, nil, "http://example.test", "")
	if login.Code != http.StatusBadRequest {
		t.Fatalf("legacy API-key UI login = %d %s", login.Code, login.Body.String())
	}
}

func TestUISessionExpiryAndSecureConfiguration(t *testing.T) {
	sessions, handler := userSessionFixture(t, core.APIKeyReadOnly)
	baseTime := time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC)
	sessions.now = func() time.Time { return baseTime }
	token, session, err := sessions.createUser(context.Background(), testUIUsername, testUIPassword)
	if err != nil {
		t.Fatal(err)
	}
	sessions.now = func() time.Time { return session.CreatedAt.Add(uiSessionIdleTTL) }
	if _, err := sessions.authenticate(context.Background(), token); err == nil {
		t.Fatal("idle UI session did not expire")
	}
	missingOrigin := uiSessionRequest(t, handler, http.MethodPost, "/api/v1/ui-session", `{"username":"`+testUIUsername+`","password":"`+testUIPassword+`"}`, nil, "", "")
	if missingOrigin.Code != http.StatusForbidden {
		t.Fatalf("login without origin = %d", missingOrigin.Code)
	}
}

func TestUISessionLoginThrottlesFailuresAndRecovers(t *testing.T) {
	sessions, handler := userSessionFixture(t, core.APIKeyAdmin)
	now := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	sessions.now = func() time.Time { return now }
	wrong := `{"username":"` + testUIUsername + `","password":"wrong-password"}`
	for attempt := 0; attempt < uiAuthFailureLimit; attempt++ {
		response := uiSessionRequest(t, handler, http.MethodPost, "/api/v1/ui-session", wrong, nil, "http://example.test", "")
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("failed login %d = %d %s", attempt+1, response.Code, response.Body.String())
		}
	}
	limited := uiSessionRequest(t, handler, http.MethodPost, "/api/v1/ui-session", wrong, nil, "http://example.test", "")
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "900" {
		t.Fatalf("limited login = %d Retry-After=%q body=%s", limited.Code, limited.Header().Get("Retry-After"), limited.Body.String())
	}

	now = now.Add(uiAuthFailureWindow)
	success := uiSessionRequest(t, handler, http.MethodPost, "/api/v1/ui-session", `{"username":"`+testUIUsername+`","password":"`+testUIPassword+`"}`, nil, "http://example.test", "")
	if success.Code != http.StatusCreated {
		t.Fatalf("login after throttle expiry = %d %s", success.Code, success.Body.String())
	}
	if retryAfter := sessions.loginFailures.retryAfter(authFailureKey(testUIUsername), now); retryAfter != 0 {
		t.Fatalf("successful login did not reset failures: %s", retryAfter)
	}
}

func TestUISetupThrottlesInvalidAttempts(t *testing.T) {
	store, _ := emptyManagementFixture(t, core.APIKeyReadOnly)
	sessions := newUISessionManager(store, false)
	now := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	sessions.now = func() time.Time { return now }
	handler := healthHandlerWithSessions(store, nil, nil, "", sessions)
	invalid := `{"username":"admin","password":"short"}`
	for attempt := 0; attempt < uiAuthFailureLimit; attempt++ {
		response := uiSessionRequest(t, handler, http.MethodPost, "/api/v1/ui-setup", invalid, nil, "http://example.test", "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid setup %d = %d %s", attempt+1, response.Code, response.Body.String())
		}
	}
	limited := uiSessionRequest(t, handler, http.MethodPost, "/api/v1/ui-setup", invalid, nil, "http://example.test", "")
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "900" {
		t.Fatalf("limited setup = %d Retry-After=%q", limited.Code, limited.Header().Get("Retry-After"))
	}

	now = now.Add(uiAuthFailureWindow)
	valid := `{"username":"admin","password":"` + testUIPassword + `"}`
	created := uiSessionRequest(t, handler, http.MethodPost, "/api/v1/ui-setup", valid, nil, "http://example.test", "")
	if created.Code != http.StatusCreated {
		t.Fatalf("setup after throttle expiry = %d %s", created.Code, created.Body.String())
	}
}

func TestUIAuthenticationLimiterIsBounded(t *testing.T) {
	limiter := newAuthFailureLimiter()
	now := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	for index := 0; index <= uiAuthFailureMaximum; index++ {
		limiter.record(authFailureKey(fmt.Sprintf("user-%d", index)), now.Add(time.Duration(index)*time.Nanosecond))
	}
	if len(limiter.failures) != uiAuthFailureMaximum {
		t.Fatalf("failure limiter entries = %d, want %d", len(limiter.failures), uiAuthFailureMaximum)
	}
}

func TestUIAuthenticationLimiterCountsConcurrentFailures(t *testing.T) {
	limiter := newAuthFailureLimiter()
	now := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	key := authFailureKey(testUIUsername)
	var group sync.WaitGroup
	for index := 0; index < 100; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			limiter.record(key, now)
		}()
	}
	group.Wait()
	if failure := limiter.failures[key]; failure.count != 100 {
		t.Fatalf("concurrent failure count = %d, want 100", failure.count)
	}
	if retryAfter := limiter.retryAfter(key, now); retryAfter != uiAuthFailureWindow {
		t.Fatalf("concurrent throttle retry = %s, want %s", retryAfter, uiAuthFailureWindow)
	}
}

func uiSessionRequest(t *testing.T, handler http.Handler, method, path, body string, cookie *http.Cookie, origin, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func uiSessionJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func uiSessionMutation(t *testing.T, handler http.Handler, method, path, body string, cookie *http.Cookie, csrf, etag string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://example.test")
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("If-Match", etag)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
