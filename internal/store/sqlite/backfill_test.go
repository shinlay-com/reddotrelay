package sqlite

import (
	"context"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestBackfillMissingIsAtomicIdempotentAndResumable(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	actor := core.APIKeyPrincipal{ID: "actor", Name: "admin", Role: core.APIKeyAdmin}
	snapshot, _ := s.SealBackfillSnapshot([]byte(`{}`))
	job := core.BackfillJob{ID: core.NewConfigID(), ListenerID: core.NewConfigID(), ChainID: 1, Mode: "backfill-missing", FromBlock: 10, ToBlock: 11, ConfigRevision: 2, Snapshot: snapshot, Destinations: []string{"https://example.test"}}
	if err := s.CreateBackfill(ctx, job, actor, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimBackfill(ctx, now)
	if err != nil || claimed.NextBlock != 10 {
		t.Fatalf("claim=%#v %v", claimed, err)
	}
	event, delivery, _ := fixture(now)
	event.BlockNumber = 10
	delivery.EventID = event.ID
	if _, _, _, err = s.SaveBackfillBatch(ctx, job.ID, []core.Event{event}, []core.Delivery{delivery}, 11, 1, 1, now); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = s.SaveBackfillBatch(ctx, job.ID, []core.Event{event}, []core.Delivery{delivery}, 0, 1, 1, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Backfill(ctx, job.ID)
	if err != nil || got.State != core.BackfillCompleted || got.CreatedEvents != 1 || got.CreatedDeliveries != 1 || got.Duplicates != 1 || got.ProcessedBlocks != 2 {
		t.Fatalf("job=%#v err=%v", got, err)
	}
	audits, err := s.BackfillAudit(ctx, 10, 0)
	if err != nil || len(audits) != 3 {
		t.Fatalf("audits=%#v err=%v", audits, err)
	}
}

func TestBackfillFailedCanBeCancelledAndResumeClearsFailure(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	actor := core.APIKeyPrincipal{ID: "actor", Name: "admin", Role: core.APIKeyAdmin}
	snapshot, _ := s.SealBackfillSnapshot([]byte(`{}`))
	job := core.BackfillJob{ID: core.NewConfigID(), ListenerID: core.NewConfigID(), ChainID: 1, Mode: "backfill-missing", FromBlock: 10, ToBlock: 11, ConfigRevision: 2, Snapshot: snapshot}
	if err := s.CreateBackfill(ctx, job, actor, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimBackfill(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := s.FailBackfill(ctx, job.ID, "RPC query failed; resume to retry", now); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionBackfill(ctx, job.ID, core.BackfillQueued, actor, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.Backfill(ctx, job.ID)
	if err != nil || resumed.State != core.BackfillQueued || resumed.FailureSummary != "" || resumed.CompletedAt != nil {
		t.Fatalf("resumed=%#v err=%v", resumed, err)
	}
	if _, err := s.ClaimBackfill(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.FailBackfill(ctx, job.ID, "RPC query failed; resume to retry", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionBackfill(ctx, job.ID, core.BackfillCancelled, actor, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.Backfill(ctx, job.ID)
	if err != nil || cancelled.State != core.BackfillCancelled || cancelled.CompletedAt == nil {
		t.Fatalf("cancelled=%#v err=%v", cancelled, err)
	}
}

func TestCanonicalHistoryIncludesEmptyBlocksAndPreciseRewind(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	event, delivery, _ := fixture(now)
	event.BlockNumber = 3
	cp := core.Checkpoint{ChainID: 1, BlockNumber: 3, BlockHash: "0x03"}
	blocks := []core.CanonicalBlock{{ChainID: 1, Number: 1, Hash: "0x01", ParentHash: "0x00"}, {ChainID: 1, Number: 2, Hash: "0x02", ParentHash: "0x01"}, {ChainID: 1, Number: 3, Hash: "0x03", ParentHash: "0x02"}}
	if err = s.SaveCanonicalBatch(ctx, []core.Event{event}, []core.Delivery{delivery}, blocks, cp, 4); err != nil {
		t.Fatal(err)
	}
	history, err := s.CanonicalBlocks(ctx, 1, 4)
	if err != nil || len(history) != 3 {
		t.Fatalf("history=%#v %v", history, err)
	}
	if err = s.RewindCanonical(ctx, 1, 3, "0x02"); err != nil {
		t.Fatal(err)
	}
	cp, err = s.Checkpoint(ctx, 1)
	if err != nil || cp.BlockNumber != 2 {
		t.Fatalf("checkpoint=%#v %v", cp, err)
	}
	history, _ = s.CanonicalBlocks(ctx, 1, 4)
	if len(history) != 2 {
		t.Fatalf("history after rewind=%#v", history)
	}
}
