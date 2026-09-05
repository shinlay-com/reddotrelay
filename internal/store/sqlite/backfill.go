package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"reddotrelay/internal/core"
)

var ErrActiveBackfill = errors.New("an active backfill already exists for this listener")
var ErrBackfillState = errors.New("backfill state transition is not allowed")

func (s *Store) CreateBackfill(ctx context.Context, job core.BackfillJob, actor core.APIKeyPrincipal, now time.Time) error {
	contracts, _ := json.Marshal(job.ContractIDs)
	events, _ := json.Marshal(job.EventIDs)
	destinations, _ := json.Marshal(job.Destinations)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO backfill_jobs(id,rpc_listener_id,chain_id,mode,from_block,to_block,next_block,contract_ids,event_ids,config_revision,snapshot,destinations,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, job.ID, job.ListenerID, job.ChainID, "backfill-missing", job.FromBlock, job.ToBlock, job.FromBlock, string(contracts), string(events), job.ConfigRevision, job.Snapshot, string(destinations), core.BackfillQueued, now.UnixNano(), now.UnixNano())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return ErrActiveBackfill
		}
		return fmt.Errorf("create backfill: %w", err)
	}
	if err = insertBackfillAudit(ctx, tx, job.ID, actor, "create", now); err != nil {
		return err
	}
	return tx.Commit()
}

func insertBackfillAudit(ctx context.Context, tx *sql.Tx, jobID string, actor core.APIKeyPrincipal, action string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO backfill_audit(id,job_id,actor_id,actor_name,actor_role,action,created_at) VALUES(?,?,?,?,?,?,?)`, core.NewConfigID(), jobID, actor.ID, actor.Name, actor.Role, action, now.UnixNano())
	return err
}

func (s *Store) Backfill(ctx context.Context, id string) (core.BackfillJob, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,rpc_listener_id,chain_id,mode,from_block,to_block,next_block,contract_ids,event_ids,config_revision,snapshot,destinations,state,processed_blocks,discovered_events,created_events,created_deliveries,duplicates,failure_summary,created_at,updated_at,started_at,completed_at FROM backfill_jobs WHERE id=?`, id)
	return scanBackfill(row)
}

type rowScanner interface{ Scan(...any) error }

func scanBackfill(row rowScanner) (core.BackfillJob, error) {
	var j core.BackfillJob
	var cs, es, ds string
	var created, updated int64
	var started, completed sql.NullInt64
	err := row.Scan(&j.ID, &j.ListenerID, &j.ChainID, &j.Mode, &j.FromBlock, &j.ToBlock, &j.NextBlock, &cs, &es, &j.ConfigRevision, &j.Snapshot, &ds, &j.State, &j.ProcessedBlocks, &j.DiscoveredEvents, &j.CreatedEvents, &j.CreatedDeliveries, &j.Duplicates, &j.FailureSummary, &created, &updated, &started, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	_ = json.Unmarshal([]byte(cs), &j.ContractIDs)
	_ = json.Unmarshal([]byte(es), &j.EventIDs)
	_ = json.Unmarshal([]byte(ds), &j.Destinations)
	j.CreatedAt = time.Unix(0, created).UTC()
	j.UpdatedAt = time.Unix(0, updated).UTC()
	if started.Valid {
		v := time.Unix(0, started.Int64).UTC()
		j.StartedAt = &v
	}
	if completed.Valid {
		v := time.Unix(0, completed.Int64).UTC()
		j.CompletedAt = &v
	}
	return j, nil
}

func (s *Store) ListBackfills(ctx context.Context, limit int, before string) ([]core.BackfillJob, error) {
	if limit < 1 || limit > 201 {
		return nil, errors.New("invalid limit")
	}
	q := `SELECT id,rpc_listener_id,chain_id,mode,from_block,to_block,next_block,contract_ids,event_ids,config_revision,snapshot,destinations,state,processed_blocks,discovered_events,created_events,created_deliveries,duplicates,failure_summary,created_at,updated_at,started_at,completed_at FROM backfill_jobs`
	args := []any{}
	if before != "" {
		q += ` WHERE (created_at,id)<(SELECT created_at,id FROM backfill_jobs WHERE id=?)`
		args = append(args, before)
	}
	q += ` ORDER BY created_at DESC,id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.BackfillJob
	for rows.Next() {
		j, e := scanBackfill(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) TransitionBackfill(ctx context.Context, id string, to core.BackfillState, actor core.APIKeyPrincipal, now time.Time) error {
	j, err := s.Backfill(ctx, id)
	if err != nil {
		return err
	}
	allowed := false
	switch to {
	case core.BackfillPaused:
		allowed = j.State == core.BackfillQueued || j.State == core.BackfillRunning
	case core.BackfillQueued:
		allowed = j.State == core.BackfillPaused || j.State == core.BackfillFailed
	case core.BackfillCancelled:
		allowed = j.State == core.BackfillQueued || j.State == core.BackfillRunning || j.State == core.BackfillPaused || j.State == core.BackfillFailed
	}
	if !allowed {
		return ErrBackfillState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE backfill_jobs SET state=?,failure_summary=CASE WHEN ?='queued' THEN '' ELSE failure_summary END,updated_at=?,completed_at=CASE WHEN ?='cancelled' THEN ? WHEN ?='queued' THEN NULL ELSE completed_at END WHERE id=? AND state=?`, to, to, now.UnixNano(), to, now.UnixNano(), to, id, j.State)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return ErrActiveBackfill
		}
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrBackfillState
	}
	if err = insertBackfillAudit(ctx, tx, id, actor, string(to), now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClaimBackfill(ctx context.Context, now time.Time) (core.BackfillJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.BackfillJob{}, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, `SELECT id,rpc_listener_id,chain_id,mode,from_block,to_block,next_block,contract_ids,event_ids,config_revision,snapshot,destinations,state,processed_blocks,discovered_events,created_events,created_deliveries,duplicates,failure_summary,created_at,updated_at,started_at,completed_at FROM backfill_jobs WHERE state IN ('queued','running') ORDER BY CASE state WHEN 'running' THEN 0 ELSE 1 END,created_at LIMIT 1`)
	j, err := scanBackfill(row)
	if err != nil {
		return j, err
	}
	if j.State == core.BackfillQueued {
		if err = insertBackfillAudit(ctx, tx, j.ID, core.APIKeyPrincipal{ID: "system", Name: "Engine", Role: core.APIKeyAdmin}, "running", now); err != nil {
			return j, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE backfill_jobs SET state='running',started_at=COALESCE(started_at,?),updated_at=? WHERE id=?`, now.UnixNano(), now.UnixNano(), j.ID)
	if err != nil {
		return j, err
	}
	if err = tx.Commit(); err != nil {
		return j, err
	}
	j.State = core.BackfillRunning
	return j, nil
}

func (s *Store) SaveBackfillBatch(ctx context.Context, id string, events []core.Event, deliveries []core.Delivery, next uint64, processed, discovered uint64, now time.Time) (created, createdDeliveries, duplicates uint64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	for _, event := range events {
		var canonicalHash string
		checkErr := tx.QueryRowContext(ctx, `SELECT block_hash FROM canonical_blocks WHERE chain_id=? AND block_number=?`, event.ID.ChainID, event.BlockNumber).Scan(&canonicalHash)
		if checkErr == nil && canonicalHash != event.BlockHash {
			err = errors.New("backfill event conflicts with durable canonical history")
			return
		}
		if checkErr != nil && !errors.Is(checkErr, sql.ErrNoRows) {
			err = checkErr
			return
		}
		inserted, e := insertEvent(ctx, tx, event)
		if e != nil {
			err = e
			return
		}
		if !inserted {
			duplicates++
			continue
		}
		created++
		for _, d := range deliveries {
			if d.EventID == event.ID {
				if e = insertDelivery(ctx, tx, d); e != nil {
					err = e
					return
				}
				createdDeliveries++
			}
		}
	}
	state := core.BackfillRunning
	var completed any = nil
	if next == 0 {
		state = core.BackfillCompleted
		completed = now.UnixNano()
	}
	res, e := tx.ExecContext(ctx, `UPDATE backfill_jobs SET next_block=CASE WHEN ?=0 THEN to_block+1 ELSE ? END,processed_blocks=processed_blocks+?,discovered_events=discovered_events+?,created_events=created_events+?,created_deliveries=created_deliveries+?,duplicates=duplicates+?,state=?,updated_at=?,completed_at=? WHERE id=? AND state='running'`, next, next, processed, discovered, created, createdDeliveries, duplicates, state, now.UnixNano(), completed, id)
	if e != nil {
		err = e
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		err = ErrBackfillState
		return
	}
	if next == 0 {
		if err = insertBackfillAudit(ctx, tx, id, core.APIKeyPrincipal{ID: "system", Name: "Engine", Role: core.APIKeyAdmin}, "completed", now); err != nil {
			return
		}
	}
	err = tx.Commit()
	return
}

func (s *Store) FailBackfill(ctx context.Context, id, summary string, now time.Time) error {
	if len(summary) > 240 {
		summary = summary[:240]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE backfill_jobs SET state='failed',failure_summary=?,updated_at=?,completed_at=? WHERE id=? AND state='running'`, summary, now.UnixNano(), now.UnixNano(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		if err = insertBackfillAudit(ctx, tx, id, core.APIKeyPrincipal{ID: "system", Name: "Engine", Role: core.APIKeyAdmin}, "failed", now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) BackfillAudit(ctx context.Context, limit int, before uint64) ([]core.BackfillAudit, error) {
	q := `SELECT sequence,id,job_id,actor_id,actor_name,actor_role,action,created_at FROM backfill_audit`
	args := []any{}
	if before > 0 {
		q += ` WHERE sequence<?`
		args = append(args, before)
	}
	q += ` ORDER BY sequence DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.BackfillAudit
	for rows.Next() {
		var a core.BackfillAudit
		var at int64
		if err := rows.Scan(&a.Sequence, &a.ID, &a.JobID, &a.ActorID, &a.ActorName, &a.ActorRole, &a.Action, &at); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(0, at).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}
