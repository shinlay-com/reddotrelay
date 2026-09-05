package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestRestartPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reddotrelay.db")
	now := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	event, delivery, checkpoint := fixture(now)

	store := openStore(t, path)
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	gotCheckpoint, err := store.Checkpoint(ctx, event.ID.ChainID)
	if err != nil {
		t.Fatal(err)
	}
	if gotCheckpoint != checkpoint {
		t.Fatalf("checkpoint = %#v, want %#v", gotCheckpoint, checkpoint)
	}
	items, err := store.DueDeliveries(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("due deliveries = %d, want 1", len(items))
	}
	if items[0].Event.ID != event.ID || string(items[0].Event.DecodedPayload) != string(event.DecodedPayload) {
		t.Fatalf("persisted event = %#v, want %#v", items[0].Event, event)
	}
	if items[0].Event.Signature != event.Signature || !reflect.DeepEqual(items[0].Event.RawTopics, event.RawTopics) ||
		!reflect.DeepEqual(items[0].Event.RawData, event.RawData) {
		t.Fatalf("persisted raw metadata = %#v, want %#v", items[0].Event, event)
	}
}

func TestDuplicateEventAndDeliveryAreIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	event, delivery, checkpoint := fixture(now)

	for range 2 {
		if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
			t.Fatal(err)
		}
	}
	assertCount(t, store, "events", 1)
	assertCount(t, store, "deliveries", 1)
}

func TestDuplicateEventDoesNotAcquireNewConfiguredDestination(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	event, delivery, checkpoint := fixture(now)
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}

	newRoute := delivery
	newRoute.Destination = "https://new-route.example.test/hook"
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{newRoute}, checkpoint); err != nil {
		t.Fatal(err)
	}
	assertCount(t, store, "events", 1)
	assertCount(t, store, "deliveries", 1)
	items, err := store.DueDeliveries(ctx, now, 10)
	if err != nil || len(items) != 1 || items[0].Delivery.Destination != delivery.Destination {
		t.Fatalf("persisted original route = %#v, %v", items, err)
	}
}

func TestDivergentDuplicateDoesNotAdvanceCheckpoint(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	event, delivery, checkpoint := fixture(now)
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}

	conflicting := event
	conflicting.BlockNumber++
	conflicting.BlockHash = "0xdifferent"
	conflicting.DecodedPayload = []byte(`{"value":"different"}`)
	newCheckpoint := core.Checkpoint{ChainID: checkpoint.ChainID, BlockNumber: checkpoint.BlockNumber + 1, BlockHash: "0xnew"}
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{conflicting}, []core.Delivery{delivery}, newCheckpoint); err == nil {
		t.Fatal("SaveEventsAndCheckpoint() error = nil, want conflicting identity error")
	}
	gotCheckpoint, err := store.Checkpoint(ctx, checkpoint.ChainID)
	if err != nil || gotCheckpoint != checkpoint {
		t.Fatalf("checkpoint after conflict = %#v, %v; want %#v", gotCheckpoint, err, checkpoint)
	}
	items, err := store.DueDeliveries(ctx, now, 1)
	if err != nil || len(items) != 1 || items[0].Event.BlockHash != event.BlockHash ||
		!reflect.DeepEqual(items[0].Event.DecodedPayload, event.DecodedPayload) {
		t.Fatalf("persisted event after conflict = %#v, %v", items, err)
	}
}

func TestEventDeliveryAndCheckpointAreAtomic(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	event, delivery, checkpoint := fixture(now)
	delivery.EventID.TransactionHash = "0xmissing"

	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err == nil {
		t.Fatal("SaveEventsAndCheckpoint() error = nil, want foreign-key error")
	}
	assertCount(t, store, "events", 0)
	assertCount(t, store, "deliveries", 0)
	if _, err := store.Checkpoint(ctx, checkpoint.ChainID); !errors.Is(err, core.ErrCheckpointNotFound) {
		t.Fatalf("Checkpoint() error = %v, want ErrCheckpointNotFound", err)
	}
}

func TestDiskFullDoesNotPartiallyPersistBatchOrCheckpoint(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	var pageCount, pageSize int
	if err := store.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `PRAGMA max_page_count = `+strconv.Itoa(pageCount)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	event, delivery, checkpoint := fixture(now)
	event.DecodedPayload = bytes.Repeat([]byte{1}, pageSize*8)
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err == nil {
		t.Fatal("SaveEventsAndCheckpoint() error = nil, want SQLite full error")
	}
	assertCount(t, store, "events", 0)
	assertCount(t, store, "deliveries", 0)
	if _, err := store.Checkpoint(ctx, checkpoint.ChainID); !errors.Is(err, core.ErrCheckpointNotFound) {
		t.Fatalf("Checkpoint() error after disk full = %v, want ErrCheckpointNotFound", err)
	}
}

func TestConnectionReplacementRetainsSQLiteSafetyPragmas(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	store.db.SetMaxIdleConns(0)

	var foreignKeys int
	if err := store.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys after connection replacement = %d, want 1", foreignKeys)
	}
	event, delivery, checkpoint := fixture(time.Now().UTC())
	delivery.EventID.TransactionHash = "0xmissing"
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err == nil {
		t.Fatal("SaveEventsAndCheckpoint() error = nil after reconnect, want foreign-key error")
	}
	assertCount(t, store, "events", 0)
	assertCount(t, store, "deliveries", 0)
}

func TestDeliveryTransitions(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	event, delivery, checkpoint := fixture(now)
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}

	claimed, err := store.ClaimDueDeliveries(ctx, now, time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim for retry = %#v, %v", claimed, err)
	}
	next := now.Add(2 * time.Minute)
	if err := store.ScheduleDeliveryRetry(ctx, event.ID, delivery.Destination, claimed[0].Delivery.LeaseToken, next, "temporary failure", 503); err != nil {
		t.Fatal(err)
	}
	items, err := store.DueDeliveries(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("delivery was due before retry time")
	}
	items, err = store.DueDeliveries(ctx, next, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Delivery.Attempts != 1 || items[0].Delivery.LastError != "temporary failure" {
		t.Fatalf("retried delivery = %#v", items)
	}

	claimed, err = store.ClaimDueDeliveries(ctx, next, time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim for delivery = %#v, %v", claimed, err)
	}
	deliveredAt := next.Add(time.Second)
	if err := store.MarkDeliveryDelivered(ctx, event.ID, delivery.Destination, claimed[0].Delivery.LeaseToken, deliveredAt, 204); err != nil {
		t.Fatal(err)
	}
	assertDeliveryState(t, store, event.ID, delivery.Destination, core.DeliveryDelivered, 2, "", true)
	if err := store.MarkDeliveryDead(ctx, event.ID, delivery.Destination, claimed[0].Delivery.LeaseToken, "too late", 410); !errors.Is(err, ErrNotFound) {
		t.Fatalf("transition from delivered error = %v, want ErrNotFound", err)
	}

	deadEvent := event
	deadEvent.ID.LogIndex++
	deadDelivery := delivery
	deadDelivery.EventID = deadEvent.ID
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{deadEvent}, []core.Delivery{deadDelivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.ClaimDueDeliveries(ctx, now, time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim for dead letter = %#v, %v", claimed, err)
	}
	if err := store.MarkDeliveryDead(ctx, deadEvent.ID, deadDelivery.Destination, claimed[0].Delivery.LeaseToken, "permanent failure", 500); err != nil {
		t.Fatal(err)
	}
	assertDeliveryState(t, store, deadEvent.ID, deadDelivery.Destination, core.DeliveryDead, 1, "permanent failure", false)
}

func TestDeadLetterListAndRequeue(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC)
	event, delivery, checkpoint := fixture(now)
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueDeliveries(ctx, now, time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim delivery = %#v, %v", claimed, err)
	}
	if err := store.MarkDeliveryDead(ctx, event.ID, delivery.Destination, claimed[0].Delivery.LeaseToken, "receiver rejected request", 422); err != nil {
		t.Fatal(err)
	}
	dead, err := store.DeadDeliveries(ctx, 10)
	if err != nil || len(dead) != 1 || dead[0].Event.ID != event.ID || dead[0].Delivery.LastError != "receiver rejected request" {
		t.Fatalf("dead deliveries = %#v, %v", dead, err)
	}
	if count, err := store.RequeueDeadByGUID(ctx, "00000000-0000-5000-8000-000000000000", now); err != nil || count != 0 {
		t.Fatalf("requeue unknown GUID = %d, %v", count, err)
	}
	if count, err := store.RequeueDeadByGUID(ctx, strings.ToUpper(core.EventGUID(event.ID)), now); err != nil || count != 1 {
		t.Fatalf("requeue event GUID = %d, %v", count, err)
	}
	assertDeliveryState(t, store, event.ID, delivery.Destination, core.DeliveryPending, 0, "", false)

	claimed, err = store.ClaimDueDeliveries(ctx, now, time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim requeued delivery = %#v, %v", claimed, err)
	}
	if err := store.MarkDeliveryDead(ctx, event.ID, delivery.Destination, claimed[0].Delivery.LeaseToken, "failed again", 503); err != nil {
		t.Fatal(err)
	}
	if count, err := store.RequeueAllDead(ctx, now); err != nil || count != 1 {
		t.Fatalf("requeue all = %d, %v", count, err)
	}
	assertDeliveryState(t, store, event.ID, delivery.Destination, core.DeliveryPending, 0, "", false)
}

func TestRetentionPrunesOnlyFullyDeliveredOldEvents(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC)
	baseEvent, baseDelivery, checkpoint := fixture(now)

	events := make([]core.Event, 4)
	deliveries := make([]core.Delivery, 0, 5)
	for i := range events {
		events[i] = baseEvent
		events[i].ID.LogIndex += uint64(i)
		delivery := baseDelivery
		delivery.EventID = events[i].ID
		deliveries = append(deliveries, delivery)
	}
	secondDestination := baseDelivery
	secondDestination.EventID = events[0].ID
	secondDestination.Destination = "https://second.example.test/hook"
	deliveries = append(deliveries, secondDestination)
	if err := store.SaveEventsAndCheckpoint(ctx, events, deliveries, checkpoint); err != nil {
		t.Fatal(err)
	}

	old := now.Add(-100 * 24 * time.Hour).UnixNano()
	recent := now.Add(-24 * time.Hour).UnixNano()
	if _, err := store.db.ExecContext(ctx, `UPDATE deliveries SET status = 'delivered', delivered_at = ? WHERE log_index = ?`, old, events[0].ID.LogIndex); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE deliveries SET status = 'dead' WHERE log_index = ?`, events[2].ID.LogIndex); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE deliveries SET status = 'delivered', delivered_at = ? WHERE log_index = ?`, recent, events[3].ID.LogIndex); err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-90 * 24 * time.Hour)
	if count, err := store.CountDeliveredBefore(ctx, cutoff); err != nil || count != 1 {
		t.Fatalf("eligible retention events = %d, %v; want 1", count, err)
	}
	if count, err := store.PruneDeliveredBefore(ctx, cutoff, 100); err != nil || count != 1 {
		t.Fatalf("pruned retention events = %d, %v; want 1", count, err)
	}
	assertCount(t, store, "events", 3)
	assertCount(t, store, "deliveries", 3)
	gotCheckpoint, err := store.Checkpoint(ctx, checkpoint.ChainID)
	if err != nil || gotCheckpoint != checkpoint {
		t.Fatalf("checkpoint after retention = %#v, %v; want %#v", gotCheckpoint, err, checkpoint)
	}
}

func TestClaimDeliveryLeaseSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reddotrelay.db")
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	event, delivery, checkpoint := fixture(now)
	store := openStore(t, path)
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueDeliveries(ctx, now, time.Minute, 1)
	if err != nil || len(claimed) != 1 || claimed[0].Delivery.Attempts != 1 || claimed[0].Delivery.LeaseToken == "" {
		t.Fatalf("first claim = %#v, %v", claimed, err)
	}
	firstToken := claimed[0].Delivery.LeaseToken
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	if items, err := store.DueDeliveries(ctx, now, 1); err != nil || len(items) != 0 {
		t.Fatalf("leased deliveries = %#v, %v", items, err)
	}
	claimed, err = store.ClaimDueDeliveries(ctx, now.Add(time.Minute), time.Minute, 1)
	if err != nil || len(claimed) != 1 || claimed[0].Delivery.Attempts != 2 ||
		claimed[0].Delivery.LeaseToken == "" || claimed[0].Delivery.LeaseToken == firstToken {
		t.Fatalf("recovered claim = %#v, %v", claimed, err)
	}
}

func TestRewindRemovesOrphanedEventsAndDeliveries(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	event, delivery, checkpoint := fixture(now)
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	store.db.SetMaxIdleConns(0)

	rewound := core.Checkpoint{ChainID: 1, BlockNumber: 99, BlockHash: "0xcanonical99"}
	if err := store.Rewind(ctx, rewound); err != nil {
		t.Fatal(err)
	}
	assertCount(t, store, "events", 0)
	assertCount(t, store, "deliveries", 0)
	checkpoint, err := store.Checkpoint(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint != rewound {
		t.Fatalf("checkpoint = %#v, want %#v", checkpoint, rewound)
	}
}

func TestResetFromDeletesBoundaryEventAndCheckpoint(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	event, delivery, checkpoint := fixture(time.Now().UTC())
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := store.ResetFrom(ctx, event.ID.ChainID, event.BlockNumber); err != nil {
		t.Fatal(err)
	}
	assertCount(t, store, "events", 0)
	assertCount(t, store, "deliveries", 0)
	if _, err := store.Checkpoint(ctx, event.ID.ChainID); !errors.Is(err, core.ErrCheckpointNotFound) {
		t.Fatalf("Checkpoint() error = %v, want ErrCheckpointNotFound", err)
	}
}

func TestStaleLeaseCannotCompleteReinsertedEvent(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 23, 4, 0, 0, 0, time.UTC)
	orphan, delivery, checkpoint := fixture(now)
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{orphan}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueDeliveries(ctx, now, time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("orphan claim = %#v, %v", claimed, err)
	}
	staleToken := claimed[0].Delivery.LeaseToken

	if err := store.ResetFrom(ctx, orphan.ID.ChainID, orphan.BlockNumber); err != nil {
		t.Fatal(err)
	}
	canonical := orphan
	canonical.BlockHash = "0xcanonical"
	canonical.DecodedPayload = []byte(`{"value":"canonical"}`)
	canonicalCheckpoint := checkpoint
	canonicalCheckpoint.BlockHash = canonical.BlockHash
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{canonical}, []core.Delivery{delivery}, canonicalCheckpoint); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliveryDelivered(ctx, canonical.ID, delivery.Destination, staleToken, now.Add(time.Second), 204); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale completion error = %v, want ErrNotFound", err)
	}
	items, err := store.DueDeliveries(ctx, now, 1)
	if err != nil || len(items) != 1 || items[0].Event.BlockHash != canonical.BlockHash ||
		!reflect.DeepEqual(items[0].Event.DecodedPayload, canonical.DecodedPayload) {
		t.Fatalf("canonical pending delivery = %#v, %v", items, err)
	}
}

func TestLegacyPayloadIsBackfilled(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reddotrelay.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE checkpoints (
    chain_id INTEGER PRIMARY KEY, block_number INTEGER NOT NULL, block_hash TEXT NOT NULL
);
CREATE TABLE events (
    chain_id INTEGER NOT NULL, transaction_hash TEXT NOT NULL, log_index INTEGER NOT NULL,
    block_number INTEGER NOT NULL, block_hash TEXT NOT NULL, address TEXT NOT NULL,
    name TEXT NOT NULL, payload BLOB NOT NULL, observed_at INTEGER NOT NULL,
    PRIMARY KEY (chain_id, transaction_hash, log_index)
);
CREATE TABLE deliveries (
    chain_id INTEGER NOT NULL, transaction_hash TEXT NOT NULL, log_index INTEGER NOT NULL,
    destination TEXT NOT NULL, status TEXT NOT NULL, attempts INTEGER NOT NULL,
    next_attempt INTEGER NOT NULL, last_error TEXT NOT NULL, delivered_at INTEGER,
    PRIMARY KEY (chain_id, transaction_hash, log_index, destination)
);
INSERT INTO checkpoints VALUES (1, 100, '0xblock');
INSERT INTO events VALUES (1, '0xabc', 7, 100, '0xblock', '0xcontract', 'Transfer',
    '{"value":"legacy"}', 0);
INSERT INTO deliveries VALUES (1, '0xabc', 7, 'https://example.test/hook', 'pending', 0, 0, '', NULL);
`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store := openStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	items, err := store.DueDeliveries(ctx, time.Unix(0, 0).UTC(), 1)
	if err != nil || len(items) != 1 || string(items[0].Event.DecodedPayload) != `{"value":"legacy"}` {
		t.Fatalf("migrated delivery = %#v, %v", items, err)
	}
}

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func fixture(now time.Time) (core.Event, core.Delivery, core.Checkpoint) {
	id := core.EventID{ChainID: 1, TransactionHash: "0xabc", LogIndex: 7}
	event := core.Event{
		ID: id, BlockNumber: 100, BlockHash: "0xblock", Address: "0xcontract",
		Name: "Transfer", Signature: "Transfer(address,address,uint256)",
		RawTopics: []string{"0xtopic"}, RawData: []byte{1, 2},
		DecodedPayload: []byte(`{"value":"42"}`), ObservedAt: now,
	}
	delivery := core.Delivery{EventID: id, Destination: "https://example.test/hook", NextAttempt: now}
	checkpoint := core.Checkpoint{ChainID: 1, BlockNumber: 100, BlockHash: "0xblock"}
	return event, delivery, checkpoint
}

func assertCount(t *testing.T, store *Store, table string, want int) {
	t.Helper()
	var got int
	if err := store.db.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}

func assertDeliveryState(t *testing.T, store *Store, id core.EventID, destination string, wantStatus core.DeliveryStatus, wantAttempts int, wantError string, wantDeliveredAt bool) {
	t.Helper()
	var status core.DeliveryStatus
	var attempts int
	var lastError string
	var deliveredAt any
	err := store.db.QueryRow(`
SELECT status, attempts, last_error, delivered_at FROM deliveries
WHERE chain_id = ? AND transaction_hash = ? AND log_index = ? AND destination = ?`,
		id.ChainID, id.TransactionHash, id.LogIndex, destination).
		Scan(&status, &attempts, &lastError, &deliveredAt)
	if err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || attempts != wantAttempts || lastError != wantError || (deliveredAt != nil) != wantDeliveredAt {
		t.Fatalf("delivery state = (%s, %d, %q, delivered=%v)", status, attempts, lastError, deliveredAt != nil)
	}
}

func TestInitializeWithRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retry_test.db")
	store := openStore(t, path)
	defer store.Close()

	if err := store.initializeWithRetry(ctx); err != nil {
		t.Fatalf("initializeWithRetry failed on existing database: %v", err)
	}

	var foreignKeys, synchronous int
	if err := store.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, want 1", foreignKeys)
	}
	if err := store.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != 2 { // 2 == FULL
		t.Fatalf("PRAGMA synchronous = %d, want 2 (FULL)", synchronous)
	}
}
