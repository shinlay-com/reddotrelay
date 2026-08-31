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

const maxDiagnosticsPageSize = 201

func (s *Store) DeliveryStatusSummary(ctx context.Context) (map[core.DeliveryStatus]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM deliveries GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("query delivery status summary: %w", err)
	}
	defer rows.Close()
	result := map[core.DeliveryStatus]int{core.DeliveryPending: 0, core.DeliveryDelivered: 0, core.DeliveryDead: 0}
	for rows.Next() {
		var status core.DeliveryStatus
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scan delivery status summary: %w", err)
		}
		result[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate delivery status summary: %w", err)
	}
	return result, nil
}

func (s *Store) EventHistory(ctx context.Context, filter core.EventHistoryFilter, limit int) ([]core.EventHistoryEntry, error) {
	if limit <= 0 || limit > maxDiagnosticsPageSize {
		return nil, fmt.Errorf("event history limit must be between 1 and %d", maxDiagnosticsPageSize)
	}
	if filter.DeliveryStatus != "" && filter.DeliveryStatus != core.DeliveryPending && filter.DeliveryStatus != core.DeliveryDelivered && filter.DeliveryStatus != core.DeliveryDead {
		return nil, errors.New("invalid delivery status filter")
	}
	var where []string
	var args []any
	if filter.ChainID != nil {
		if err := validUint64(*filter.ChainID); err != nil {
			return nil, err
		}
		where = append(where, "e.chain_id = ?")
		args = append(args, *filter.ChainID)
	}
	if filter.TransactionHash != "" {
		where = append(where, "lower(e.transaction_hash) = ?")
		args = append(args, strings.ToLower(filter.TransactionHash))
	}
	if filter.BlockNumber != nil {
		if err := validUint64(*filter.BlockNumber); err != nil {
			return nil, err
		}
		where = append(where, "e.block_number = ?")
		args = append(args, *filter.BlockNumber)
	}
	if filter.Address != "" {
		where = append(where, "lower(e.address) = ?")
		args = append(args, strings.ToLower(filter.Address))
	}
	if filter.Signature != "" {
		where = append(where, "e.signature = ?")
		args = append(args, filter.Signature)
	}
	if filter.DeliveryStatus != "" {
		where = append(where, "EXISTS (SELECT 1 FROM deliveries fd WHERE fd.chain_id = e.chain_id AND fd.transaction_hash = e.transaction_hash AND fd.log_index = e.log_index AND fd.status = ?)")
		args = append(args, filter.DeliveryStatus)
	}
	if filter.Before != nil {
		if err := validUint64(filter.Before.ChainID, filter.Before.LogIndex); err != nil {
			return nil, err
		}
		where = append(where, `(e.observed_at < ? OR (e.observed_at = ? AND (e.chain_id < ? OR (e.chain_id = ? AND (e.transaction_hash < ? OR (e.transaction_hash = ? AND e.log_index < ?))))))`)
		stamp := filter.Before.ObservedAt.UTC().UnixNano()
		args = append(args, stamp, stamp, filter.Before.ChainID, filter.Before.ChainID, filter.Before.TransactionHash, filter.Before.TransactionHash, filter.Before.LogIndex)
	}
	query := `SELECT e.event_guid, e.chain_id, e.transaction_hash, e.log_index, e.block_number, e.block_hash, e.address, e.name, e.signature, e.decoded_payload, e.observed_at,
	COALESCE(SUM(CASE WHEN d.status = 'pending' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN d.status = 'delivered' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN d.status = 'dead' THEN 1 ELSE 0 END), 0)
FROM events e LEFT JOIN deliveries d USING (chain_id, transaction_hash, log_index)`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` GROUP BY e.chain_id, e.transaction_hash, e.log_index
ORDER BY e.observed_at DESC, e.chain_id DESC, e.transaction_hash DESC, e.log_index DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query event history: %w", err)
	}
	defer rows.Close()
	entries := make([]core.EventHistoryEntry, 0, limit)
	for rows.Next() {
		var entry core.EventHistoryEntry
		var observedAt int64
		if err := rows.Scan(&entry.EventGUID, &entry.Event.ID.ChainID, &entry.Event.ID.TransactionHash, &entry.Event.ID.LogIndex, &entry.Event.BlockNumber, &entry.Event.BlockHash, &entry.Event.Address, &entry.Event.Name, &entry.Event.Signature, &entry.Event.DecodedPayload, &observedAt, &entry.Pending, &entry.Delivered, &entry.Dead); err != nil {
			return nil, fmt.Errorf("scan event history: %w", err)
		}
		entry.Event.ObservedAt = time.Unix(0, observedAt).UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate event history: %w", err)
	}
	return entries, nil
}

func (s *Store) EventDeliveries(ctx context.Context, eventGUID, afterDeliveryGUID string, limit int) ([]core.Delivery, error) {
	if limit <= 0 || limit > maxDiagnosticsPageSize {
		return nil, fmt.Errorf("delivery history limit must be between 1 and %d", maxDiagnosticsPageSize)
	}
	if strings.TrimSpace(eventGUID) == "" {
		return nil, errors.New("event ID is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.delivery_guid, d.chain_id, d.transaction_hash, d.log_index, d.destination, d.auth_type, d.auth_secret_ref, d.auth_key_id, d.status, d.attempts, d.total_attempts,
	next_attempt, last_attempt_at, last_status_code, last_error, delivered_at
FROM deliveries d JOIN events e USING (chain_id, transaction_hash, log_index)
WHERE e.event_guid = ? AND d.delivery_guid > ? ORDER BY d.delivery_guid LIMIT ?`, eventGUID, afterDeliveryGUID, limit)
	if err != nil {
		return nil, fmt.Errorf("query event deliveries: %w", err)
	}
	defer rows.Close()
	result := make([]core.Delivery, 0, limit)
	for rows.Next() {
		var delivery core.Delivery
		var nextAttempt int64
		var lastAttemptAt, lastStatusCode, deliveredAt sql.NullInt64
		if err := rows.Scan(&delivery.ID, &delivery.EventID.ChainID, &delivery.EventID.TransactionHash, &delivery.EventID.LogIndex, &delivery.Destination, &delivery.Authentication.Type, &delivery.Authentication.SecretRef, &delivery.Authentication.KeyID, &delivery.Status, &delivery.Attempts, &delivery.TotalAttempts, &nextAttempt, &lastAttemptAt, &lastStatusCode, &delivery.LastError, &deliveredAt); err != nil {
			return nil, fmt.Errorf("scan event delivery: %w", err)
		}
		delivery.NextAttempt = time.Unix(0, nextAttempt).UTC()
		if lastAttemptAt.Valid {
			value := time.Unix(0, lastAttemptAt.Int64).UTC()
			delivery.LastAttemptAt = &value
		}
		if lastStatusCode.Valid {
			delivery.LastStatusCode = int(lastStatusCode.Int64)
		}
		if deliveredAt.Valid {
			value := time.Unix(0, deliveredAt.Int64).UTC()
			delivery.DeliveredAt = &value
		}
		result = append(result, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate event deliveries: %w", err)
	}
	return result, nil
}
