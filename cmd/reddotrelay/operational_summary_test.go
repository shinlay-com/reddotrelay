package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"reddotrelay/internal/core"
	"reddotrelay/internal/observability"
)

func TestOperationalSummaryIsAuthenticatedAndUsesInternalCounters(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyReadOnly)
	metrics := observability.New(store, "test")
	metrics.BatchCommitted("listener-1", 1, 10, 10, 4)
	metrics.DeliveryAttempt("retry")
	handler := healthHandlerWithRuntimeOperations(store, nil, nil, "", newUISessionManager(store, false), newOperationalEventBuffer(10), nil, metrics)

	if response := authenticatedRequest(handler, "", http.MethodGet, "/api/v1/operational-summary"); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated summary status = %d", response.Code)
	}
	response := authenticatedRequest(handler, secret, http.MethodGet, "/api/v1/operational-summary")
	if response.Code != http.StatusOK {
		t.Fatalf("summary status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Deliveries struct {
			PendingRetrying int64 `json:"pendingRetrying"`
		} `json:"deliveries"`
		Counters observability.Summary `json:"counters"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if payload.Deliveries.PendingRetrying != 0 || payload.Counters.EventsProcessedTotal != 4 || payload.Counters.DeliveryFailuresTotal != 1 {
		t.Fatalf("summary = %#v", payload)
	}
}
