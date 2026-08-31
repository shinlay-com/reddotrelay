package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"reddotrelay/internal/core"
)

const maxDeliveryAuditPageSize = 201

func (s *Store) RequeueDeadDeliveryAudited(ctx context.Context, deliveryID string, actor core.DeliveryRequeueAudit, now time.Time) (core.DeliveryRequeueAuditEntry, error) {
	deliveryID = strings.ToLower(strings.TrimSpace(deliveryID))
	if deliveryID == "" {
		return core.DeliveryRequeueAuditEntry{}, errors.New("delivery ID is required")
	}
	if actor.ActorID == "" || actor.ActorName == "" || actor.ActorRole != core.APIKeyAdmin {
		return core.DeliveryRequeueAuditEntry{}, errors.New("valid admin audit actor is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.DeliveryRequeueAuditEntry{}, fmt.Errorf("begin delivery requeue: %w", err)
	}
	defer tx.Rollback()
	var eventID string
	var attempts int
	if err := tx.QueryRowContext(ctx, `SELECT e.event_guid, d.attempts FROM deliveries d JOIN events e USING (chain_id, transaction_hash, log_index) WHERE d.delivery_guid = ? AND d.status = 'dead'`, deliveryID).Scan(&eventID, &attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.DeliveryRequeueAuditEntry{}, ErrNotFound
		}
		return core.DeliveryRequeueAuditEntry{}, fmt.Errorf("load dead delivery for requeue: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE deliveries SET status = 'pending', attempts = 0, lease_token = '', next_attempt = ?, last_error = '', last_status_code = NULL, delivered_at = NULL WHERE delivery_guid = ? AND status = 'dead'`, now.UTC().UnixNano(), deliveryID)
	if err != nil {
		return core.DeliveryRequeueAuditEntry{}, fmt.Errorf("requeue dead delivery: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return core.DeliveryRequeueAuditEntry{}, fmt.Errorf("read requeue result: %w", err)
	}
	if changed != 1 {
		return core.DeliveryRequeueAuditEntry{}, ErrNotFound
	}
	entry := core.DeliveryRequeueAuditEntry{ID: core.NewConfigID(), ActorID: actor.ActorID, ActorName: actor.ActorName, ActorRole: actor.ActorRole, DeliveryID: deliveryID, EventID: eventID, PreviousAttempts: attempts, CreatedAt: now.UTC()}
	insert, err := tx.ExecContext(ctx, `INSERT INTO delivery_requeue_audit (id, actor_id, actor_name, actor_role, delivery_id, event_id, previous_attempts, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, entry.ID, entry.ActorID, entry.ActorName, entry.ActorRole, entry.DeliveryID, entry.EventID, entry.PreviousAttempts, entry.CreatedAt.UnixNano())
	if err != nil {
		return core.DeliveryRequeueAuditEntry{}, fmt.Errorf("audit delivery requeue: %w", err)
	}
	sequence, err := insert.LastInsertId()
	if err != nil {
		return core.DeliveryRequeueAuditEntry{}, fmt.Errorf("read delivery requeue audit sequence: %w", err)
	}
	entry.Sequence = uint64(sequence)
	if err := tx.Commit(); err != nil {
		return core.DeliveryRequeueAuditEntry{}, fmt.Errorf("commit delivery requeue: %w", err)
	}
	return entry, nil
}

func (s *Store) DeliveryRequeueAudit(ctx context.Context, limit int, before uint64) ([]core.DeliveryRequeueAuditEntry, error) {
	if limit <= 0 || limit > maxDeliveryAuditPageSize {
		return nil, fmt.Errorf("delivery audit limit must be between 1 and %d", maxDeliveryAuditPageSize)
	}
	query := `SELECT sequence, id, actor_id, actor_name, actor_role, delivery_id, event_id, previous_attempts, created_at FROM delivery_requeue_audit`
	var args []any
	if before != 0 {
		query += ` WHERE sequence < ?`
		args = append(args, before)
	}
	query += ` ORDER BY sequence DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query delivery requeue audit: %w", err)
	}
	defer rows.Close()
	entries := make([]core.DeliveryRequeueAuditEntry, 0, limit)
	for rows.Next() {
		var entry core.DeliveryRequeueAuditEntry
		var createdAt int64
		if err := rows.Scan(&entry.Sequence, &entry.ID, &entry.ActorID, &entry.ActorName, &entry.ActorRole, &entry.DeliveryID, &entry.EventID, &entry.PreviousAttempts, &createdAt); err != nil {
			return nil, fmt.Errorf("scan delivery requeue audit: %w", err)
		}
		entry.CreatedAt = time.Unix(0, createdAt).UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate delivery requeue audit: %w", err)
	}
	return entries, nil
}
