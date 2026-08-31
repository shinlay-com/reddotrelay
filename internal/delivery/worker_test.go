package delivery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
)

func TestWorkerDeliversWithIdempotencyHeader(t *testing.T) {
	var gotHeader string
	var gotPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotHeader = request.Header.Get(idempotencyHeader)
		if err := json.NewDecoder(request.Body).Decode(&gotPayload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	item := testItem(server.URL, 1)
	outbox := &memoryOutbox{items: []core.OutboxItem{item}}
	worker := newWorker(t, outbox, Options{Workers: 1, BatchSize: 1, MaxAttempts: 3})
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotHeader != eventID(item.Event.ID) {
		t.Fatalf("idempotency header = %q", gotHeader)
	}
	if gotPayload["eventId"] != gotHeader || gotPayload["eventName"] != "Transfer" ||
		gotPayload["chainId"] != float64(1) || gotPayload["eventSignature"] != "Transfer(address,address,uint256)" {
		t.Fatalf("webhook payload = %#v", gotPayload)
	}
	params, ok := gotPayload["params"].(map[string]any)
	if !ok || params["value"] != "42" {
		t.Fatalf("webhook params = %#v", gotPayload["params"])
	}
	if _, exists := gotPayload["event_id"]; exists {
		t.Fatalf("legacy snake_case field remains in webhook payload: %#v", gotPayload)
	}
	if len(outbox.delivered) != 1 || len(outbox.retries) != 0 || len(outbox.dead) != 0 {
		t.Fatalf("outbox transitions = %#v", outbox)
	}
}

func TestWorkerResolvesReferencedDestinationWithoutPersistingValue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	item := testItem("env://WEBHOOK_URL", 1)
	outbox := &memoryOutbox{items: []core.OutboxItem{item}}
	options := Options{Workers: 1, BatchSize: 1, HTTPTimeout: time.Second, LeaseDuration: 2 * time.Second, RetryBackoff: time.Millisecond, MaxBackoff: time.Second, MaxAttempts: 3, PollInterval: time.Second}
	worker, err := NewWithResolver(outbox, options, slog.New(slog.NewTextHandler(io.Discard, nil)), staticResolver{value: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(outbox.delivered) != 1 || item.Delivery.Destination != "env://WEBHOOK_URL" {
		t.Fatalf("delivery was not resolved safely: %#v", outbox)
	}
}

func TestWorkerSignsExactPayloadWithHMACSHA256(t *testing.T) {
	var body []byte
	var timestamp, signature, keyID string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ = io.ReadAll(request.Body)
		timestamp = request.Header.Get(timestampHeader)
		signature = request.Header.Get(signatureHeader)
		keyID = request.Header.Get(keyIDHeader)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	item := testItem(server.URL, 1)
	item.Delivery.Authentication = core.WebhookAuthentication{Type: "hmac-sha256", SecretRef: "env://WEBHOOK_SIGNING_KEY", KeyID: "receiver-2026-08"}
	outbox := &memoryOutbox{items: []core.OutboxItem{item}}
	options := Options{Workers: 1, BatchSize: 1, HTTPTimeout: time.Second, LeaseDuration: 2 * time.Second, RetryBackoff: time.Millisecond, MaxBackoff: time.Second, MaxAttempts: 3, PollInterval: time.Second}
	worker, err := NewWithResolver(outbox, options, slog.New(slog.NewTextHandler(io.Discard, nil)), staticResolver{value: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1787446800, 0).UTC()
	worker.now = func() time.Time { return fixed }
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("test-secret"))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	if timestamp != "1787446800" || signature != want || keyID != "receiver-2026-08" {
		t.Fatalf("signature headers = timestamp:%q signature:%q keyID:%q, want %q", timestamp, signature, keyID, want)
	}
}

func TestWorkerResolvesSigningKeyAndRefreshesTimestampOnRetry(t *testing.T) {
	var timestamps []string
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		timestamps = append(timestamps, request.Header.Get(timestampHeader))
		requests++
		if requests == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	item := testItem(server.URL, 1)
	item.Delivery.Authentication = core.WebhookAuthentication{Type: "hmac-sha256", SecretRef: "file:///run/secrets/webhook_hmac"}
	outbox := &memoryOutbox{items: []core.OutboxItem{item}}
	options := Options{Workers: 1, BatchSize: 1, HTTPTimeout: time.Second, LeaseDuration: 2 * time.Second, RetryBackoff: time.Millisecond, MaxBackoff: time.Second, MaxAttempts: 3, PollInterval: time.Second}
	resolver := &countingResolver{value: "rotatable-secret"}
	worker, err := NewWithResolver(outbox, options, slog.New(slog.NewTextHandler(io.Discard, nil)), resolver)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	worker.now = func() time.Time { return now }
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = time.Unix(101, 0).UTC()
	outbox.items = []core.OutboxItem{item}
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 2 || !reflect.DeepEqual(timestamps, []string{"100", "101"}) {
		t.Fatalf("retry signing = resolves:%d timestamps:%v", resolver.calls, timestamps)
	}
}

type staticResolver struct {
	value string
	err   error
}

type countingResolver struct {
	value string
	calls int
}

func (r *countingResolver) Resolve(context.Context, string) (string, error) {
	r.calls++
	return r.value, nil
}

func (r staticResolver) Resolve(context.Context, string) (string, error) { return r.value, r.err }

func TestWorkerRetriesThenMovesToDeadLetter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte("unavailable"))
	}))
	defer server.Close()
	outbox := &memoryOutbox{items: []core.OutboxItem{testItem(server.URL, 1)}}
	worker := newWorker(t, outbox, Options{Workers: 1, BatchSize: 1, MaxAttempts: 2, RetryBackoff: time.Second, MaxBackoff: 10 * time.Second})
	fixed := time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC)
	worker.now = func() time.Time { return fixed }
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(outbox.retries) != 1 || !outbox.retries[0].next.Equal(fixed.Add(time.Second)) || !strings.Contains(outbox.retries[0].message, "503") {
		t.Fatalf("retry = %#v", outbox.retries)
	}

	outbox.items = []core.OutboxItem{testItem(server.URL, 2)}
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(outbox.dead) != 1 || !strings.Contains(outbox.dead[0].message, "503") {
		t.Fatalf("dead letter = %#v", outbox.dead)
	}
}

func TestWorkerBoundsConcurrentRequests(t *testing.T) {
	var active, maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		active.Add(-1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	items := make([]core.OutboxItem, 4)
	for i := range items {
		items[i] = testItem(server.URL, 1)
		items[i].Event.ID.LogIndex = uint64(i)
		items[i].Delivery.EventID = items[i].Event.ID
	}
	outbox := &memoryOutbox{items: items}
	worker := newWorker(t, outbox, Options{Workers: 2, BatchSize: 4, MaxAttempts: 3})
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrent requests = %d, want 2", got)
	}
	if outbox.claimLimit != 2 || len(outbox.delivered) != 2 {
		t.Fatalf("claim limit = %d, delivered = %d; want one immediately runnable item per worker", outbox.claimLimit, len(outbox.delivered))
	}
}

func TestWorkerRetriesOnHTTPTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	outbox := &memoryOutbox{items: []core.OutboxItem{testItem(server.URL, 1)}}
	worker := newWorker(t, outbox, Options{Workers: 1, BatchSize: 1, HTTPTimeout: 10 * time.Millisecond, LeaseDuration: 50 * time.Millisecond, MaxAttempts: 3})
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(outbox.retries) != 1 || outbox.retries[0].message != "webhook request timed out" {
		t.Fatalf("timeout retry = %#v", outbox.retries)
	}
}

func TestWorkerRetriesClientErrorUntilAttemptLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	outbox := &memoryOutbox{items: []core.OutboxItem{testItem(server.URL, 1)}}
	worker := newWorker(t, outbox, Options{Workers: 1, BatchSize: 1, MaxAttempts: 5})
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(outbox.dead) != 0 || len(outbox.retries) != 1 {
		t.Fatalf("client error transitions = dead:%#v retries:%#v", outbox.dead, outbox.retries)
	}
}

func TestWorkerDoesNotTreatRedirectedGETAsDelivery(t *testing.T) {
	var redirectedRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	outbox := &memoryOutbox{items: []core.OutboxItem{testItem(redirect.URL, 1)}}
	worker := newWorker(t, outbox, Options{Workers: 1, BatchSize: 1, MaxAttempts: 3})
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if redirectedRequests.Load() != 0 || len(outbox.delivered) != 0 || len(outbox.retries) != 1 {
		t.Fatalf("redirect handling = followed:%d delivered:%d retries:%d", redirectedRequests.Load(), len(outbox.delivered), len(outbox.retries))
	}
}

func TestSuccessfulWebhookIsRetriedWhenCompletionWriteFails(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reddotrelay.db")
	now := time.Date(2026, 8, 23, 3, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	item := testItem(server.URL, 0)
	item.Event.ObservedAt = now
	item.Delivery.NextAttempt = now
	checkpoint := core.Checkpoint{ChainID: 1, BlockNumber: item.Event.BlockNumber, BlockHash: item.Event.BlockHash}
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{item.Event}, []core.Delivery{item.Delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	worker := newWorker(t, failingCompletionOutbox{Store: store}, Options{Workers: 1, BatchSize: 1, HTTPTimeout: time.Second, LeaseDuration: time.Minute, MaxAttempts: 3})
	worker.now = func() time.Time { return now }
	if err := worker.DrainOnce(ctx); err == nil {
		t.Fatal("DrainOnce() error = nil, want completion-write failure")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	claimed, err := store.ClaimDueDeliveries(ctx, now.Add(time.Minute), time.Minute, 1)
	if err != nil || len(claimed) != 1 || claimed[0].Delivery.Attempts != 2 {
		t.Fatalf("recovered delivery = %#v, %v", claimed, err)
	}
}

func TestWebhookOutageRecoversAfterStoreRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reddotrelay.db")
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	var unavailable atomic.Bool
	unavailable.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if unavailable.Load() {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	item := testItem(server.URL, 0)
	item.Event.ObservedAt = now
	item.Delivery.NextAttempt = now
	checkpoint := core.Checkpoint{ChainID: 1, BlockNumber: item.Event.BlockNumber, BlockHash: item.Event.BlockHash}
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{item.Event}, []core.Delivery{item.Delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	worker := newWorker(t, store, Options{Workers: 1, BatchSize: 1, HTTPTimeout: time.Second, LeaseDuration: time.Minute, RetryBackoff: time.Second, MaxBackoff: time.Second, MaxAttempts: 3})
	worker.now = func() time.Time { return now }
	if err := worker.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	unavailable.Store(false)
	store, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	worker = newWorker(t, store, Options{Workers: 1, BatchSize: 1, HTTPTimeout: time.Second, LeaseDuration: time.Minute, RetryBackoff: time.Second, MaxBackoff: time.Second, MaxAttempts: 3})
	worker.now = func() time.Time { return now.Add(time.Second) }
	if err := worker.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}
	pending, delivered, dead, err := store.DeliveryStatusCounts(ctx)
	if err != nil || pending != 0 || delivered != 1 || dead != 0 {
		t.Fatalf("delivery counts after recovery = pending:%d delivered:%d dead:%d, %v", pending, delivered, dead, err)
	}
	gotCheckpoint, err := store.Checkpoint(ctx, checkpoint.ChainID)
	if err != nil || gotCheckpoint != checkpoint {
		t.Fatalf("checkpoint during webhook outage = %#v, %v; want %#v", gotCheckpoint, err, checkpoint)
	}
}

type failingCompletionOutbox struct{ *sqlite.Store }

func (f failingCompletionOutbox) MarkDeliveryDelivered(context.Context, core.EventID, string, string, time.Time, int) error {
	return errors.New("simulated completion write failure")
}

type memoryOutbox struct {
	mu         sync.Mutex
	items      []core.OutboxItem
	delivered  []core.EventID
	retries    []retry
	dead       []dead
	claimLimit int
	statusCode int
}

type retry struct {
	id      core.EventID
	next    time.Time
	message string
}

type dead struct {
	id      core.EventID
	message string
}

func (m *memoryOutbox) ClaimDueDeliveries(_ context.Context, _ time.Time, _ time.Duration, limit int) ([]core.OutboxItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claimLimit = limit
	limit = min(limit, len(m.items))
	return append([]core.OutboxItem(nil), m.items[:limit]...), nil
}

func (m *memoryOutbox) MarkDeliveryDelivered(_ context.Context, id core.EventID, _, _ string, _ time.Time, statusCode int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delivered = append(m.delivered, id)
	m.statusCode = statusCode
	return nil
}

func (m *memoryOutbox) ScheduleDeliveryRetry(_ context.Context, id core.EventID, _, _ string, next time.Time, message string, statusCode int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retries = append(m.retries, retry{id: id, next: next, message: message})
	m.statusCode = statusCode
	return nil
}

func (m *memoryOutbox) MarkDeliveryDead(_ context.Context, id core.EventID, _, _, message string, statusCode int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dead = append(m.dead, dead{id: id, message: message})
	m.statusCode = statusCode
	return nil
}

func newWorker(t *testing.T, outbox Outbox, options Options) *Worker {
	t.Helper()
	if options.Workers == 0 {
		options.Workers = 1
	}
	if options.BatchSize == 0 {
		options.BatchSize = 1
	}
	if options.HTTPTimeout == 0 {
		options.HTTPTimeout = time.Second
	}
	if options.LeaseDuration == 0 {
		options.LeaseDuration = 2 * time.Second
	}
	if options.RetryBackoff == 0 {
		options.RetryBackoff = time.Millisecond
	}
	if options.MaxBackoff == 0 {
		options.MaxBackoff = time.Second
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = 3
	}
	if options.PollInterval == 0 {
		options.PollInterval = time.Second
	}
	worker, err := New(outbox, options, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func testItem(destination string, attempts int) core.OutboxItem {
	id := core.EventID{ChainID: 1, TransactionHash: "0xabc", LogIndex: 7}
	event := core.Event{ID: id, BlockNumber: 10, BlockHash: "0xblock", Address: "0xcontract", Name: "Transfer", Signature: "Transfer(address,address,uint256)", DecodedPayload: []byte(`{"value":"42"}`)}
	return core.OutboxItem{Event: event, Delivery: core.Delivery{EventID: id, Destination: destination, Attempts: attempts, LeaseToken: "test-lease"}}
}
