package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestBackupCreatesVerifiedRestorableSnapshot(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	source := filepath.Join(directory, "source.db")
	destination := filepath.Join(directory, "backup.db")
	store := openStore(t, source)
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	event, delivery, checkpoint := fixture(now)
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, source, destination); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restored := openStore(t, destination)
	t.Cleanup(func() { _ = restored.Close() })
	got, err := restored.Checkpoint(ctx, checkpoint.ChainID)
	if err != nil || got != checkpoint {
		t.Fatalf("restored checkpoint = %#v, %v; want %#v", got, err, checkpoint)
	}
	items, err := restored.DueDeliveries(ctx, now, 1)
	if err != nil || len(items) != 1 || items[0].Event.ID != event.ID {
		t.Fatalf("restored delivery = %#v, %v", items, err)
	}
}

func TestBackupRefusesOverwriteAndRemovesFailedDestination(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	source := filepath.Join(directory, "source.db")
	destination := filepath.Join(directory, "backup.db")
	store := openStore(t, source)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, source, destination); err == nil {
		t.Fatal("Backup() error = nil, want existing-destination error")
	}
	contents, err := os.ReadFile(destination)
	if err != nil || string(contents) != "keep" {
		t.Fatalf("existing destination changed: %q, %v", contents, err)
	}
}

func TestRestoreReplacesDatabaseAndRemovesStaleSidecars(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	source := filepath.Join(directory, "source.db")
	backup := filepath.Join(directory, "backup.db")
	destination := filepath.Join(directory, "destination.db")
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	event, delivery, checkpoint := fixture(now)
	store := openStore(t, source)
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, source, backup); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	old := openStore(t, destination)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(destination+suffix, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Restore(ctx, backup, destination); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(destination + suffix); !os.IsNotExist(err) {
			t.Fatalf("stale sidecar %s still exists: %v", suffix, err)
		}
	}
	restored := openStore(t, destination)
	t.Cleanup(func() { _ = restored.Close() })
	got, err := restored.Checkpoint(ctx, checkpoint.ChainID)
	if err != nil || got != checkpoint {
		t.Fatalf("restored checkpoint = %#v, %v; want %#v", got, err, checkpoint)
	}
}
