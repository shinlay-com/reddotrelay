package sqlite

import (
	"context"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestRPCListenerAuditIsAtomicWithMutation(t *testing.T) {
	store := openStore(t, t.TempDir()+"/audit.db")
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	config := rpcListenerFixture()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	invalid := core.RPCListenerAudit{
		ActorID: "not-a-uuid", ActorName: "operator", ActorRole: core.APIKeyAdmin,
		Action: core.AuditActionCreate, ResourceKind: core.AuditResourceRPCListener,
		ResourceID: config.ID,
	}
	if _, err := store.CreateRPCListenerAudited(ctx, config, 0, now, &invalid); err == nil {
		t.Fatal("CreateRPCListenerAudited() error = nil with invalid audit actor")
	}
	snapshot, err := store.RPCListenerSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 0 || len(snapshot.Listeners) != 0 {
		t.Fatalf("mutation was not rolled back: revision=%d listeners=%d", snapshot.Revision, len(snapshot.Listeners))
	}
	entries, err := store.RPCListenerAudit(ctx, 10, 0)
	if err != nil || len(entries) != 0 {
		t.Fatalf("audit after rollback = %#v, %v", entries, err)
	}

	valid := invalid
	valid.ActorID = core.NewConfigID()
	if revision, err := store.CreateRPCListenerAudited(ctx, config, 0, now, &valid); err != nil || revision != 1 {
		t.Fatalf("CreateRPCListenerAudited() = %d, %v", revision, err)
	}
	entries, err = store.RPCListenerAudit(ctx, 10, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit entries = %#v, %v", entries, err)
	}
	entry := entries[0]
	if entry.ActorID != valid.ActorID || entry.ActorName != valid.ActorName || entry.ActorRole != valid.ActorRole ||
		entry.Action != core.AuditActionCreate || entry.ResourceID != config.ID ||
		entry.PreviousRevision != 0 || entry.NewRevision != 1 || !entry.CreatedAt.Equal(now) {
		t.Fatalf("audit entry = %#v", entry)
	}
}

func TestRPCListenerAuditPagination(t *testing.T) {
	store := openStore(t, t.TempDir()+"/audit-pagination.db")
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	config := rpcListenerFixture()
	actor := core.RPCListenerAudit{
		ActorID: core.NewConfigID(), ActorName: "deployment-console", ActorRole: core.APIKeyAdmin,
		ResourceKind: core.AuditResourceRPCListener, ResourceID: config.ID,
	}
	actor.Action = core.AuditActionCreate
	if _, err := store.CreateRPCListenerAudited(ctx, config, 0, time.Unix(1, 0), &actor); err != nil {
		t.Fatal(err)
	}
	config.Name = "updated"
	actor.Action = core.AuditActionUpdate
	if _, err := store.ReplaceRPCListenerAudited(ctx, config, 1, time.Unix(2, 0), &actor); err != nil {
		t.Fatal(err)
	}
	actor.Action = core.AuditActionDelete
	if _, err := store.DeleteRPCListenerAudited(ctx, config.ID, 2, time.Unix(3, 0), &actor); err != nil {
		t.Fatal(err)
	}

	first, err := store.RPCListenerAudit(ctx, 2, 0)
	if err != nil || len(first) != 2 || first[0].Action != core.AuditActionDelete || first[1].Action != core.AuditActionUpdate {
		t.Fatalf("first page = %#v, %v", first, err)
	}
	second, err := store.RPCListenerAudit(ctx, 2, first[1].Sequence)
	if err != nil || len(second) != 1 || second[0].Action != core.AuditActionCreate {
		t.Fatalf("second page = %#v, %v", second, err)
	}
}
