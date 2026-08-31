package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
)

const (
	uiSessionCookieName  = "reddotrelay_ui_session"
	uiSessionIdleTTL     = 30 * time.Minute
	uiSessionAbsoluteTTL = 8 * time.Hour
	uiSessionMaximum     = 1024
	uiAuthFailureLimit   = 5
	uiAuthFailureWindow  = 15 * time.Minute
	uiAuthFailureMaximum = 2048
)

var errInvalidUISession = errors.New("invalid UI session")

type uiSessionAuthenticator interface {
	AuthenticateUser(context.Context, string, string, time.Time) (core.APIKeyPrincipal, error)
	ActiveUserPrincipal(context.Context, string) (core.APIKeyPrincipal, error)
	SaveUISession(context.Context, core.UISessionRecord) error
	UISession(context.Context, []byte) (core.UISessionRecord, error)
	DeleteUISession(context.Context, []byte) error
}

type uiSession struct {
	Principal core.APIKeyPrincipal
	CSRFToken string
	CreatedAt time.Time
	LastSeen  time.Time
}

type uiSessionManager struct {
	authenticator uiSessionAuthenticator
	secureCookies bool
	now           func() time.Time
	mu            sync.Mutex
	sessions      map[[sha256.Size]byte]uiSession
	loginFailures *authFailureLimiter
	setupFailures *authFailureLimiter
}

type authFailure struct {
	count     int
	firstSeen time.Time
}

type authFailureLimiter struct {
	mu       sync.Mutex
	failures map[[sha256.Size]byte]authFailure
}

type uiSessionResponse struct {
	Name      string          `json:"name"`
	Role      core.APIKeyRole `json:"role"`
	CSRFToken string          `json:"csrfToken"`
	ExpiresAt time.Time       `json:"expiresAt"`
}

func newUISessionManager(authenticator uiSessionAuthenticator, secureCookies bool) *uiSessionManager {
	return &uiSessionManager{
		authenticator: authenticator,
		secureCookies: secureCookies,
		now:           func() time.Time { return time.Now().UTC() },
		sessions:      make(map[[sha256.Size]byte]uiSession),
		loginFailures: newAuthFailureLimiter(),
		setupFailures: newAuthFailureLimiter(),
	}
}

func newAuthFailureLimiter() *authFailureLimiter {
	return &authFailureLimiter{failures: make(map[[sha256.Size]byte]authFailure)}
}

func authFailureKey(value string) [sha256.Size]byte {
	return sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(value))))
}

func (limiter *authFailureLimiter) retryAfter(key [sha256.Size]byte, now time.Time) time.Duration {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	limiter.deleteExpiredLocked(now)
	failure, ok := limiter.failures[key]
	if !ok || failure.count < uiAuthFailureLimit {
		return 0
	}
	return failure.firstSeen.Add(uiAuthFailureWindow).Sub(now)
}

func (limiter *authFailureLimiter) record(key [sha256.Size]byte, now time.Time) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	limiter.deleteExpiredLocked(now)
	if len(limiter.failures) >= uiAuthFailureMaximum {
		limiter.evictOldestLocked()
	}
	failure, ok := limiter.failures[key]
	if !ok {
		failure.firstSeen = now
	}
	failure.count++
	limiter.failures[key] = failure
}

func (limiter *authFailureLimiter) reset(key [sha256.Size]byte) {
	limiter.mu.Lock()
	delete(limiter.failures, key)
	limiter.mu.Unlock()
}

func (limiter *authFailureLimiter) deleteExpiredLocked(now time.Time) {
	for key, failure := range limiter.failures {
		if !now.Before(failure.firstSeen.Add(uiAuthFailureWindow)) {
			delete(limiter.failures, key)
		}
	}
}

func (limiter *authFailureLimiter) evictOldestLocked() {
	var oldestKey [sha256.Size]byte
	var oldestTime time.Time
	found := false
	for key, failure := range limiter.failures {
		if !found || failure.firstSeen.Before(oldestTime) {
			oldestKey = key
			oldestTime = failure.firstSeen
			found = true
		}
	}
	if found {
		delete(limiter.failures, oldestKey)
	}
}

func writeAuthenticationRateLimit(writer http.ResponseWriter, retryAfter time.Duration) {
	seconds := int64((retryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	writer.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	writeAPIError(writer, http.StatusTooManyRequests, "too many authentication attempts")
}

func (manager *uiSessionManager) createUser(ctx context.Context, username, password string) (string, uiSession, error) {
	now := manager.now()
	principal, err := manager.authenticator.AuthenticateUser(ctx, username, password, now)
	if err != nil {
		return "", uiSession{}, err
	}
	token, err := randomURLToken()
	if err != nil {
		return "", uiSession{}, err
	}
	csrf, err := randomURLToken()
	if err != nil {
		return "", uiSession{}, err
	}
	session := uiSession{Principal: principal, CSRFToken: csrf, CreatedAt: now, LastSeen: now}
	manager.mu.Lock()
	manager.deleteExpiredLocked(now)
	if len(manager.sessions) >= uiSessionMaximum {
		manager.evictOldestLocked()
	}
	manager.sessions[sha256.Sum256([]byte(token))] = session
	manager.mu.Unlock()
	hash := sha256.Sum256([]byte(token))
	if err := manager.authenticator.SaveUISession(ctx, core.UISessionRecord{TokenHash: hash[:], PrincipalID: principal.ID, CSRFToken: csrf, CreatedAt: now, LastSeen: now}); err != nil {
		return "", uiSession{}, err
	}
	return token, session, nil
}

func (manager *uiSessionManager) authenticate(ctx context.Context, token string) (uiSession, error) {
	if token == "" {
		return uiSession{}, errInvalidUISession
	}
	hash := sha256.Sum256([]byte(token))
	now := manager.now()
	manager.mu.Lock()
	session, ok := manager.sessions[hash]
	if !ok {
		manager.mu.Unlock()
		record, loadErr := manager.authenticator.UISession(ctx, hash[:])
		if loadErr != nil {
			return uiSession{}, errInvalidUISession
		}
		session = uiSession{Principal: core.APIKeyPrincipal{ID: record.PrincipalID}, CSRFToken: record.CSRFToken, CreatedAt: record.CreatedAt, LastSeen: record.LastSeen}
		manager.mu.Lock()
		manager.sessions[hash] = session
		ok = true
	}
	if !ok || manager.expired(session, now) {
		delete(manager.sessions, hash)
		manager.mu.Unlock()
		return uiSession{}, errInvalidUISession
	}
	manager.mu.Unlock()

	principal, err := manager.authenticator.ActiveUserPrincipal(ctx, session.Principal.ID)
	if err != nil {
		manager.mu.Lock()
		delete(manager.sessions, hash)
		manager.mu.Unlock()
		if errors.Is(err, sqlite.ErrInvalidUserCredentials) {
			return uiSession{}, errInvalidUISession
		}
		return uiSession{}, err
	}

	manager.mu.Lock()
	current, ok := manager.sessions[hash]
	if !ok || manager.expired(current, now) {
		delete(manager.sessions, hash)
		manager.mu.Unlock()
		return uiSession{}, errInvalidUISession
	}
	current.Principal = principal
	current.LastSeen = now
	manager.sessions[hash] = current
	manager.mu.Unlock()
	_ = manager.authenticator.SaveUISession(ctx, core.UISessionRecord{TokenHash: hash[:], PrincipalID: current.Principal.ID, CSRFToken: current.CSRFToken, CreatedAt: current.CreatedAt, LastSeen: current.LastSeen})
	return current, nil
}

func (manager *uiSessionManager) destroy(token string) {
	hash := sha256.Sum256([]byte(token))
	manager.mu.Lock()
	delete(manager.sessions, hash)
	manager.mu.Unlock()
	_ = manager.authenticator.DeleteUISession(context.Background(), hash[:])
}

func (manager *uiSessionManager) expired(session uiSession, now time.Time) bool {
	return !now.Before(session.CreatedAt.Add(uiSessionAbsoluteTTL)) || !now.Before(session.LastSeen.Add(uiSessionIdleTTL))
}

func (manager *uiSessionManager) deleteExpiredLocked(now time.Time) {
	for key, session := range manager.sessions {
		if manager.expired(session, now) {
			delete(manager.sessions, key)
		}
	}
}

func (manager *uiSessionManager) evictOldestLocked() {
	var oldestKey [sha256.Size]byte
	var oldestTime time.Time
	found := false
	for key, session := range manager.sessions {
		if !found || session.LastSeen.Before(oldestTime) {
			oldestKey = key
			oldestTime = session.LastSeen
			found = true
		}
	}
	if found {
		delete(manager.sessions, oldestKey)
	}
}

func randomURLToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func handleUISession(manager *uiSessionManager, writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodPost:
		handleUISessionCreate(manager, writer, request)
	case http.MethodGet:
		handleUISessionStatus(manager, writer, request)
	case http.MethodDelete:
		handleUISessionDelete(manager, writer, request)
	default:
		writer.Header().Set("Allow", "DELETE, GET, POST")
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handleUISessionCreate(manager *uiSessionManager, writer http.ResponseWriter, request *http.Request) {
	if !requireSameOrigin(writer, request) {
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeCreateRequest(writer, request, &input) {
		return
	}
	now := manager.now()
	failureKey := authFailureKey(input.Username)
	if retryAfter := manager.loginFailures.retryAfter(failureKey, now); retryAfter > 0 {
		writeAuthenticationRateLimit(writer, retryAfter)
		return
	}
	token, session, err := manager.createUser(request.Context(), input.Username, input.Password)
	if err != nil {
		if errors.Is(err, sqlite.ErrInvalidUserCredentials) {
			manager.loginFailures.record(failureKey, now)
			unauthorized(writer)
			return
		}
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
		return
	}
	manager.loginFailures.reset(failureKey)
	expires := session.CreatedAt.Add(uiSessionAbsoluteTTL)
	http.SetCookie(writer, &http.Cookie{Name: uiSessionCookieName, Value: token, Path: "/", HttpOnly: true, Secure: manager.secureCookies || request.TLS != nil, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(uiSessionAbsoluteTTL.Seconds())})
	writeJSON(writer, http.StatusCreated, sessionAPIResponse(session))
}

func handleUISessionStatus(manager *uiSessionManager, writer http.ResponseWriter, request *http.Request) {
	_, session, ok := requestUISession(manager, writer, request)
	if !ok {
		return
	}
	writeJSON(writer, http.StatusOK, sessionAPIResponse(session))
}

func handleUISessionDelete(manager *uiSessionManager, writer http.ResponseWriter, request *http.Request) {
	token, session, ok := requestUISession(manager, writer, request)
	if !ok {
		return
	}
	if !requireSameOrigin(writer, request) || !validCSRFToken(request.Header.Get("X-CSRF-Token"), session.CSRFToken) {
		writeAPIError(writer, http.StatusForbidden, "same-origin CSRF validation failed")
		return
	}
	manager.destroy(token)
	clearUISessionCookie(writer, manager.secureCookies || request.TLS != nil)
	writer.WriteHeader(http.StatusNoContent)
}

func requestUISession(manager *uiSessionManager, writer http.ResponseWriter, request *http.Request) (string, uiSession, bool) {
	cookie, err := request.Cookie(uiSessionCookieName)
	if err != nil {
		unauthorized(writer)
		return "", uiSession{}, false
	}
	session, err := manager.authenticate(request.Context(), cookie.Value)
	if err != nil {
		if errors.Is(err, errInvalidUISession) {
			clearUISessionCookie(writer, manager.secureCookies || request.TLS != nil)
			unauthorized(writer)
			return "", uiSession{}, false
		}
		writeAPIError(writer, http.StatusInternalServerError, "internal server error")
		return "", uiSession{}, false
	}
	return cookie.Value, session, true
}

func sessionAPIResponse(session uiSession) uiSessionResponse {
	return uiSessionResponse{Name: session.Principal.Name, Role: session.Principal.Role, CSRFToken: session.CSRFToken, ExpiresAt: session.CreatedAt.Add(uiSessionAbsoluteTTL)}
}

func clearUISessionCookie(writer http.ResponseWriter, secure bool) {
	http.SetCookie(writer, &http.Cookie{Name: uiSessionCookieName, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}

func requireSameOrigin(writer http.ResponseWriter, request *http.Request) bool {
	origin, err := url.Parse(request.Header.Get("Origin"))
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host != request.Host || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		writeAPIError(writer, http.StatusForbidden, "same-origin request required")
		return false
	}
	return true
}

func validCSRFToken(presented, expected string) bool {
	return presented != "" && len(presented) == len(expected) && subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}
