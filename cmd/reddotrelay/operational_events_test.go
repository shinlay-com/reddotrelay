package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestOperationalEventHandlerCapturesOnlyAllowlistedSecretSafeRecords(t *testing.T) {
	buffer := newOperationalEventBuffer(10)
	output := &bytes.Buffer{}
	logger := slog.New(teeLogHandler{primary: slog.NewTextHandler(output, nil), events: &operationalEventHandler{buffer: buffer}})
	logger.Warn("delivery failed; retry scheduled", "event_id", "event-1", "next_attempt", 2, "error", "secret=do-not-expose", "url", "https://user:pass@example.test")
	logger.Error("dynamic secret: do-not-expose", "event_id", "event-2")

	entries, more := buffer.page(10, 0, "", "")
	if more || len(entries) != 1 {
		t.Fatalf("events = %#v, more %v", entries, more)
	}
	event := entries[0]
	if event.Message != "Delivery failed; retry scheduled" || event.Component != "delivery" || event.Attributes["event_id"] != "event-1" || event.Attributes["next_attempt"] != int64(2) {
		t.Fatalf("event = %#v", event)
	}
	if _, exists := event.Attributes["error"]; exists {
		t.Fatal("unsafe error attribute was captured")
	}
}

func TestOperationalEventBufferBoundedPaginationAndFilters(t *testing.T) {
	buffer := newOperationalEventBuffer(3)
	for index, item := range []struct{ level, component string }{{"info", "server"}, {"warn", "scanner"}, {"error", "delivery"}, {"warn", "scanner"}} {
		buffer.append(operationalEvent{Timestamp: time.Unix(int64(index), 0).UTC(), Level: item.level, Component: item.component, Message: "event", Attributes: map[string]any{}})
	}
	page, more := buffer.page(2, 0, "", "")
	if !more || len(page) != 2 || page[0].Sequence != 4 || page[1].Sequence != 3 {
		t.Fatalf("first page = %#v, more %v", page, more)
	}
	page, more = buffer.page(2, page[1].Sequence, "warn", "scanner")
	if more || len(page) != 1 || page[0].Sequence != 2 {
		t.Fatalf("filtered page = %#v, more %v", page, more)
	}
}

func TestOperationalEventsEndpointSupportsReadOnlySession(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyReadOnly)
	buffer := newOperationalEventBuffer(10)
	buffer.append(operationalEvent{Timestamp: time.Now().UTC(), Level: "info", Component: "server", Message: "RedDotRelay started", Attributes: map[string]any{}})
	handler := healthHandlerWithSessions(store, nil, nil, "", newUISessionManager(store, false), buffer)
	response := authenticatedRequest(handler, secret, http.MethodGet, "/api/v1/operational-events?limit=1&component=server")
	if response.Code != http.StatusOK {
		t.Fatalf("events = %d %s", response.Code, response.Body.String())
	}
	bad := authenticatedRequest(handler, secret, http.MethodGet, "/api/v1/operational-events?level=debug")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid filter = %d", bad.Code)
	}
}
