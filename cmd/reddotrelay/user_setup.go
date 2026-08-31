package main

import (
	"errors"
	"net/http"

	"reddotrelay/internal/store/sqlite"
)

func handleUserSetup(store *sqlite.Store, sessions *uiSessionManager, writer http.ResponseWriter, request *http.Request) {
	hasUsers, err := store.HasUsers(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "setup status unavailable")
		return
	}
	if request.Method == http.MethodGet {
		writeJSON(writer, http.StatusOK, map[string]bool{"required": !hasUsers})
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", "GET, POST")
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireSameOrigin(writer, request) {
		return
	}
	if hasUsers {
		writeAPIError(writer, http.StatusConflict, "initial setup is complete")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeCreateRequest(writer, request, &input) {
		return
	}
	now := sessions.now()
	failureKey := authFailureKey("initial-admin-setup")
	if retryAfter := sessions.setupFailures.retryAfter(failureKey, now); retryAfter > 0 {
		writeAuthenticationRateLimit(writer, retryAfter)
		return
	}
	_, err = store.CreateInitialAdmin(request.Context(), input.Username, input.Password, now)
	if errors.Is(err, sqlite.ErrUserSetupComplete) {
		writeAPIError(writer, http.StatusConflict, "initial setup is complete")
		return
	}
	if err != nil {
		sessions.setupFailures.record(failureKey, now)
		writeAPIError(writer, http.StatusBadRequest, err.Error())
		return
	}
	sessions.setupFailures.reset(failureKey)
	writer.WriteHeader(http.StatusCreated)
}
