package sqlite

import (
	"context"
	"fmt"

	"reddotrelay/internal/core"
)

func (s *Store) backfillOperationalGUIDs(ctx context.Context) error {
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT chain_id, transaction_hash, log_index FROM events WHERE event_guid = '' LIMIT 500`)
		if err != nil {
			return fmt.Errorf("query event GUID backfill: %w", err)
		}
		var ids []core.EventID
		for rows.Next() {
			var id core.EventID
			if err := rows.Scan(&id.ChainID, &id.TransactionHash, &id.LogIndex); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan event GUID backfill: %w", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close event GUID backfill: %w", err)
		}
		if len(ids) == 0 {
			break
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin event GUID backfill: %w", err)
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE events SET event_guid = ? WHERE chain_id = ? AND transaction_hash = ? AND log_index = ? AND event_guid = ''`, core.EventGUID(id), id.ChainID, id.TransactionHash, id.LogIndex); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("backfill event GUID: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit event GUID backfill: %w", err)
		}
	}
	type key struct {
		id          core.EventID
		destination string
	}
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT chain_id, transaction_hash, log_index, destination FROM deliveries WHERE delivery_guid = '' LIMIT 500`)
		if err != nil {
			return fmt.Errorf("query delivery GUID backfill: %w", err)
		}
		var keys []key
		for rows.Next() {
			var candidate key
			if err := rows.Scan(&candidate.id.ChainID, &candidate.id.TransactionHash, &candidate.id.LogIndex, &candidate.destination); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan delivery GUID backfill: %w", err)
			}
			keys = append(keys, candidate)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close delivery GUID backfill: %w", err)
		}
		if len(keys) == 0 {
			break
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin delivery GUID backfill: %w", err)
		}
		for _, candidate := range keys {
			if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET delivery_guid = ? WHERE chain_id = ? AND transaction_hash = ? AND log_index = ? AND destination = ? AND delivery_guid = ''`, core.DeliveryGUID(candidate.id, candidate.destination), candidate.id.ChainID, candidate.id.TransactionHash, candidate.id.LogIndex, candidate.destination); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("backfill delivery GUID: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit delivery GUID backfill: %w", err)
		}
	}
	return nil
}
