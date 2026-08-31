package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"reddotrelay/internal/config"
	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
)

func TestRetentionWorkerPrunesOnlyFullyDeliveredEligibleEvents(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "reddotrelay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	old := time.Now().UTC().Add(-48 * time.Hour)
	makeEvent := func(index uint64, status core.DeliveryStatus) {
		id := core.EventID{ChainID: 1, TransactionHash: "0x" + string(rune('a'+index)), LogIndex: index}
		event := core.Event{ID: id, BlockNumber: 10 + index, BlockHash: "0xblock", Address: "0x0000000000000000000000000000000000000001", Name: "Ping", Signature: "Ping()", ObservedAt: old}
		delivery := core.Delivery{EventID: id, Destination: "https://receiver.example/hook", NextAttempt: old}
		if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, core.Checkpoint{ChainID: 1, BlockNumber: 10 + index, BlockHash: "0xblock"}); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimDueDeliveries(ctx, old, time.Minute, 1)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim = %#v, %v", claimed, err)
		}
		if status == core.DeliveryDelivered {
			if err := store.MarkDeliveryDelivered(ctx, id, delivery.Destination, claimed[0].Delivery.LeaseToken, old, 204); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := store.MarkDeliveryDead(ctx, id, delivery.Destination, claimed[0].Delivery.LeaseToken, "webhook request failed", 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	makeEvent(0, core.DeliveryDelivered)
	makeEvent(1, core.DeliveryDead)
	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runRetentionWorker(workerCtx, store, config.RetentionConfig{DeliveredFor: 24 * time.Hour, PollInterval: time.Hour, BatchSize: 10}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := store.EventHistory(ctx, core.EventHistoryFilter{}, 10)
		if err == nil && len(entries) == 1 && entries[0].Dead == 1 {
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("worker stop = %v", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("retention worker did not prune only the eligible delivered event")
}
