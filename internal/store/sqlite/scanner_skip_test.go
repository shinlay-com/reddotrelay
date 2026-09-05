package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"reddotrelay/internal/core"
	"strings"
	"testing"
	"time"
)

func TestScannerSkipAtomicAndDurable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skip.db")
	s := openStore(t, path)
	now := time.Now().UTC()
	listener := rpcListenerFixture()
	listener.Paused = true
	revision, err := s.CreateRPCListener(ctx, listener, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	previous := core.Checkpoint{ChainID: listener.ChainID, BlockNumber: 20, BlockHash: "0x" + strings.Repeat("1", 64)}
	if err := s.SaveEventsAndCheckpoint(ctx, nil, nil, previous); err != nil {
		t.Fatal(err)
	}
	target := core.CanonicalBlock{ChainID: listener.ChainID, Number: 100, Hash: "0x" + strings.Repeat("2", 64), ParentHash: "0x" + strings.Repeat("3", 64)}
	actor := core.APIKeyPrincipal{ID: core.NewConfigID(), Name: "operator", Role: core.APIKeyAdmin}
	// A failure at the final audit insert must roll back checkpoint and resume.
	if _, err := s.db.Exec(`CREATE TRIGGER reject_skip BEFORE INSERT ON scanner_skip_audit BEGIN SELECT RAISE(ABORT,'test'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SkipScannerToHead(ctx, listener.ID, revision, &previous, target, actor, now); err == nil {
		t.Fatal("wanted rollback")
	}
	cp, err := s.Checkpoint(ctx, listener.ChainID)
	if err != nil || cp != previous {
		t.Fatalf("checkpoint changed: %+v %v", cp, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_skip`); err != nil {
		t.Fatal(err)
	}
	wrong := previous
	wrong.BlockNumber--
	if _, err := s.SkipScannerToHead(ctx, listener.ID, revision, &wrong, target, actor, now); !errors.Is(err, ErrScannerSkipConflict) {
		t.Fatalf("stale checkpoint: %v", err)
	}
	result, err := s.SkipScannerToHead(ctx, listener.ID, revision, &previous, target, actor, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.FromBlock != 21 || result.ToBlock != 100 || result.Revision != revision+1 {
		t.Fatalf("audit: %+v", result)
	}
	if _, err := s.SkipScannerToHead(ctx, listener.ID, revision, &previous, target, actor, now); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("replay: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, path)
	defer s.Close()
	cp, err = s.Checkpoint(ctx, listener.ChainID)
	if err != nil || cp.BlockNumber != 100 || cp.BlockHash != target.Hash {
		t.Fatalf("checkpoint: %+v %v", cp, err)
	}
	var paused bool
	var hash, parent string
	if err := s.db.QueryRow(`SELECT paused FROM rpc_listeners WHERE id=?`, listener.ID).Scan(&paused); err != nil || paused {
		t.Fatalf("not resumed: %v", err)
	}
	if err := s.db.QueryRow(`SELECT block_hash,parent_hash FROM canonical_blocks WHERE chain_id=? AND block_number=100`, listener.ChainID).Scan(&hash, &parent); err != nil || hash != target.Hash || parent != target.ParentHash {
		t.Fatalf("anchor: %v", err)
	}
	audit, err := s.ScannerSkipAudit(ctx, listener.ID, 10, 0)
	if err != nil || len(audit) != 1 || audit[0].ActorName != "operator" {
		t.Fatalf("audit: %+v %v", audit, err)
	}
	page, err := s.ScannerSkipAudit(ctx, listener.ID, 10, audit[0].Sequence)
	if err != nil || len(page) != 0 {
		t.Fatalf("page: %+v %v", page, err)
	}
}

func TestScannerSkipPreservesOutboxAndRejectsActiveBackfill(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, filepath.Join(t.TempDir(), "skip.db"))
	defer s.Close()
	now := time.Now().UTC()
	listener := rpcListenerFixture()
	listener.ChainID = 1
	listener.Paused = true
	revision, err := s.CreateRPCListener(ctx, listener, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	event, delivery, previous := fixture(now)
	if err := s.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, previous); err != nil {
		t.Fatal(err)
	}
	leased, err := s.ClaimDueDeliveries(ctx, now, time.Minute, 1)
	if err != nil || len(leased) != 1 {
		t.Fatalf("claim: %v", err)
	}
	var lease string
	if err := s.db.QueryRow(`SELECT lease_token FROM deliveries`).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	actor := core.APIKeyPrincipal{ID: core.NewConfigID(), Name: "admin", Role: core.APIKeyAdmin}
	target := core.CanonicalBlock{ChainID: 1, Number: 200, Hash: "0x" + strings.Repeat("4", 64), ParentHash: "0x" + strings.Repeat("5", 64)}
	sealed, err := s.SealBackfillSnapshot([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	job := core.BackfillJob{ID: core.NewConfigID(), ListenerID: listener.ID, ChainID: 1, Mode: "backfill-missing", FromBlock: 10, ToBlock: 11, ConfigRevision: revision, Snapshot: sealed}
	if err := s.CreateBackfill(ctx, job, actor, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SkipScannerToHead(ctx, listener.ID, revision, &previous, target, actor, now); !errors.Is(err, ErrActiveBackfill) {
		t.Fatalf("active job accepted: %v", err)
	}
	// Test-only terminal transition; production uses the audited backfill API.
	if _, err := s.db.Exec(`UPDATE backfill_jobs SET state='cancelled' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SkipScannerToHead(ctx, listener.ID, revision, &previous, target, actor, now); err != nil {
		t.Fatal(err)
	}
	assertCount(t, s, "events", 1)
	assertCount(t, s, "deliveries", 1)
	var after string
	if err := s.db.QueryRow(`SELECT lease_token FROM deliveries`).Scan(&after); err != nil || after != lease || lease == "" {
		t.Fatalf("lease changed %v", err)
	}
	assertDeliveryState(t, s, event.ID, delivery.Destination, core.DeliveryPending, 1, "", false)
}
