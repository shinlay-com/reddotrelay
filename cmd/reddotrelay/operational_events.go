package main

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const operationalEventCapacity = 1000

var safeOperationalAttributes = map[string]struct{}{
	"attempt": {}, "chain_id": {}, "event_id": {}, "error_summary": {}, "listen_address": {},
	"events": {}, "eligible": {},
	"next_attempt": {}, "reason": {}, "retry_in": {}, "rpc_listener_id": {}, "rpc_listener_source": {},
}

type operationalEventDefinition struct{ component, message string }

var operationalEventDefinitions = map[string]operationalEventDefinition{
	"RedDotRelay started":                                      {"server", "RedDotRelay started"},
	"scanner runtime manager stopped":                          {"scanner", "Scanner runtime manager stopped"},
	"delivery worker stopped":                                  {"delivery", "Delivery worker stopped"},
	"health server stopped":                                    {"server", "HTTP server stopped"},
	"shutdown health server":                                   {"server", "HTTP server shutdown failed"},
	"RPC listener has no event subscriptions; scanner is idle": {"scanner", "RPC listener has no event subscriptions; scanner is idle"},
	"RPC TLS certificate verification is disabled":             {"scanner", "RPC TLS certificate verification is disabled"},
	"load RPC listener desired state":                          {"scanner", "Could not load desired RPC listener configuration"},
	"scanner runtime started":                                  {"scanner", "Scanner runtime started"},
	"scanner runtime stopped":                                  {"scanner", "Scanner runtime stopped"},
	"scanner runtime exited unexpectedly":                      {"scanner", "Scanner runtime exited unexpectedly"},
	"build scanner runtime":                                    {"scanner", "Scanner runtime build failed"},
	"outbox delivery cycle failed":                             {"delivery", "Outbox delivery cycle failed"},
	"delivery moved to dead letter":                            {"delivery", "Delivery moved to dead letter"},
	"delivery failed; retry scheduled":                         {"delivery", "Delivery failed; retry scheduled"},
	"scanner cycle failed":                                     {"scanner", "Scanner cycle failed"},
	"chain reorganization detected":                            {"scanner", "Chain reorganization detected"},
	"retention cleanup completed":                              {"server", "Retention cleanup completed"},
	"retention prune failed":                                   {"server", "Retention cleanup failed"},
	"SQLite optimized":                                         {"server", "SQLite storage optimized"},
	"SQLite optimize failed":                                   {"server", "SQLite storage optimization failed"},
}

type operationalEvent struct {
	Sequence   uint64         `json:"sequence"`
	Timestamp  time.Time      `json:"timestamp"`
	Level      string         `json:"level"`
	Component  string         `json:"component"`
	Message    string         `json:"message"`
	Attributes map[string]any `json:"attributes"`
}

type operationalEventBuffer struct {
	mu       sync.RWMutex
	capacity int
	next     uint64
	events   []operationalEvent
}

func newOperationalEventBuffer(capacity int) *operationalEventBuffer {
	if capacity < 1 {
		capacity = 1
	}
	return &operationalEventBuffer{capacity: capacity, next: 1, events: make([]operationalEvent, 0, capacity)}
}

func (buffer *operationalEventBuffer) append(event operationalEvent) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	event.Sequence = buffer.next
	buffer.next++
	if len(buffer.events) == buffer.capacity {
		copy(buffer.events, buffer.events[1:])
		buffer.events = buffer.events[:len(buffer.events)-1]
	}
	buffer.events = append(buffer.events, event)
}

func (buffer *operationalEventBuffer) page(limit int, before uint64, level, component string) ([]operationalEvent, bool) {
	buffer.mu.RLock()
	defer buffer.mu.RUnlock()
	result := make([]operationalEvent, 0, limit+1)
	for index := len(buffer.events) - 1; index >= 0; index-- {
		event := buffer.events[index]
		if before != 0 && event.Sequence >= before {
			continue
		}
		if (level != "" && event.Level != level) || (component != "" && event.Component != component) {
			continue
		}
		result = append(result, event)
		if len(result) == limit+1 {
			break
		}
	}
	hasMore := len(result) > limit
	if hasMore {
		result = result[:limit]
	}
	return result, hasMore
}

type operationalEventHandler struct {
	buffer     *operationalEventBuffer
	attributes []slog.Attr
}

func (handler *operationalEventHandler) Enabled(context.Context, slog.Level) bool { return true }
func (handler *operationalEventHandler) Handle(_ context.Context, record slog.Record) error {
	definition, allowed := operationalEventDefinitions[record.Message]
	if !allowed {
		return nil
	}
	attributes := make(map[string]any)
	collect := func(attribute slog.Attr) bool {
		attribute.Value = attribute.Value.Resolve()
		if _, ok := safeOperationalAttributes[attribute.Key]; ok {
			attributes[attribute.Key] = safeAttributeValue(attribute.Value)
		}
		return true
	}
	for _, attribute := range handler.attributes {
		collect(attribute)
	}
	record.Attrs(collect)
	timestamp := record.Time.UTC()
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	handler.buffer.append(operationalEvent{Timestamp: timestamp, Level: strings.ToLower(record.Level.String()), Component: definition.component, Message: definition.message, Attributes: attributes})
	return nil
}
func (handler *operationalEventHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	clone := *handler
	clone.attributes = append(append([]slog.Attr(nil), handler.attributes...), attributes...)
	return &clone
}
func (handler *operationalEventHandler) WithGroup(string) slog.Handler {
	clone := *handler
	return &clone
}

func safeAttributeValue(value slog.Value) any {
	switch value.Kind() {
	case slog.KindBool:
		return value.Bool()
	case slog.KindInt64:
		return value.Int64()
	case slog.KindUint64:
		return value.Uint64()
	case slog.KindFloat64:
		return value.Float64()
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindTime:
		return value.Time().UTC()
	default:
		return value.String()
	}
}

type teeLogHandler struct{ primary, events slog.Handler }

func (handler teeLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.primary.Enabled(ctx, level)
}
func (handler teeLogHandler) Handle(ctx context.Context, record slog.Record) error {
	if err := handler.primary.Handle(ctx, record); err != nil {
		return err
	}
	return handler.events.Handle(ctx, record.Clone())
}
func (handler teeLogHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	return teeLogHandler{handler.primary.WithAttrs(attributes), handler.events.WithAttrs(attributes)}
}
func (handler teeLogHandler) WithGroup(name string) slog.Handler {
	return teeLogHandler{handler.primary.WithGroup(name), handler.events.WithGroup(name)}
}

func handleOperationalEvents(buffer *operationalEventBuffer, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	query := request.URL.Query()
	for key := range query {
		if key != "limit" && key != "before" && key != "level" && key != "component" {
			writeAPIError(writer, http.StatusBadRequest, "unknown query parameter")
			return
		}
	}
	limit := 50
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 || strconv.Itoa(parsed) != raw {
			writeAPIError(writer, http.StatusBadRequest, "limit must be an integer between 1 and 200")
			return
		}
		limit = parsed
	}
	var before uint64
	if raw := query.Get("before"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != raw {
			writeAPIError(writer, http.StatusBadRequest, "before must be a valid event cursor")
			return
		}
		before = parsed
	}
	level := strings.ToLower(query.Get("level"))
	if level != "" && level != "info" && level != "warn" && level != "error" {
		writeAPIError(writer, http.StatusBadRequest, "level must be info, warn, or error")
		return
	}
	component := strings.ToLower(query.Get("component"))
	if component != "" && component != "server" && component != "scanner" && component != "delivery" {
		writeAPIError(writer, http.StatusBadRequest, "component must be server, scanner, or delivery")
		return
	}
	entries, hasMore := buffer.page(limit, before, level, component)
	response := map[string]any{"entries": entries}
	if hasMore && len(entries) != 0 {
		response["nextBefore"] = strconv.FormatUint(entries[len(entries)-1].Sequence, 10)
	}
	writeJSON(writer, http.StatusOK, response)
}
