package main

import (
	"net/http"
	"reddotrelay/internal/auth"
	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
	"strings"
	"time"
)

func handleAPIKeys(store *sqlite.Store, w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		keys, err := store.APIKeys(r.Context())
		if err != nil {
			writeAPIError(w, 500, "API keys unavailable")
			return
		}
		writeJSON(w, 200, map[string]any{"apiKeys": keys})
		return
	}
	if r.Method != http.MethodPost || !requireAdmin(w, r) {
		return
	}
	var in struct {
		Name    string `json:"name"`
		Confirm bool   `json:"confirm"`
	}
	if !decodeCreateRequest(w, r, &in) {
		return
	}
	if !in.Confirm {
		writeAPIError(w, 400, "confirm must be true")
		return
	}
	secret, err := auth.GenerateAPIKeySecret()
	if err != nil {
		writeAPIError(w, 500, "key generation failed")
		return
	}
	hash, _ := auth.HashAPIKeySecret(secret)
	key := core.APIKey{ID: core.NewConfigID(), Name: strings.TrimSpace(in.Name), Role: core.APIKeyReadOnly, Prefix: auth.APIKeyPrefix(secret), CreatedAt: time.Now().UTC()}
	if err := store.CreateAPIKey(r.Context(), key, hash); err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"apiKey": key, "secret": secret})
}
func handleAPIKeyResource(store *sqlite.Store, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !requireAdmin(w, r) {
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/api-keys/"), "/"), "/")
	if len(parts) != 2 {
		writeAPIError(w, 404, "resource not found")
		return
	}
	id, operation := parts[0], parts[1]
	var in struct {
		Role    core.APIKeyRole `json:"role"`
		Confirm bool            `json:"confirm"`
	}
	if !decodeCreateRequest(w, r, &in) {
		return
	}
	if !in.Confirm {
		writeAPIError(w, 400, "confirm must be true")
		return
	}
	var err error
	switch operation {
	case "revoke":
		err = store.RevokeAPIKey(r.Context(), id, time.Now().UTC())
	case "role":
		err = store.SetAPIKeyRole(r.Context(), id, in.Role)
	default:
		writeAPIError(w, 404, "resource not found")
		return
	}
	if err != nil {
		if err == sqlite.ErrNotFound {
			writeAPIError(w, 404, "API key not found")
		} else {
			writeAPIError(w, 400, err.Error())
		}
		return
	}
	w.WriteHeader(204)
}
