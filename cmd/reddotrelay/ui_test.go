package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUIFileHandlerServesIndexAssetsAndFallback(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "index.html"), []byte("<!doctype html><title>RedDotRelay</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "assets", "app-ABC123.js"), []byte("export{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := newUIHandler(directory)

	for _, requestPath := range []string{"/ui/", "/ui/listeners/example"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "RedDotRelay") {
			t.Fatalf("GET %s = %d %q", requestPath, response.Code, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("GET %s cache = %q", requestPath, response.Header().Get("Cache-Control"))
		}
	}

	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/ui/assets/app-ABC123.js", nil))
	if asset.Code != http.StatusOK || !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset = %d, cache %q", asset.Code, asset.Header().Get("Cache-Control"))
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/ui/assets/missing.js", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d", missing.Code)
	}
}

func TestUIFileHandlerRejectsMutations(t *testing.T) {
	response := httptest.NewRecorder()
	newUIHandler(t.TempDir()).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/ui/", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST = %d, allow %q", response.Code, response.Header().Get("Allow"))
	}
}

func TestUIFileHandlerRejectsSymlinkOutsideDirectory(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.js")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "linked.js")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	response := httptest.NewRecorder()
	newUIHandler(directory).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/linked.js", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("symlink response = %d", response.Code)
	}
}
