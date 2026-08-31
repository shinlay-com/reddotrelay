package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"reddotrelay/internal/core"
)

const maxRPCListenerAuditPageSize = 201

func insertRPCListenerAudit(ctx context.Context, tx *sql.Tx, audit core.RPCListenerAudit, previousRevision, newRevision uint64, timestamp int64) error {
	if err := validateRPCListenerAudit(audit); err != nil {
		return err
	}
	if previousRevision > math.MaxInt64 || newRevision > math.MaxInt64 {
		return errors.New("audit revision exceeds SQLite integer range")
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO rpc_listener_audit (
    id, actor_id, actor_name, actor_role, action, resource_kind,
    resource_id, parent_rpc_listener_id, previous_revision, new_revision, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		core.NewConfigID(), audit.ActorID, audit.ActorName, audit.ActorRole,
		audit.Action, audit.ResourceKind, audit.ResourceID, audit.ParentListenerID,
		previousRevision, newRevision, timestamp)
	if err != nil {
		return fmt.Errorf("record RPC listener audit: %w", err)
	}
	return nil
}

func validateRPCListenerAudit(audit core.RPCListenerAudit) error {
	if err := validConfigID(audit.ActorID); err != nil {
		return fmt.Errorf("audit actor ID: %w", err)
	}
	if strings.TrimSpace(audit.ActorName) == "" {
		return errors.New("audit actor name is required")
	}
	if audit.ActorRole != core.APIKeyAdmin && audit.ActorRole != core.APIKeyReadOnly {
		return errors.New("audit actor role is invalid")
	}
	if audit.Action != core.AuditActionCreate && audit.Action != core.AuditActionUpdate && audit.Action != core.AuditActionDelete && audit.Action != core.AuditActionImport && audit.Action != core.AuditActionPause && audit.Action != core.AuditActionResume {
		return errors.New("audit action is invalid")
	}
	switch audit.ResourceKind {
	case core.AuditResourceRPCListener, core.AuditResourceContract, core.AuditResourceEvent, core.AuditResourceWebhook, core.AuditResourceConfiguration:
	default:
		return errors.New("audit resource kind is invalid")
	}
	if err := validConfigID(audit.ResourceID); err != nil {
		return fmt.Errorf("audit resource ID: %w", err)
	}
	if audit.ParentListenerID != "" {
		if err := validConfigID(audit.ParentListenerID); err != nil {
			return fmt.Errorf("audit parent listener ID: %w", err)
		}
	}
	if audit.ResourceKind == core.AuditResourceRPCListener && audit.ParentListenerID != "" {
		return errors.New("RPC listener audit cannot have a parent listener ID")
	}
	if audit.ResourceKind == core.AuditResourceConfiguration && (audit.Action != core.AuditActionImport || audit.ParentListenerID != "") {
		return errors.New("configuration audit must be a parentless import")
	}
	if (audit.ResourceKind == core.AuditResourceContract || audit.ResourceKind == core.AuditResourceEvent) && audit.ParentListenerID == "" {
		return errors.New("nested configuration audit requires a parent listener ID")
	}
	return nil
}

// RPCListenerAudit returns newest-first audit entries. before is an opaque
// monotonically decreasing cursor returned as Sequence on the prior page.
func (s *Store) RPCListenerAudit(ctx context.Context, limit int, before uint64) ([]core.RPCListenerAuditEntry, error) {
	if limit <= 0 || limit > maxRPCListenerAuditPageSize {
		return nil, fmt.Errorf("audit page limit must be between 1 and %d", maxRPCListenerAuditPageSize)
	}
	if before > math.MaxInt64 {
		return nil, errors.New("audit cursor exceeds SQLite integer range")
	}
	query := `
SELECT sequence, id, actor_id, actor_name, actor_role, action, resource_kind,
       resource_id, parent_rpc_listener_id, previous_revision, new_revision, created_at
FROM rpc_listener_audit`
	args := make([]any, 0, 2)
	if before != 0 {
		query += ` WHERE sequence < ?`
		args = append(args, before)
	}
	query += ` ORDER BY sequence DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query RPC listener audit: %w", err)
	}
	defer rows.Close()
	entries := make([]core.RPCListenerAuditEntry, 0, limit)
	for rows.Next() {
		var entry core.RPCListenerAuditEntry
		var createdAt int64
		if err := rows.Scan(
			&entry.Sequence, &entry.ID, &entry.ActorID, &entry.ActorName, &entry.ActorRole,
			&entry.Action, &entry.ResourceKind, &entry.ResourceID, &entry.ParentListenerID,
			&entry.PreviousRevision, &entry.NewRevision, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan RPC listener audit: %w", err)
		}
		entry.CreatedAt = time.Unix(0, createdAt).UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate RPC listener audit: %w", err)
	}
	return entries, nil
}
