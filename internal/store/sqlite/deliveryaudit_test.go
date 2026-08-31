package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestDeadDeliveryRequeueAndAuditAreAtomic(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
	event, delivery, checkpoint := fixture(now)
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	deliveryID := core.DeliveryGUID(event.ID, delivery.Destination)
	if _, err := store.db.ExecContext(ctx, `UPDATE deliveries SET status = 'dead', attempts = 4, total_attempts = 9, last_error = 'webhook returned HTTP 503', last_status_code = 503 WHERE delivery_guid = ?`, deliveryID); err != nil {
		t.Fatal(err)
	}
	actor := core.DeliveryRequeueAudit{ActorID: core.NewConfigID(), ActorName: "operator", ActorRole: core.APIKeyAdmin}
	entry, err := store.RequeueDeadDeliveryAudited(ctx, deliveryID, actor, now.Add(time.Minute))
	if err != nil || entry.DeliveryID != deliveryID || entry.EventID != core.EventGUID(event.ID) || entry.PreviousAttempts != 4 || entry.Sequence == 0 {
		t.Fatalf("requeue entry = %#v, error %v", entry, err)
	}
	deliveries, err := store.EventDeliveries(ctx, entry.EventID, "", 2)
	if err != nil || len(deliveries) != 1 || deliveries[0].Status != core.DeliveryPending || deliveries[0].Attempts != 0 || deliveries[0].TotalAttempts != 9 || deliveries[0].LastError != "" || deliveries[0].LastStatusCode != 0 {
		t.Fatalf("requeued delivery = %#v, error %v", deliveries, err)
	}
	audit, err := store.DeliveryRequeueAudit(ctx, 2, 0)
	if err != nil || len(audit) != 1 || audit[0].ID != entry.ID || audit[0].ActorName != "operator" {
		t.Fatalf("requeue audit = %#v, error %v", audit, err)
	}
	if _, err := store.RequeueDeadDeliveryAudited(ctx, deliveryID, actor, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("requeue non-dead error = %v", err)
	}
	if audit, err := store.DeliveryRequeueAudit(ctx, 2, 0); err != nil || len(audit) != 1 {
		t.Fatalf("failed requeue created audit: %#v, %v", audit, err)
	}
}
