package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestRPCListenerAggregatePersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reddotrelay.db")
	store := openStore(t, path)
	now := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	config := rpcListenerFixture()

	revision, err := store.CreateRPCListener(ctx, config, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if revision != 1 {
		t.Fatalf("revision = %d, want 1", revision)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	snapshot, err := store.RPCListenerSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 1 || !snapshot.UpdatedAt.Equal(now) || len(snapshot.Listeners) != 1 {
		t.Fatalf("snapshot header = %#v", snapshot)
	}
	got := snapshot.Listeners[0]
	if got.ID != config.ID || got.Name != config.Name || got.ChainID != config.ChainID || got.RPCURL != config.RPCURL ||
		got.PollInterval != config.PollInterval || got.RPCRetryBackoff != config.RPCRetryBackoff || got.RPCTimeout != config.RPCTimeout ||
		got.TLS != config.TLS || len(got.Webhooks) != 1 || len(got.Contracts) != 1 || len(got.Contracts[0].Events) != 1 {
		t.Fatalf("persisted listener = %#v", got)
	}
	if string(got.Contracts[0].ABI) != string(config.Contracts[0].ABI) || got.Contracts[0].Events[0].Selector != config.Contracts[0].Events[0].Selector ||
		got.Contracts[0].Events[0].Webhooks[0].URL != config.Contracts[0].Events[0].Webhooks[0].URL {
		t.Fatalf("persisted nested configuration = %#v", got.Contracts[0])
	}
	if !got.CreatedAt.Equal(now) || !got.Contracts[0].CreatedAt.Equal(now) || !got.Contracts[0].Events[0].CreatedAt.Equal(now) {
		t.Fatalf("creation timestamps = %#v", got)
	}
}

func TestReplaceRPCListenerIsAtomicAndPreservesCreationTimes(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	createdAt := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)
	config := rpcListenerFixture()
	if _, err := store.CreateRPCListener(ctx, config, 0, createdAt); err != nil {
		t.Fatal(err)
	}

	config.Name = "renamed-chain"
	config.Contracts[0].ABI = json.RawMessage(`[{"type":"event","name":"Approval","inputs":[]}]`)
	config.Contracts[0].Events[0].Selector = "Approval()"
	config.Contracts[0].Events[0].Webhooks[0].URL = "https://updated.example.test/hook"
	config.Contracts[0].Events = append(config.Contracts[0].Events, core.EventConfig{
		ID: core.NewConfigID(), Selector: "Transfer()",
		Webhooks: []core.WebhookConfig{{ID: core.NewConfigID(), URL: "https://new.example.test/hook"}},
	})
	revision, err := store.ReplaceRPCListener(ctx, config, 1, updatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if revision != 2 {
		t.Fatalf("revision = %d, want 2", revision)
	}
	got, gotRevision, err := store.RPCListener(ctx, config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRevision != 2 || got.Name != config.Name || len(got.Contracts[0].Events) != 2 {
		t.Fatalf("replaced listener = %#v at revision %d", got, gotRevision)
	}
	if !got.CreatedAt.Equal(createdAt) || !got.Contracts[0].CreatedAt.Equal(createdAt) || !got.Contracts[0].Events[0].CreatedAt.Equal(createdAt) {
		t.Fatalf("existing creation timestamps changed: %#v", got)
	}
	if !got.UpdatedAt.Equal(updatedAt) || !got.Contracts[0].Events[1].CreatedAt.Equal(updatedAt) {
		t.Fatalf("replacement timestamps = %#v", got)
	}
}

func TestRPCListenerRevisionConflictRollsBackMutation(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	config := rpcListenerFixture()
	if _, err := store.CreateRPCListener(ctx, config, 0, now); err != nil {
		t.Fatal(err)
	}

	changed := config
	changed.Name = "stale-write"
	returnedRevision, err := store.ReplaceRPCListener(ctx, changed, 0, now.Add(time.Minute))
	if !errors.Is(err, ErrRevisionConflict) || returnedRevision != 1 {
		t.Fatalf("stale replace = revision %d, error %v", returnedRevision, err)
	}
	got, revision, err := store.RPCListener(ctx, config.ID)
	if err != nil || revision != 1 || got.Name != config.Name {
		t.Fatalf("configuration after stale replace = %#v, revision %d, error %v", got, revision, err)
	}
}

func TestDeleteRPCListenerCascadesConfigurationOnly(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	config := rpcListenerFixture()
	if _, err := store.CreateRPCListener(ctx, config, 0, now); err != nil {
		t.Fatal(err)
	}
	event, delivery, checkpoint := fixture(now)
	event.ID.ChainID = config.ChainID
	delivery.EventID = event.ID
	checkpoint.ChainID = config.ChainID
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}

	revision, err := store.DeleteRPCListener(ctx, config.ID, 1, now.Add(time.Minute))
	if err != nil || revision != 2 {
		t.Fatalf("delete = revision %d, error %v", revision, err)
	}
	snapshot, err := store.RPCListenerSnapshot(ctx)
	if err != nil || len(snapshot.Listeners) != 0 {
		t.Fatalf("listener snapshot after delete = %#v, %v", snapshot, err)
	}
	gotCheckpoint, err := store.Checkpoint(ctx, config.ChainID)
	if err != nil || gotCheckpoint != checkpoint {
		t.Fatalf("checkpoint after config delete = %#v, %v", gotCheckpoint, err)
	}
	items, err := store.DueDeliveries(ctx, now, 1)
	if err != nil || len(items) != 1 || items[0].Event.ID != event.ID {
		t.Fatalf("outbox after config delete = %#v, %v", items, err)
	}
	assertCount(t, store, "contract_configs", 0)
	assertCount(t, store, "event_configs", 0)
	assertCount(t, store, "event_webhook_configs", 0)
}

func TestReplaceGlobalWebhooksPersistsOrder(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	want := []core.WebhookConfig{
		{ID: core.NewConfigID(), URL: "https://one.example.test/hook"},
		{ID: core.NewConfigID(), URL: "https://two.example.test/hook"},
	}
	revision, err := store.ReplaceGlobalWebhooks(ctx, want, 0, now)
	if err != nil || revision != 1 {
		t.Fatalf("replace global webhooks = revision %d, error %v", revision, err)
	}
	snapshot, err := store.RPCListenerSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gotURLs := []string{snapshot.GlobalWebhooks[0].URL, snapshot.GlobalWebhooks[1].URL}
	wantURLs := []string{want[0].URL, want[1].URL}
	if !reflect.DeepEqual(gotURLs, wantURLs) {
		t.Fatalf("global webhook order = %#v, want %#v", gotURLs, wantURLs)
	}
}

func TestCreateRPCListenerRejectsInvalidAggregateWithoutRevisionChange(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	config := rpcListenerFixture()
	config.Contracts[0].ABI = json.RawMessage(`not-json`)
	if _, err := store.CreateRPCListener(ctx, config, 0, time.Now()); err == nil {
		t.Fatal("CreateRPCListener() error = nil, want invalid ABI error")
	}
	snapshot, err := store.RPCListenerSnapshot(ctx)
	if err != nil || snapshot.Revision != 0 || len(snapshot.Listeners) != 0 {
		t.Fatalf("snapshot after invalid create = %#v, %v", snapshot, err)
	}
}

func TestRPCListenerChangesNotifiesOnlyAfterCommit(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	config := rpcListenerFixture()
	if _, err := store.CreateRPCListener(ctx, config, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.RPCListenerChanges():
	default:
		t.Fatal("successful commit did not emit a configuration change")
	}
	config.Name = "stale"
	if _, err := store.ReplaceRPCListener(ctx, config, 0, time.Now()); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale mutation error = %v", err)
	}
	select {
	case <-store.RPCListenerChanges():
		t.Fatal("failed mutation emitted a configuration change")
	default:
	}
}

func TestRPCListenerSnapshotIsConsistentDuringReplacement(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	config := rpcListenerFixture()
	config.Name = "version-0"
	config.Contracts[0].Events[0].Webhooks[0].URL = "https://version-0.example.test/hook"
	if _, err := store.CreateRPCListener(ctx, config, 0, time.Now()); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	writerError := make(chan error, 1)
	go func() {
		defer close(done)
		for version := 1; version <= 30; version++ {
			value := strconv.Itoa(version)
			config.Name = "version-" + value
			config.Contracts[0].Events[0].Webhooks[0].URL = "https://version-" + value + ".example.test/hook"
			if _, err := store.ReplaceRPCListener(ctx, config, uint64(version), time.Now()); err != nil {
				writerError <- err
				return
			}
			time.Sleep(100 * time.Microsecond)
		}
	}()

	for {
		select {
		case <-done:
			select {
			case err := <-writerError:
				t.Fatal(err)
			default:
			}
			return
		default:
		}
		snapshot, err := store.RPCListenerSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := snapshot.Listeners[0]
		version := strings.TrimPrefix(got.Name, "version-")
		wantURL := "https://version-" + version + ".example.test/hook"
		if got.Contracts[0].Events[0].Webhooks[0].URL != wantURL {
			t.Fatalf("mixed snapshot at revision %d: name=%s webhook=%s", snapshot.Revision, got.Name, got.Contracts[0].Events[0].Webhooks[0].URL)
		}
	}
}

func rpcListenerFixture() core.RPCListener {
	return core.RPCListener{
		ID: core.NewConfigID(), Name: "private-besu", ChainID: 9171317,
		RPCURL: "https://rpc.example.test", StartBlock: 10, BatchSize: 2000,
		PollInterval: 3 * time.Second, Confirmations: 2, ReorgDepth: 12,
		RPCRetryAttempts: 5, RPCRetryBackoff: 500 * time.Millisecond, RPCTimeout: 15 * time.Second,
		TLS:      core.ListenerTLSConfig{CAPEM: "test-ca", ServerName: "rpc.example.test"},
		Webhooks: []core.WebhookConfig{{ID: core.NewConfigID(), URL: "https://chain.example.test/hook"}},
		Contracts: []core.ContractConfig{{
			ID: core.NewConfigID(), Address: "0x0000000000000000000000000000000000000001",
			ABI:      json.RawMessage(`[{"type":"event","name":"Transfer","inputs":[]}]`),
			Webhooks: []core.WebhookConfig{{ID: core.NewConfigID(), URL: "https://contract.example.test/hook"}},
			Events: []core.EventConfig{{
				ID: core.NewConfigID(), Selector: "Transfer()",
				Webhooks: []core.WebhookConfig{{ID: core.NewConfigID(), URL: "https://event.example.test/hook"}},
			}},
		}},
	}
}
