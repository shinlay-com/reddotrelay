package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestDeliverySnapshotsWebhookAuthentication(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "hmac.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	id := core.EventID{ChainID: 1, TransactionHash: "0xabc", LogIndex: 2}
	event := core.Event{ID: id, BlockNumber: 3, BlockHash: "0xblock", Address: "0xcontract", Name: "Ping", ObservedAt: now}
	authentication := core.WebhookAuthentication{Type: "hmac-sha256", SecretRef: "env://WEBHOOK_HMAC_KEY", KeyID: "receiver-1"}
	delivery := core.Delivery{EventID: id, Destination: "https://receiver.example/hook", Authentication: authentication, NextAttempt: now}
	checkpoint := core.Checkpoint{ChainID: 1, BlockNumber: 3, BlockHash: "0xblock"}
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	items, err := store.ClaimDueDeliveries(ctx, now, time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Delivery.Authentication != authentication {
		t.Fatalf("claimed authentication = %#v", items)
	}
}

func TestConfigurationSnapshotReplacementPreservesOperationalData(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "import.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	id := core.EventID{ChainID: 1, TransactionHash: "0xdef", LogIndex: 1}
	event := core.Event{ID: id, BlockNumber: 9, BlockHash: "0xblock9", Address: "0xcontract", Name: "Ping", ObservedAt: now}
	delivery := core.Delivery{EventID: id, Destination: "https://receiver.example", NextAttempt: now}
	checkpoint := core.Checkpoint{ChainID: 1, BlockNumber: 9, BlockHash: "0xblock9"}
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	if revision, err := store.ReplaceRPCListenerSnapshotAudited(ctx, core.RPCListenerSnapshot{}, 0, now, nil); err != nil || revision != 1 {
		t.Fatalf("replace = %d, %v", revision, err)
	}
	gotCheckpoint, err := store.Checkpoint(ctx, 1)
	if err != nil || gotCheckpoint != checkpoint {
		t.Fatalf("checkpoint = %#v, %v", gotCheckpoint, err)
	}
	items, err := store.DueDeliveries(ctx, now, 1)
	if err != nil || len(items) != 1 || items[0].Event.ID != id {
		t.Fatalf("deliveries = %#v, %v", items, err)
	}
}
