package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"reddotrelay/internal/core"
)

func TestMetricsEndpointIsPublicAndUsesSecurityHeaders(t *testing.T) {
	store, _ := emptyManagementFixture(t, core.APIKeyAdmin)
	metrics := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("reddotrelay_build_info 1\n"))
	})
	response := httptest.NewRecorder()
	healthHandlerWithMetrics(store, nil, metrics).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || response.Body.String() != "reddotrelay_build_info 1\n" {
		t.Fatalf("metrics response = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("metrics security headers = %#v", response.Header())
	}
}
