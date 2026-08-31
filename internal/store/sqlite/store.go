package sqlite

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"reddotrelay/internal/core"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("store record not found")
var ErrRevisionConflict = errors.New("RPC listener revision conflict")

type Store struct {
	db                 *sql.DB
	path               string
	rpcListenerChanges chan struct{}
	rpcSecretCipher    cipher.AEAD
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("SQLite path is required")
	}
	db, err := sql.Open("sqlite", connectionDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path, rpcListenerChanges: make(chan struct{}, 1)}
	if err := store.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func connectionDSN(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) DeliveryStatusCounts(ctx context.Context) (pending, delivered, dead int64, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM deliveries GROUP BY status`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count delivery statuses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return 0, 0, 0, err
		}
		switch status {
		case "pending":
			pending = count
		case "delivered":
			delivered = count
		case "dead":
			dead = count
		}
	}
	return pending, delivered, dead, rows.Err()
}

func (s *Store) initialize(ctx context.Context) error {
	const schema = `
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS checkpoints (
    chain_id INTEGER PRIMARY KEY,
    block_number INTEGER NOT NULL CHECK (block_number >= 0),
    block_hash TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS retention_settings (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    enabled INTEGER NOT NULL,
    delivered_for INTEGER NOT NULL,
    poll_interval INTEGER NOT NULL,
    batch_size INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash BLOB NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin', 'read-only')),
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    last_login_at INTEGER
);

CREATE TABLE IF NOT EXISTS ui_sessions (
    token_hash BLOB PRIMARY KEY,
    principal_id TEXT NOT NULL,
    principal_kind TEXT NOT NULL CHECK (principal_kind IN ('user','api-key')),
    csrf_token TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
    event_guid TEXT NOT NULL UNIQUE,
    chain_id INTEGER NOT NULL,
    transaction_hash TEXT NOT NULL,
    log_index INTEGER NOT NULL CHECK (log_index >= 0),
    block_number INTEGER NOT NULL CHECK (block_number >= 0),
    block_hash TEXT NOT NULL,
    address TEXT NOT NULL,
    name TEXT NOT NULL,
    payload BLOB NOT NULL DEFAULT X'',
    signature TEXT NOT NULL DEFAULT '',
    raw_topics TEXT NOT NULL DEFAULT '[]',
    raw_data BLOB NOT NULL DEFAULT X'',
    decoded_payload BLOB NOT NULL DEFAULT X'',
    observed_at INTEGER NOT NULL,
    PRIMARY KEY (chain_id, transaction_hash, log_index)
);

CREATE TABLE IF NOT EXISTS deliveries (
    delivery_guid TEXT NOT NULL UNIQUE,
    chain_id INTEGER NOT NULL,
    transaction_hash TEXT NOT NULL,
    log_index INTEGER NOT NULL,
    destination TEXT NOT NULL,
    auth_type TEXT NOT NULL DEFAULT '',
    auth_secret_ref TEXT NOT NULL DEFAULT '',
    auth_key_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('pending', 'delivered', 'dead')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    total_attempts INTEGER NOT NULL DEFAULT 0 CHECK (total_attempts >= 0),
    lease_token TEXT NOT NULL DEFAULT '',
    next_attempt INTEGER NOT NULL,
    last_attempt_at INTEGER,
    last_status_code INTEGER CHECK (last_status_code IS NULL OR (last_status_code >= 100 AND last_status_code <= 599)),
    last_error TEXT NOT NULL DEFAULT '',
    delivered_at INTEGER,
    PRIMARY KEY (chain_id, transaction_hash, log_index, destination),
    FOREIGN KEY (chain_id, transaction_hash, log_index)
        REFERENCES events (chain_id, transaction_hash, log_index)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS deliveries_due_idx
    ON deliveries (status, next_attempt);

CREATE TABLE IF NOT EXISTS rpc_listener_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    revision INTEGER NOT NULL CHECK (revision >= 0),
    updated_at INTEGER NOT NULL
);

INSERT INTO rpc_listener_state (singleton, revision, updated_at)
VALUES (1, 0, 0)
ON CONFLICT(singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS rpc_listeners (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    paused INTEGER NOT NULL DEFAULT 0 CHECK (paused IN (0, 1)),
    chain_id INTEGER NOT NULL UNIQUE CHECK (chain_id > 0),
    rpc_url TEXT NOT NULL,
	 rpc_auth_type TEXT NOT NULL DEFAULT '',
	 rpc_auth_username TEXT NOT NULL DEFAULT '',
	 rpc_auth_header_name TEXT NOT NULL DEFAULT '',
	 rpc_auth_secret BLOB NOT NULL DEFAULT X'',
	 rpc_auth_token_url TEXT NOT NULL DEFAULT '',
	 rpc_auth_token_api_key BLOB NOT NULL DEFAULT X'',
    start_block INTEGER NOT NULL CHECK (start_block >= 0),
    batch_size INTEGER NOT NULL CHECK (batch_size > 0),
    poll_interval INTEGER NOT NULL CHECK (poll_interval > 0),
    confirmations INTEGER NOT NULL CHECK (confirmations >= 0),
    reorg_depth INTEGER NOT NULL CHECK (reorg_depth > 0),
    rpc_retry_attempts INTEGER NOT NULL CHECK (rpc_retry_attempts > 0),
    rpc_retry_backoff INTEGER NOT NULL CHECK (rpc_retry_backoff > 0),
    rpc_timeout INTEGER NOT NULL CHECK (rpc_timeout > 0),
    tls_ca_pem TEXT NOT NULL DEFAULT '',
    tls_server_name TEXT NOT NULL DEFAULT '',
    tls_insecure_skip_verify INTEGER NOT NULL DEFAULT 0 CHECK (tls_insecure_skip_verify IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS contract_configs (
    id TEXT PRIMARY KEY,
    rpc_listener_id TEXT NOT NULL,
    address TEXT NOT NULL,
    abi_json BLOB NOT NULL,
    position INTEGER NOT NULL CHECK (position >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (rpc_listener_id, address),
    UNIQUE (rpc_listener_id, position),
    FOREIGN KEY (rpc_listener_id) REFERENCES rpc_listeners (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS event_configs (
    id TEXT PRIMARY KEY,
    contract_config_id TEXT NOT NULL,
    selector TEXT NOT NULL,
    position INTEGER NOT NULL CHECK (position >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (contract_config_id, selector),
    UNIQUE (contract_config_id, position),
    FOREIGN KEY (contract_config_id) REFERENCES contract_configs (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS global_webhook_configs (
    id TEXT PRIMARY KEY,
    url TEXT NOT NULL UNIQUE,
    auth_type TEXT NOT NULL DEFAULT '',
    auth_secret_ref TEXT NOT NULL DEFAULT '',
    auth_key_id TEXT NOT NULL DEFAULT '',
    position INTEGER NOT NULL UNIQUE CHECK (position >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS chain_webhook_configs (
    id TEXT PRIMARY KEY,
    rpc_listener_id TEXT NOT NULL,
    url TEXT NOT NULL,
    auth_type TEXT NOT NULL DEFAULT '',
    auth_secret_ref TEXT NOT NULL DEFAULT '',
    auth_key_id TEXT NOT NULL DEFAULT '',
    position INTEGER NOT NULL CHECK (position >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (rpc_listener_id, url),
    UNIQUE (rpc_listener_id, position),
    FOREIGN KEY (rpc_listener_id) REFERENCES rpc_listeners (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS contract_webhook_configs (
    id TEXT PRIMARY KEY,
    contract_config_id TEXT NOT NULL,
    url TEXT NOT NULL,
    auth_type TEXT NOT NULL DEFAULT '',
    auth_secret_ref TEXT NOT NULL DEFAULT '',
    auth_key_id TEXT NOT NULL DEFAULT '',
    position INTEGER NOT NULL CHECK (position >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (contract_config_id, url),
    UNIQUE (contract_config_id, position),
    FOREIGN KEY (contract_config_id) REFERENCES contract_configs (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS event_webhook_configs (
    id TEXT PRIMARY KEY,
    event_config_id TEXT NOT NULL,
    url TEXT NOT NULL,
    auth_type TEXT NOT NULL DEFAULT '',
    auth_secret_ref TEXT NOT NULL DEFAULT '',
    auth_key_id TEXT NOT NULL DEFAULT '',
    position INTEGER NOT NULL CHECK (position >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (event_config_id, url),
    UNIQUE (event_config_id, position),
    FOREIGN KEY (event_config_id) REFERENCES event_configs (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    role TEXT NOT NULL CHECK (role IN ('admin', 'read-only')),
    secret_hash BLOB NOT NULL UNIQUE CHECK (length(secret_hash) = 32),
    secret_prefix TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at INTEGER
);

CREATE TABLE IF NOT EXISTS rpc_listener_audit (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    actor_id TEXT NOT NULL,
    actor_name TEXT NOT NULL,
    actor_role TEXT NOT NULL CHECK (actor_role IN ('admin', 'read-only')),
    action TEXT NOT NULL CHECK (action IN ('create', 'update', 'delete', 'import', 'pause', 'resume')),
    resource_kind TEXT NOT NULL CHECK (resource_kind IN ('rpc-listener', 'contract-config', 'event-config', 'webhook-config', 'configuration')),
    resource_id TEXT NOT NULL,
    parent_rpc_listener_id TEXT NOT NULL DEFAULT '',
    previous_revision INTEGER NOT NULL CHECK (previous_revision >= 0),
    new_revision INTEGER NOT NULL CHECK (new_revision > previous_revision),
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS rpc_listener_audit_created_idx
    ON rpc_listener_audit (sequence DESC);

CREATE TABLE IF NOT EXISTS delivery_requeue_audit (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    actor_id TEXT NOT NULL,
    actor_name TEXT NOT NULL,
    actor_role TEXT NOT NULL CHECK (actor_role = 'admin'),
    delivery_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    previous_attempts INTEGER NOT NULL CHECK (previous_attempts >= 0),
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS delivery_requeue_audit_created_idx ON delivery_requeue_audit (sequence DESC);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("initialize SQLite schema: %w", err)
	}
	if err := s.ensureImportAuditSchema(ctx); err != nil {
		return err
	}
	if err := s.ensureTableColumn(ctx, "rpc_listeners", "paused", "INTEGER NOT NULL DEFAULT 0 CHECK (paused IN (0, 1))"); err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"rpc_auth_type", "TEXT NOT NULL DEFAULT ''"}, {"rpc_auth_username", "TEXT NOT NULL DEFAULT ''"},
		{"rpc_auth_header_name", "TEXT NOT NULL DEFAULT ''"}, {"rpc_auth_secret", "BLOB NOT NULL DEFAULT X''"},
		{"rpc_auth_token_url", "TEXT NOT NULL DEFAULT ''"}, {"rpc_auth_token_api_key", "BLOB NOT NULL DEFAULT X''"},
	} {
		if err := s.ensureTableColumn(ctx, "rpc_listeners", column.name, column.definition); err != nil {
			return err
		}
	}
	for name, definition := range map[string]string{
		"event_guid":      "TEXT NOT NULL DEFAULT ''",
		"signature":       "TEXT NOT NULL DEFAULT ''",
		"raw_topics":      "TEXT NOT NULL DEFAULT '[]'",
		"raw_data":        "BLOB NOT NULL DEFAULT X''",
		"decoded_payload": "BLOB NOT NULL DEFAULT X''",
	} {
		if err := s.ensureEventColumn(ctx, name, definition); err != nil {
			return err
		}
	}
	if err := s.ensureDeliveryColumn(ctx, "lease_token", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"delivery_guid", "TEXT NOT NULL DEFAULT ''"},
		{"auth_type", "TEXT NOT NULL DEFAULT ''"},
		{"auth_secret_ref", "TEXT NOT NULL DEFAULT ''"},
		{"auth_key_id", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureDeliveryColumn(ctx, column.name, column.definition); err != nil {
			return err
		}
		for _, table := range []string{"global_webhook_configs", "chain_webhook_configs", "contract_webhook_configs", "event_webhook_configs"} {
			if err := s.ensureTableColumn(ctx, table, column.name, column.definition); err != nil {
				return err
			}
		}
	}
	for _, column := range []struct{ name, definition string }{
		{"last_attempt_at", "INTEGER"},
		{"last_status_code", "INTEGER CHECK (last_status_code IS NULL OR (last_status_code >= 100 AND last_status_code <= 599))"},
		{"total_attempts", "INTEGER NOT NULL DEFAULT 0 CHECK (total_attempts >= 0)"},
	} {
		if err := s.ensureDeliveryColumn(ctx, column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE events SET decoded_payload = payload
WHERE length(decoded_payload) = 0 AND length(payload) > 0`); err != nil {
		return fmt.Errorf("backfill decoded event payloads: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE deliveries SET total_attempts = attempts WHERE total_attempts < attempts`); err != nil {
		return fmt.Errorf("backfill total delivery attempts: %w", err)
	}
	if err := s.backfillOperationalGUIDs(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
CREATE UNIQUE INDEX IF NOT EXISTS events_guid_idx ON events (event_guid);
CREATE UNIQUE INDEX IF NOT EXISTS deliveries_guid_idx ON deliveries (delivery_guid);
CREATE INDEX IF NOT EXISTS events_history_idx ON events (observed_at DESC, chain_id DESC, transaction_hash DESC, log_index DESC);
CREATE INDEX IF NOT EXISTS events_transaction_idx ON events (transaction_hash, observed_at DESC);
CREATE INDEX IF NOT EXISTS events_block_idx ON events (block_number, observed_at DESC);
CREATE INDEX IF NOT EXISTS events_address_idx ON events (address, observed_at DESC);
CREATE INDEX IF NOT EXISTS events_signature_idx ON events (signature, observed_at DESC);
CREATE INDEX IF NOT EXISTS deliveries_status_event_idx ON deliveries (status, chain_id, transaction_hash, log_index);`); err != nil {
		return fmt.Errorf("initialize diagnostic indexes: %w", err)
	}
	return nil
}

func (s *Store) ensureImportAuditSchema(ctx context.Context) error {
	var definition string
	if err := s.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'rpc_listener_audit'`).Scan(&definition); err != nil {
		return fmt.Errorf("inspect RPC listener audit schema: %w", err)
	}
	if strings.Contains(definition, "'import'") && strings.Contains(definition, "'configuration'") && strings.Contains(definition, "'pause'") && strings.Contains(definition, "'resume'") {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin audit schema migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE rpc_listener_audit_v3 (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    actor_id TEXT NOT NULL,
    actor_name TEXT NOT NULL,
    actor_role TEXT NOT NULL CHECK (actor_role IN ('admin', 'read-only')),
    action TEXT NOT NULL CHECK (action IN ('create', 'update', 'delete', 'import', 'pause', 'resume')),
    resource_kind TEXT NOT NULL CHECK (resource_kind IN ('rpc-listener', 'contract-config', 'event-config', 'webhook-config', 'configuration')),
    resource_id TEXT NOT NULL,
    parent_rpc_listener_id TEXT NOT NULL DEFAULT '',
    previous_revision INTEGER NOT NULL CHECK (previous_revision >= 0),
    new_revision INTEGER NOT NULL CHECK (new_revision > previous_revision),
    created_at INTEGER NOT NULL
);
INSERT INTO rpc_listener_audit_v3 SELECT * FROM rpc_listener_audit;
DROP TABLE rpc_listener_audit;
ALTER TABLE rpc_listener_audit_v3 RENAME TO rpc_listener_audit;
CREATE INDEX rpc_listener_audit_created_idx ON rpc_listener_audit (sequence DESC);`); err != nil {
		return fmt.Errorf("migrate RPC listener audit schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit audit schema migration: %w", err)
	}
	return nil
}

func (s *Store) ensureTableColumn(ctx context.Context, table, name, definition string) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var column, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &column, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		found = found || column == name
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+name+" "+definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, name, err)
	}
	return nil
}

func (s *Store) ensureEventColumn(ctx context.Context, name, definition string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(events)`)
	if err != nil {
		return fmt.Errorf("inspect events schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var column, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &column, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan events schema: %w", err)
		}
		if column == name {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close events schema rows: %w", err)
	}
	if found {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE events ADD COLUMN "+name+" "+definition); err != nil {
		return fmt.Errorf("add events.%s: %w", name, err)
	}
	return nil
}

func (s *Store) ensureDeliveryColumn(ctx context.Context, name, definition string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(deliveries)`)
	if err != nil {
		return fmt.Errorf("inspect deliveries schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var column, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &column, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan deliveries schema: %w", err)
		}
		if column == name {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close deliveries schema rows: %w", err)
	}
	if found {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE deliveries ADD COLUMN "+name+" "+definition); err != nil {
		return fmt.Errorf("add deliveries.%s: %w", name, err)
	}
	return nil
}

func (s *Store) SaveEventsAndCheckpoint(ctx context.Context, events []core.Event, deliveries []core.Delivery, checkpoint core.Checkpoint) (err error) {
	if err := validUint64(checkpoint.ChainID, checkpoint.BlockNumber); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin persistence transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	batchEvents := make(map[core.EventID]struct{}, len(events))
	insertedEvents := make(map[core.EventID]struct{}, len(events))
	for _, event := range events {
		batchEvents[event.ID] = struct{}{}
		var inserted bool
		if inserted, err = insertEvent(ctx, tx, event); err != nil {
			return err
		}
		if inserted {
			insertedEvents[event.ID] = struct{}{}
		}
	}
	for _, delivery := range deliveries {
		if _, supplied := batchEvents[delivery.EventID]; !supplied {
			return fmt.Errorf("delivery event %v is not present in persistence batch", delivery.EventID)
		}
		if _, inserted := insertedEvents[delivery.EventID]; !inserted {
			continue
		}
		if err = insertDelivery(ctx, tx, delivery); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO checkpoints (chain_id, block_number, block_hash) VALUES (?, ?, ?)
ON CONFLICT(chain_id) DO UPDATE SET
    block_number = excluded.block_number,
    block_hash = excluded.block_hash
WHERE excluded.block_number >= checkpoints.block_number`,
		checkpoint.ChainID, checkpoint.BlockNumber, checkpoint.BlockHash)
	if err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit persistence transaction: %w", err)
	}
	return nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, event core.Event) (bool, error) {
	if err := validUint64(event.ID.ChainID, event.ID.LogIndex, event.BlockNumber); err != nil {
		return false, err
	}
	if event.ID.TransactionHash == "" {
		return false, errors.New("event transaction hash is required")
	}
	rawTopics, err := json.Marshal(event.RawTopics)
	if err != nil {
		return false, fmt.Errorf("encode event raw topics: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO events (
    event_guid, chain_id, transaction_hash, log_index, block_number, block_hash,
    address, name, payload, signature, raw_topics, raw_data, decoded_payload, observed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(chain_id, transaction_hash, log_index) DO NOTHING`,
		core.EventGUID(event.ID), event.ID.ChainID, event.ID.TransactionHash, event.ID.LogIndex,
		event.BlockNumber, event.BlockHash, event.Address, event.Name,
		bytesOrEmpty(event.DecodedPayload), event.Signature, string(rawTopics), bytesOrEmpty(event.RawData), bytesOrEmpty(event.DecodedPayload),
		event.ObservedAt.UTC().UnixNano())
	if err != nil {
		return false, fmt.Errorf("save event %v: %w", event.ID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read event insert result %v: %w", event.ID, err)
	}
	if inserted == 1 {
		return true, nil
	}

	var blockNumber int64
	var blockHash, address, name, signature, storedTopics string
	var rawData, decodedPayload []byte
	err = tx.QueryRowContext(ctx, `
SELECT block_number, block_hash, address, name, signature,
       raw_topics, raw_data, decoded_payload
FROM events
WHERE chain_id = ? AND transaction_hash = ? AND log_index = ?`,
		event.ID.ChainID, event.ID.TransactionHash, event.ID.LogIndex).Scan(
		&blockNumber, &blockHash, &address, &name, &signature,
		&storedTopics, &rawData, &decodedPayload,
	)
	if err != nil {
		return false, fmt.Errorf("load duplicate event %v: %w", event.ID, err)
	}
	if blockNumber != int64(event.BlockNumber) || blockHash != event.BlockHash ||
		address != event.Address || name != event.Name || signature != event.Signature ||
		storedTopics != string(rawTopics) || !bytes.Equal(rawData, bytesOrEmpty(event.RawData)) ||
		!bytes.Equal(decodedPayload, bytesOrEmpty(event.DecodedPayload)) {
		return false, fmt.Errorf("event identity %v conflicts with different persisted event", event.ID)
	}
	return false, nil
}

func bytesOrEmpty(value []byte) []byte {
	if value == nil {
		return []byte{}
	}
	return value
}

func insertDelivery(ctx context.Context, tx *sql.Tx, delivery core.Delivery) error {
	if err := validUint64(delivery.EventID.ChainID, delivery.EventID.LogIndex); err != nil {
		return err
	}
	if delivery.EventID.TransactionHash == "" || delivery.Destination == "" {
		return errors.New("delivery event transaction hash and destination are required")
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO deliveries (
    delivery_guid, chain_id, transaction_hash, log_index, destination, auth_type, auth_secret_ref, auth_key_id, status,
    attempts, next_attempt, last_error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0, ?, '')
ON CONFLICT(chain_id, transaction_hash, log_index, destination) DO NOTHING`,
		core.DeliveryGUID(delivery.EventID, delivery.Destination), delivery.EventID.ChainID, delivery.EventID.TransactionHash,
		delivery.EventID.LogIndex, delivery.Destination, delivery.Authentication.Type, delivery.Authentication.SecretRef, delivery.Authentication.KeyID,
		delivery.NextAttempt.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("save delivery for event %v: %w", delivery.EventID, err)
	}
	return nil
}

func (s *Store) Checkpoint(ctx context.Context, chainID uint64) (core.Checkpoint, error) {
	if err := validUint64(chainID); err != nil {
		return core.Checkpoint{}, err
	}
	var checkpoint core.Checkpoint
	err := s.db.QueryRowContext(ctx,
		`SELECT chain_id, block_number, block_hash FROM checkpoints WHERE chain_id = ?`, chainID).
		Scan(&checkpoint.ChainID, &checkpoint.BlockNumber, &checkpoint.BlockHash)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Checkpoint{}, core.ErrCheckpointNotFound
	}
	if err != nil {
		return core.Checkpoint{}, fmt.Errorf("load checkpoint: %w", err)
	}
	return checkpoint, nil
}

func (s *Store) Rewind(ctx context.Context, checkpoint core.Checkpoint) (err error) {
	if err := validUint64(checkpoint.ChainID, checkpoint.BlockNumber); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rewind transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM events WHERE chain_id = ? AND block_number > ?`,
		checkpoint.ChainID, checkpoint.BlockNumber); err != nil {
		return fmt.Errorf("delete orphaned events: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE checkpoints SET block_number = ?, block_hash = ? WHERE chain_id = ?`,
		checkpoint.BlockNumber, checkpoint.BlockHash, checkpoint.ChainID)
	if err != nil {
		return fmt.Errorf("rewind checkpoint: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read rewind result: %w", err)
	}
	if changed != 1 {
		return core.ErrCheckpointNotFound
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit rewind transaction: %w", err)
	}
	return nil
}

// ResetFrom removes the checkpoint and all events at or after block in one
// transaction. It is used when the scanner cannot prove an earlier common
// ancestor after a reorganization.
func (s *Store) ResetFrom(ctx context.Context, chainID, block uint64) (err error) {
	if err := validUint64(chainID, block); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin chain reset transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM events WHERE chain_id = ? AND block_number >= ?`, chainID, block); err != nil {
		return fmt.Errorf("delete reset events: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM checkpoints WHERE chain_id = ?`, chainID)
	if err != nil {
		return fmt.Errorf("delete reset checkpoint: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read reset checkpoint result: %w", err)
	}
	if changed != 1 {
		return core.ErrCheckpointNotFound
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit chain reset transaction: %w", err)
	}
	return nil
}

func (s *Store) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]core.OutboxItem, error) {
	return s.deliveries(ctx, now, limit)
}

func (s *Store) DeadDeliveries(ctx context.Context, limit int) ([]core.OutboxItem, error) {
	if limit <= 0 {
		return nil, errors.New("dead-letter query limit must be greater than zero")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT chain_id, transaction_hash, log_index, destination
FROM deliveries
WHERE status = 'dead'
ORDER BY chain_id, transaction_hash, log_index, destination
LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query dead-letter deliveries: %w", err)
	}
	type key struct {
		id          core.EventID
		destination string
	}
	keys := make([]key, 0, limit)
	for rows.Next() {
		var candidate key
		if err := rows.Scan(&candidate.id.ChainID, &candidate.id.TransactionHash, &candidate.id.LogIndex, &candidate.destination); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan dead-letter delivery: %w", err)
		}
		keys = append(keys, candidate)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close dead-letter deliveries: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dead-letter deliveries: %w", err)
	}
	items := make([]core.OutboxItem, 0, len(keys))
	for _, candidate := range keys {
		item, err := loadOutboxItem(ctx, s.db, candidate.id, candidate.destination)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Store) RequeueDeadByGUID(ctx context.Context, guid string, now time.Time) (count int64, err error) {
	guid = strings.ToLower(strings.TrimSpace(guid))
	if guid == "" {
		return 0, errors.New("event GUID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin dead-letter requeue transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.QueryContext(ctx, `
SELECT chain_id, transaction_hash, log_index, destination
FROM deliveries WHERE status = 'dead'`)
	if err != nil {
		return 0, fmt.Errorf("query dead letters for requeue: %w", err)
	}
	type key struct {
		id          core.EventID
		destination string
	}
	var matches []key
	for rows.Next() {
		var candidate key
		if err := rows.Scan(&candidate.id.ChainID, &candidate.id.TransactionHash, &candidate.id.LogIndex, &candidate.destination); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan dead letter for requeue: %w", err)
		}
		if core.EventGUID(candidate.id) == guid {
			matches = append(matches, candidate)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close dead letters for requeue: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate dead letters for requeue: %w", err)
	}
	for _, candidate := range matches {
		result, err := tx.ExecContext(ctx, `
UPDATE deliveries SET status = 'pending', attempts = 0, lease_token = '',
    next_attempt = ?, last_error = '', delivered_at = NULL
WHERE chain_id = ? AND transaction_hash = ? AND log_index = ?
  AND destination = ? AND status = 'dead'`, now.UTC().UnixNano(),
			candidate.id.ChainID, candidate.id.TransactionHash, candidate.id.LogIndex, candidate.destination)
		if err != nil {
			return 0, fmt.Errorf("requeue dead-letter delivery: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("read dead-letter requeue result: %w", err)
		}
		count += changed
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit dead-letter requeue: %w", err)
	}
	return count, nil
}

func (s *Store) RequeueAllDead(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE deliveries SET status = 'pending', attempts = 0, lease_token = '',
    next_attempt = ?, last_error = '', delivered_at = NULL
WHERE status = 'dead'`, now.UTC().UnixNano())
	if err != nil {
		return 0, fmt.Errorf("requeue all dead-letter deliveries: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read requeue-all result: %w", err)
	}
	return count, nil
}

// CountDeliveredBefore returns the number of events eligible for retention
// pruning. An event is eligible only when it has deliveries and every delivery
// reached the delivered state before cutoff.
func (s *Store) CountDeliveredBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM events AS e
WHERE EXISTS (
    SELECT 1 FROM deliveries AS d
    WHERE d.chain_id = e.chain_id
      AND d.transaction_hash = e.transaction_hash
      AND d.log_index = e.log_index
)
AND NOT EXISTS (
    SELECT 1 FROM deliveries AS d
    WHERE d.chain_id = e.chain_id
      AND d.transaction_hash = e.transaction_hash
      AND d.log_index = e.log_index
      AND (d.status <> 'delivered' OR d.delivered_at IS NULL OR d.delivered_at >= ?)
)`, cutoff.UTC().UnixNano()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count delivered events before retention cutoff: %w", err)
	}
	return count, nil
}

// PruneDeliveredBefore atomically removes at most limit retention-eligible
// events and their delivered outbox rows. Bounding each write transaction lets
// the live scanner and delivery workers make progress between prune batches.
// Foreign-key cascading performs the outbox deletion; scanner checkpoints are
// intentionally unaffected.
func (s *Store) PruneDeliveredBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, errors.New("retention prune limit must be greater than zero")
	}
	result, err := s.db.ExecContext(ctx, `
DELETE FROM events
WHERE rowid IN (
    SELECT e.rowid
    FROM events AS e
    WHERE EXISTS (
        SELECT 1 FROM deliveries AS d
        WHERE d.chain_id = e.chain_id
          AND d.transaction_hash = e.transaction_hash
          AND d.log_index = e.log_index
    )
    AND NOT EXISTS (
        SELECT 1 FROM deliveries AS d
        WHERE d.chain_id = e.chain_id
          AND d.transaction_hash = e.transaction_hash
          AND d.log_index = e.log_index
          AND (d.status <> 'delivered' OR d.delivered_at IS NULL OR d.delivered_at >= ?)
    )
    ORDER BY e.observed_at, e.chain_id, e.transaction_hash, e.log_index
    LIMIT ?
)`, cutoff.UTC().UnixNano(), limit)
	if err != nil {
		return 0, fmt.Errorf("prune delivered events before retention cutoff: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read retention prune result: %w", err)
	}
	return count, nil
}

// ClaimDueDeliveries atomically leases pending outbox rows by moving their
// next-attempt time forward. If the process stops before a transition is
// recorded, the lease expires and a subsequent process can safely retry it.
func (s *Store) ClaimDueDeliveries(ctx context.Context, now time.Time, lease time.Duration, limit int) (items []core.OutboxItem, err error) {
	if limit <= 0 {
		return nil, errors.New("delivery claim limit must be greater than zero")
	}
	if lease <= 0 {
		return nil, errors.New("delivery lease must be greater than zero")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin delivery claim transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.QueryContext(ctx, `
SELECT chain_id, transaction_hash, log_index, destination
FROM deliveries
WHERE status = 'pending' AND next_attempt <= ?
ORDER BY next_attempt, chain_id, transaction_hash, log_index, destination
LIMIT ?`, now.UTC().UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("select due delivery claims: %w", err)
	}
	type key struct {
		id          core.EventID
		destination string
	}
	keys := make([]key, 0, limit)
	for rows.Next() {
		var candidate key
		if err := rows.Scan(&candidate.id.ChainID, &candidate.id.TransactionHash, &candidate.id.LogIndex, &candidate.destination); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan due delivery claim: %w", err)
		}
		keys = append(keys, candidate)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close due delivery claims: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due delivery claims: %w", err)
	}

	leaseUntil := now.Add(lease).UTC().UnixNano()
	items = make([]core.OutboxItem, 0, len(keys))
	for _, candidate := range keys {
		leaseToken, err := newLeaseToken()
		if err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `
UPDATE deliveries SET attempts = attempts + 1, total_attempts = total_attempts + 1, next_attempt = ?, last_attempt_at = ?, lease_token = ?
WHERE chain_id = ? AND transaction_hash = ? AND log_index = ?
  AND destination = ? AND status = 'pending' AND next_attempt <= ?`,
			leaseUntil, now.UTC().UnixNano(), leaseToken, candidate.id.ChainID, candidate.id.TransactionHash, candidate.id.LogIndex,
			candidate.destination, now.UTC().UnixNano())
		if err != nil {
			return nil, fmt.Errorf("claim delivery: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("read delivery claim result: %w", err)
		}
		if changed != 1 {
			continue
		}
		item, err := loadOutboxItem(ctx, tx, candidate.id, candidate.destination)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit delivery claims: %w", err)
	}
	return items, nil
}

func newLeaseToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate delivery lease token: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *Store) deliveries(ctx context.Context, now time.Time, limit int) ([]core.OutboxItem, error) {
	if limit <= 0 {
		return nil, errors.New("delivery query limit must be greater than zero")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT d.chain_id, d.transaction_hash, d.log_index, d.destination,
       d.auth_type, d.auth_secret_ref, d.auth_key_id,
       d.status, d.attempts, d.total_attempts, d.lease_token, d.next_attempt, d.last_attempt_at, d.last_status_code, d.last_error, d.delivered_at,
       e.block_number, e.block_hash, e.address, e.name, e.signature,
       e.raw_topics, e.raw_data, e.decoded_payload, e.observed_at
FROM deliveries d
JOIN events e USING (chain_id, transaction_hash, log_index)
WHERE d.status = 'pending' AND d.next_attempt <= ?
ORDER BY d.next_attempt, d.chain_id, d.transaction_hash, d.log_index, d.destination
LIMIT ?`, now.UTC().UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("query due deliveries: %w", err)
	}
	defer rows.Close()

	items := make([]core.OutboxItem, 0)
	for rows.Next() {
		var item core.OutboxItem
		var nextAttempt, observedAt int64
		var deliveredAt, lastAttemptAt, lastStatusCode sql.NullInt64
		var rawTopics string
		err := rows.Scan(
			&item.Delivery.EventID.ChainID, &item.Delivery.EventID.TransactionHash,
			&item.Delivery.EventID.LogIndex, &item.Delivery.Destination,
			&item.Delivery.Authentication.Type, &item.Delivery.Authentication.SecretRef, &item.Delivery.Authentication.KeyID,
			&item.Delivery.Status, &item.Delivery.Attempts, &item.Delivery.TotalAttempts, &item.Delivery.LeaseToken, &nextAttempt, &lastAttemptAt, &lastStatusCode,
			&item.Delivery.LastError, &deliveredAt, &item.Event.BlockNumber,
			&item.Event.BlockHash, &item.Event.Address, &item.Event.Name,
			&item.Event.Signature, &rawTopics, &item.Event.RawData,
			&item.Event.DecodedPayload, &observedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan due delivery: %w", err)
		}
		item.Event.ID = item.Delivery.EventID
		item.Delivery.NextAttempt = time.Unix(0, nextAttempt).UTC()
		if lastAttemptAt.Valid {
			value := time.Unix(0, lastAttemptAt.Int64).UTC()
			item.Delivery.LastAttemptAt = &value
		}
		if lastStatusCode.Valid {
			item.Delivery.LastStatusCode = int(lastStatusCode.Int64)
		}
		item.Event.ObservedAt = time.Unix(0, observedAt).UTC()
		if err := json.Unmarshal([]byte(rawTopics), &item.Event.RawTopics); err != nil {
			return nil, fmt.Errorf("decode stored raw topics: %w", err)
		}
		if deliveredAt.Valid {
			value := time.Unix(0, deliveredAt.Int64).UTC()
			item.Delivery.DeliveredAt = &value
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due deliveries: %w", err)
	}
	return items, nil
}

type outboxQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadOutboxItem(ctx context.Context, query outboxQuerier, id core.EventID, destination string) (core.OutboxItem, error) {
	var item core.OutboxItem
	var nextAttempt, observedAt int64
	var deliveredAt, lastAttemptAt, lastStatusCode sql.NullInt64
	var rawTopics string
	err := query.QueryRowContext(ctx, `
SELECT d.chain_id, d.transaction_hash, d.log_index, d.destination,
       d.auth_type, d.auth_secret_ref, d.auth_key_id,
       d.status, d.attempts, d.total_attempts, d.lease_token, d.next_attempt, d.last_attempt_at, d.last_status_code, d.last_error, d.delivered_at,
       e.block_number, e.block_hash, e.address, e.name, e.signature,
       e.raw_topics, e.raw_data, e.decoded_payload, e.observed_at
FROM deliveries d
JOIN events e USING (chain_id, transaction_hash, log_index)
WHERE d.chain_id = ? AND d.transaction_hash = ? AND d.log_index = ? AND d.destination = ?`,
		id.ChainID, id.TransactionHash, id.LogIndex, destination).Scan(
		&item.Delivery.EventID.ChainID, &item.Delivery.EventID.TransactionHash,
		&item.Delivery.EventID.LogIndex, &item.Delivery.Destination,
		&item.Delivery.Authentication.Type, &item.Delivery.Authentication.SecretRef, &item.Delivery.Authentication.KeyID,
		&item.Delivery.Status, &item.Delivery.Attempts, &item.Delivery.TotalAttempts, &item.Delivery.LeaseToken, &nextAttempt, &lastAttemptAt, &lastStatusCode,
		&item.Delivery.LastError, &deliveredAt, &item.Event.BlockNumber,
		&item.Event.BlockHash, &item.Event.Address, &item.Event.Name,
		&item.Event.Signature, &rawTopics, &item.Event.RawData,
		&item.Event.DecodedPayload, &observedAt,
	)
	if err != nil {
		return core.OutboxItem{}, fmt.Errorf("load claimed delivery: %w", err)
	}
	item.Event.ID = item.Delivery.EventID
	item.Delivery.NextAttempt = time.Unix(0, nextAttempt).UTC()
	if lastAttemptAt.Valid {
		value := time.Unix(0, lastAttemptAt.Int64).UTC()
		item.Delivery.LastAttemptAt = &value
	}
	if lastStatusCode.Valid {
		item.Delivery.LastStatusCode = int(lastStatusCode.Int64)
	}
	item.Event.ObservedAt = time.Unix(0, observedAt).UTC()
	if err := json.Unmarshal([]byte(rawTopics), &item.Event.RawTopics); err != nil {
		return core.OutboxItem{}, fmt.Errorf("decode claimed raw topics: %w", err)
	}
	if deliveredAt.Valid {
		value := time.Unix(0, deliveredAt.Int64).UTC()
		item.Delivery.DeliveredAt = &value
	}
	return item, nil
}

func (s *Store) MarkDeliveryDelivered(ctx context.Context, id core.EventID, destination, leaseToken string, at time.Time, statusCode int) error {
	return s.transition(ctx, id, destination, leaseToken, `
UPDATE deliveries SET status = 'delivered',
    delivered_at = ?, last_status_code = ?, last_error = '', lease_token = ''
WHERE chain_id = ? AND transaction_hash = ? AND log_index = ?
  AND destination = ? AND lease_token = ? AND status = 'pending'`, at.UTC().UnixNano(), nullableHTTPStatus(statusCode))
}

func (s *Store) ScheduleDeliveryRetry(ctx context.Context, id core.EventID, destination, leaseToken string, next time.Time, lastError string, statusCode int) error {
	return s.transition(ctx, id, destination, leaseToken, `
UPDATE deliveries SET next_attempt = ?, last_error = ?, last_status_code = ?, lease_token = ''
WHERE chain_id = ? AND transaction_hash = ? AND log_index = ?
  AND destination = ? AND lease_token = ? AND status = 'pending'`, next.UTC().UnixNano(), lastError, nullableHTTPStatus(statusCode))
}

func (s *Store) MarkDeliveryDead(ctx context.Context, id core.EventID, destination, leaseToken, lastError string, statusCode int) error {
	return s.transition(ctx, id, destination, leaseToken, `
UPDATE deliveries SET status = 'dead', last_error = ?, last_status_code = ?, lease_token = ''
WHERE chain_id = ? AND transaction_hash = ? AND log_index = ?
  AND destination = ? AND lease_token = ? AND status = 'pending'`, lastError, nullableHTTPStatus(statusCode))
}

func nullableHTTPStatus(statusCode int) any {
	if statusCode >= 100 && statusCode <= 599 {
		return statusCode
	}
	return nil
}

func (s *Store) transition(ctx context.Context, id core.EventID, destination, leaseToken, query string, args ...any) error {
	if err := validUint64(id.ChainID, id.LogIndex); err != nil {
		return err
	}
	if leaseToken == "" {
		return errors.New("delivery lease token is required")
	}
	args = append(args, id.ChainID, id.TransactionHash, id.LogIndex, destination, leaseToken)
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("transition delivery: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read transition result: %w", err)
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func validUint64(values ...uint64) error {
	for _, value := range values {
		if value > math.MaxInt64 {
			return fmt.Errorf("value %d exceeds SQLite integer range", value)
		}
	}
	return nil
}

var _ core.Store = (*Store)(nil)
