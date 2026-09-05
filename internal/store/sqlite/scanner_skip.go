package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"reddotrelay/internal/core"
)

var ErrScannerSkipConflict = errors.New("listener must be paused with an unchanged checkpoint and a strictly newer target; preview again")

type ScannerSkipAudit struct {
	Sequence      uint64          `json:"sequence"`
	ID            string          `json:"id"`
	ListenerID    string          `json:"rpcListenerId"`
	ChainID       uint64          `json:"chainId"`
	ActorID       string          `json:"actorId"`
	ActorName     string          `json:"actorName"`
	ActorRole     core.APIKeyRole `json:"actorRole"`
	PreviousBlock *uint64         `json:"previousBlock"`
	PreviousHash  string          `json:"previousHash"`
	FromBlock     uint64          `json:"fromBlock"`
	ToBlock       uint64          `json:"toBlock"`
	BlockHash     string          `json:"blockHash"`
	ParentHash    string          `json:"parentHash"`
	Revision      uint64          `json:"configurationRevision"`
	CreatedAt     time.Time       `json:"createdAt"`
}

// SkipScannerToHead requires the caller to have observed the runtime stopped
// at expectedRevision. The transaction rechecks that paused revision and the
// previewed checkpoint before advancing and enabling the listener together.
// No existing event, delivery, attempt, or delivery lease is modified.
func (s *Store) SkipScannerToHead(ctx context.Context, id string, expectedRevision uint64, previous *core.Checkpoint, target core.CanonicalBlock, actor core.APIKeyPrincipal, now time.Time) (ScannerSkipAudit, error) {
	result := ScannerSkipAudit{ID: core.NewConfigID(), ListenerID: id, ChainID: target.ChainID, ActorID: actor.ID, ActorName: actor.Name, ActorRole: actor.Role, ToBlock: target.Number, BlockHash: target.Hash, ParentHash: target.ParentHash, CreatedAt: now.UTC()}
	hash, hashErr := hexutil.Decode(target.Hash)
	parent, parentErr := hexutil.Decode(target.ParentHash)
	if actor.Role != core.APIKeyAdmin || target.Number >= math.MaxInt64 || target.ChainID > math.MaxInt64 || hashErr != nil || parentErr != nil || len(hash) != 32 || len(parent) != 32 || common.HexToHash(target.Hash) == (common.Hash{}) || (target.Number > 0 && common.HexToHash(target.ParentHash) == (common.Hash{})) {
		return result, ErrScannerSkipConflict
	}
	audit := &core.RPCListenerAudit{ActorID: actor.ID, ActorName: actor.Name, ActorRole: actor.Role, Action: core.AuditActionResume, ResourceKind: core.AuditResourceRPCListener, ResourceID: id}
	revision, err := s.mutateRPCListeners(ctx, expectedRevision, now, audit, func(tx *sql.Tx, timestamp int64) error {
		var chain, start, depth uint64
		var paused bool
		if err := tx.QueryRowContext(ctx, `SELECT chain_id,start_block,reorg_depth,paused FROM rpc_listeners WHERE id=?`, id).Scan(&chain, &start, &depth, &paused); err != nil {
			return err
		}
		if !paused || chain != target.ChainID || target.Number < start {
			return ErrScannerSkipConflict
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM backfill_jobs WHERE chain_id=? AND state IN ('queued','running','paused')`, chain).Scan(&active); err != nil {
			return err
		}
		if active != 0 {
			return ErrActiveBackfill
		}
		var current core.Checkpoint
		current.ChainID = chain
		err := tx.QueryRowContext(ctx, `SELECT block_number,block_hash FROM checkpoints WHERE chain_id=?`, chain).Scan(&current.BlockNumber, &current.BlockHash)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if previous != nil {
				return ErrScannerSkipConflict
			}
			result.FromBlock = start
		case err != nil:
			return err
		default:
			if previous == nil || *previous != current || target.Number <= current.BlockNumber {
				return ErrScannerSkipConflict
			}
			result.PreviousBlock = &current.BlockNumber
			result.PreviousHash = current.BlockHash
			result.FromBlock = current.BlockNumber + 1
		}
		// A backfill may already have observed the target. Never replace its branch.
		var conflicts int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE chain_id=? AND block_number=? AND block_hash<>?`, chain, target.Number, target.Hash).Scan(&conflicts); err != nil {
			return err
		}
		if conflicts != 0 {
			return ErrScannerSkipConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints(chain_id,block_number,block_hash) VALUES(?,?,?) ON CONFLICT(chain_id) DO UPDATE SET block_number=excluded.block_number,block_hash=excluded.block_hash`, chain, target.Number, target.Hash); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO canonical_blocks(chain_id,block_number,block_hash,parent_hash) VALUES(?,?,?,?) ON CONFLICT(chain_id,block_number) DO UPDATE SET block_hash=excluded.block_hash,parent_hash=excluded.parent_hash`, chain, target.Number, target.Hash, target.ParentHash); err != nil {
			return err
		}
		if target.Number > depth {
			if _, err := tx.ExecContext(ctx, `DELETE FROM canonical_blocks WHERE chain_id=? AND block_number<?`, chain, target.Number-depth); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rpc_listeners SET paused=0,updated_at=? WHERE id=?`, timestamp, id); err != nil {
			return err
		}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO scanner_skip_audit(id,rpc_listener_id,chain_id,actor_id,actor_name,actor_role,previous_block,previous_hash,from_block,to_block,block_hash,parent_hash,config_revision,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, result.ID, id, chain, actor.ID, actor.Name, actor.Role, result.PreviousBlock, result.PreviousHash, result.FromBlock, target.Number, target.Hash, target.ParentHash, expectedRevision+1, timestamp)
		if err != nil {
			return err
		}
		sequence, err := inserted.LastInsertId()
		result.Sequence = uint64(sequence)
		return err
	})
	result.Revision = revision
	return result, err
}

func (s *Store) ScannerSkipAudit(ctx context.Context, id string, limit int, before uint64) ([]ScannerSkipAudit, error) {
	if limit < 1 || limit > 201 || before > math.MaxInt64 {
		return nil, errors.New("invalid audit page")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,id,rpc_listener_id,chain_id,actor_id,actor_name,actor_role,previous_block,previous_hash,from_block,to_block,block_hash,parent_hash,config_revision,created_at FROM scanner_skip_audit WHERE rpc_listener_id=? AND (?=0 OR sequence<?) ORDER BY sequence DESC LIMIT ?`, id, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScannerSkipAudit{}
	for rows.Next() {
		var a ScannerSkipAudit
		var prev sql.NullInt64
		var at int64
		if err := rows.Scan(&a.Sequence, &a.ID, &a.ListenerID, &a.ChainID, &a.ActorID, &a.ActorName, &a.ActorRole, &prev, &a.PreviousHash, &a.FromBlock, &a.ToBlock, &a.BlockHash, &a.ParentHash, &a.Revision, &at); err != nil {
			return nil, err
		}
		if prev.Valid {
			v := uint64(prev.Int64)
			a.PreviousBlock = &v
		}
		a.CreatedAt = time.Unix(0, at).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}
