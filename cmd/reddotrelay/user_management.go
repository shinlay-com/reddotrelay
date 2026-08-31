package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
)

func handleUsers(store *sqlite.Store, w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		users, err := store.Users(r.Context())
		if err != nil {
			writeAPIError(w, 500, "users unavailable")
			return
		}
		writeJSON(w, 200, map[string]any{"users": users})
		return
	}
	if r.Method != http.MethodPost || !requireAdmin(w, r) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			writeAPIError(w, 405, "method not allowed")
		}
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Confirm  bool   `json:"confirm"`
	}
	if !decodeCreateRequest(w, r, &in) {
		return
	}
	if !in.Confirm {
		writeAPIError(w, http.StatusBadRequest, "confirm must be true")
		return
	}
	user, err := store.CreateUser(r.Context(), in.Username, in.Password, core.APIKeyReadOnly, time.Now().UTC())
	if err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, user)
}
func handleUserResource(store *sqlite.Store, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !requireAdmin(w, r) {
		if r.Method != http.MethodPost {
			writeAPIError(w, 405, "method not allowed")
		}
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/users/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		writeAPIError(w, 404, "resource not found")
		return
	}
	id, action := parts[0], parts[1]
	var in struct {
		Enabled  *bool           `json:"enabled"`
		Password string          `json:"password"`
		Role     core.APIKeyRole `json:"role"`
		Confirm  bool            `json:"confirm"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || !in.Confirm {
		writeAPIError(w, 400, "valid request and confirm must be provided")
		return
	}
	var err error
	switch action {
	case "enabled":
		if in.Enabled == nil {
			writeAPIError(w, 400, "enabled is required")
			return
		}
		err = store.SetUserEnabled(r.Context(), id, *in.Enabled, time.Now().UTC())
	case "password":
		err = store.ResetUserPassword(r.Context(), id, in.Password, time.Now().UTC())
	case "role":
		err = store.SetUserRole(r.Context(), id, in.Role, time.Now().UTC())
	default:
		writeAPIError(w, 404, "resource not found")
		return
	}
	if err == sqlite.ErrNotFound {
		writeAPIError(w, 404, "user not found")
		return
	}
	if err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	w.WriteHeader(204)
}
