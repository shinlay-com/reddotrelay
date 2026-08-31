package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestEventHistoryFiltersAndPaginatesDeterministically(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	base := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC)
	baseEvent, baseDelivery, checkpoint := fixture(base)
	events := make([]core.Event, 3)
	deliveries := make([]core.Delivery, 3)
	for i := range events {
		events[i] = baseEvent
		events[i].ID.TransactionHash = "0x" + strings.Repeat(string(rune('a'+i)), 64)
		events[i].ID.LogIndex = uint64(i)
		events[i].BlockNumber = 100 + uint64(i)
		events[i].ObservedAt = base.Add(time.Duration(i) * time.Second)
		events[i].Signature = "Transfer(address,address,uint256)"
		deliveries[i] = baseDelivery
		deliveries[i].EventID = events[i].ID
		deliveries[i].Destination = "https://receiver.example/" + string(rune('a'+i))
	}
	checkpoint.BlockNumber = 102
	if err := store.SaveEventsAndCheckpoint(ctx, events, deliveries, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE deliveries SET status = 'delivered', delivered_at = ?, last_status_code = 204 WHERE transaction_hash = ?`, base.UnixNano(), events[1].ID.TransactionHash); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE deliveries SET status = 'dead', last_error = 'webhook returned HTTP 503', last_status_code = 503 WHERE transaction_hash = ?`, events[2].ID.TransactionHash); err != nil {
		t.Fatal(err)
	}

	first, err := store.EventHistory(ctx, core.EventHistoryFilter{}, 2)
	if err != nil || len(first) != 2 || first[0].Event.ID != events[2].ID || first[0].Dead != 1 || first[1].Delivered != 1 {
		t.Fatalf("first event page = %#v, error %v", first, err)
	}
	cursor := core.EventHistoryCursor{ObservedAt: first[1].Event.ObservedAt, ChainID: first[1].Event.ID.ChainID, TransactionHash: first[1].Event.ID.TransactionHash, LogIndex: first[1].Event.ID.LogIndex}
	second, err := store.EventHistory(ctx, core.EventHistoryFilter{Before: &cursor}, 2)
	if err != nil || len(second) != 1 || second[0].Event.ID != events[0].ID || second[0].Pending != 1 {
		t.Fatalf("second event page = %#v, error %v", second, err)
	}
	status := core.DeliveryDead
	filtered, err := store.EventHistory(ctx, core.EventHistoryFilter{TransactionHash: strings.ToUpper(events[2].ID.TransactionHash), Address: strings.ToUpper(events[2].Address), BlockNumber: &events[2].BlockNumber, Signature: events[2].Signature, DeliveryStatus: status}, 2)
	if err != nil || len(filtered) != 1 || filtered[0].Event.ID != events[2].ID {
		t.Fatalf("filtered event page = %#v, error %v", filtered, err)
	}
}

func TestEventDeliveriesAreBoundedAndExposeSafeAttemptState(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC)
	event, delivery, checkpoint := fixture(now)
	first := delivery
	first.Destination = "env://WEBHOOK_A"
	second := delivery
	second.Destination = "https://receiver.example/b"
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{first, second}, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE deliveries SET status = 'dead', attempts = 4, last_attempt_at = ?, last_status_code = 503, last_error = 'webhook returned HTTP 503' WHERE destination = ?`, now.UnixNano(), first.Destination); err != nil {
		t.Fatal(err)
	}

	eventGUID := core.EventGUID(event.ID)
	all, err := store.EventDeliveries(ctx, eventGUID, "", 2)
	if err != nil || len(all) != 2 {
		t.Fatalf("all deliveries = %#v, error %v", all, err)
	}
	var dead *core.Delivery
	for i := range all {
		if all[i].Destination == first.Destination {
			dead = &all[i]
		}
	}
	if dead == nil || dead.Status != core.DeliveryDead || dead.Attempts != 4 || dead.LastAttemptAt == nil || dead.LastStatusCode != 503 || dead.ID == "" {
		t.Fatalf("dead delivery diagnostics = %#v", dead)
	}
	page, err := store.EventDeliveries(ctx, eventGUID, "", 1)
	if err != nil || len(page) != 1 {
		t.Fatalf("first delivery page = %#v, error %v", page, err)
	}
	next, err := store.EventDeliveries(ctx, eventGUID, page[0].ID, 2)
	if err != nil || len(next) != 1 || next[0].ID == page[0].ID {
		t.Fatalf("second delivery page = %#v, error %v", next, err)
	}
}

func TestEventHistoryLargeDatabaseRemainsBounded(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventStatement, err := tx.PrepareContext(ctx, `INSERT INTO events (event_guid, chain_id, transaction_hash, log_index, block_number, block_hash, address, name, signature, observed_at) VALUES (?, 1, ?, 0, ?, '0xblock', '0x0000000000000000000000000000000000000001', 'Ping', 'Ping()', ?)`)
	if err != nil {
		t.Fatal(err)
	}
	deliveryStatement, err := tx.PrepareContext(ctx, `INSERT INTO deliveries (delivery_guid, chain_id, transaction_hash, log_index, destination, status, next_attempt) VALUES (?, 1, ?, 0, 'env://LOAD_TEST_WEBHOOK', 'dead', ?)`)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 24, 5, 0, 0, 0, time.UTC)
	for i := 0; i < 2000; i++ {
		id := core.EventID{ChainID: 1, TransactionHash: fmt.Sprintf("0x%064x", i), LogIndex: 0}
		stamp := base.Add(time.Duration(i) * time.Nanosecond).UnixNano()
		if _, err := eventStatement.ExecContext(ctx, core.EventGUID(id), id.TransactionHash, i, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := deliveryStatement.ExecContext(ctx, core.DeliveryGUID(id, "env://LOAD_TEST_WEBHOOK"), id.TransactionHash, stamp); err != nil {
			t.Fatal(err)
		}
	}
	_ = eventStatement.Close()
	_ = deliveryStatement.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	first, err := store.EventHistory(ctx, core.EventHistoryFilter{DeliveryStatus: core.DeliveryDead}, 51)
	if err != nil || len(first) != 51 {
		t.Fatalf("large first page = %d, %v", len(first), err)
	}
	last := first[49]
	cursor := core.EventHistoryCursor{ObservedAt: last.Event.ObservedAt, ChainID: last.Event.ID.ChainID, TransactionHash: last.Event.ID.TransactionHash, LogIndex: last.Event.ID.LogIndex}
	second, err := store.EventHistory(ctx, core.EventHistoryFilter{DeliveryStatus: core.DeliveryDead, Before: &cursor}, 51)
	if err != nil || len(second) != 51 || second[0].EventGUID == first[49].EventGUID {
		t.Fatalf("large second page = %d, %v", len(second), err)
	}
}
