package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestV03MigrationPreservesDurableState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reddotrelay-v0.3.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
PRAGMA foreign_keys = ON;
CREATE TABLE checkpoints (chain_id INTEGER PRIMARY KEY, block_number INTEGER NOT NULL, block_hash TEXT NOT NULL);
CREATE TABLE events (
 chain_id INTEGER NOT NULL, transaction_hash TEXT NOT NULL, log_index INTEGER NOT NULL,
 block_number INTEGER NOT NULL, block_hash TEXT NOT NULL, address TEXT NOT NULL, name TEXT NOT NULL,
 payload BLOB NOT NULL DEFAULT X'', signature TEXT NOT NULL DEFAULT '', raw_topics TEXT NOT NULL DEFAULT '[]',
 raw_data BLOB NOT NULL DEFAULT X'', decoded_payload BLOB NOT NULL DEFAULT X'', observed_at INTEGER NOT NULL,
 PRIMARY KEY (chain_id, transaction_hash, log_index));
CREATE TABLE deliveries (
 chain_id INTEGER NOT NULL, transaction_hash TEXT NOT NULL, log_index INTEGER NOT NULL, destination TEXT NOT NULL,
 auth_type TEXT NOT NULL DEFAULT '', auth_secret_ref TEXT NOT NULL DEFAULT '', auth_key_id TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, lease_token TEXT NOT NULL DEFAULT '',
 next_attempt INTEGER NOT NULL, last_error TEXT NOT NULL DEFAULT '', delivered_at INTEGER,
 PRIMARY KEY (chain_id, transaction_hash, log_index, destination));
CREATE TABLE rpc_listener_state (singleton INTEGER PRIMARY KEY, revision INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE rpc_listeners (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, paused INTEGER NOT NULL DEFAULT 0, chain_id INTEGER NOT NULL UNIQUE,
 rpc_url TEXT NOT NULL, start_block INTEGER NOT NULL, batch_size INTEGER NOT NULL, poll_interval INTEGER NOT NULL,
 confirmations INTEGER NOT NULL, reorg_depth INTEGER NOT NULL, rpc_retry_attempts INTEGER NOT NULL,
 rpc_retry_backoff INTEGER NOT NULL, rpc_timeout INTEGER NOT NULL, tls_ca_pem TEXT NOT NULL DEFAULT '',
 tls_server_name TEXT NOT NULL DEFAULT '', tls_insecure_skip_verify INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE api_keys (
 id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, role TEXT NOT NULL, secret_hash BLOB NOT NULL UNIQUE,
 secret_prefix TEXT NOT NULL, created_at INTEGER NOT NULL, last_used_at INTEGER, revoked_at INTEGER);
CREATE TABLE rpc_listener_audit (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, actor_id TEXT NOT NULL,
 actor_name TEXT NOT NULL, actor_role TEXT NOT NULL, action TEXT NOT NULL, resource_kind TEXT NOT NULL,
 resource_id TEXT NOT NULL, parent_rpc_listener_id TEXT NOT NULL DEFAULT '', previous_revision INTEGER NOT NULL,
 new_revision INTEGER NOT NULL, created_at INTEGER NOT NULL);
INSERT INTO checkpoints VALUES (1, 100, '0xblock');
INSERT INTO events VALUES (1, '0xabc', 7, 100, '0xblock', '0xcontract', 'Transfer', X'',
 'Transfer(address,address,uint256)', '["0xtopic"]', X'01', '{"value":"42"}', 1787556000000000000);
INSERT INTO deliveries VALUES (1, '0xabc', 7, 'https://example.test/hook', 'hmac-sha256',
 'env://HOOK_HMAC', 'key-1', 'pending', 3, 'old-lease', 1787556060000000000, 'temporary', NULL);
INSERT INTO rpc_listener_state VALUES (1, 1, 1787556000000000000);
INSERT INTO rpc_listeners VALUES ('00000000-0000-4000-8000-000000000001', 'Ethereum', 0, 1,
 'https://rpc.example.test', 0, 100, 1000000000, 12, 64, 3, 1000000000, 10000000000, '', '', 0,
 1787556000000000000, 1787556000000000000);
INSERT INTO api_keys VALUES ('00000000-0000-4000-8000-000000000002', 'admin', 'admin', zeroblob(32),
 'api_key_test', 1787556000000000000, NULL, NULL);
INSERT INTO rpc_listener_audit VALUES (1, '00000000-0000-4000-8000-000000000003',
 '00000000-0000-4000-8000-000000000002', 'admin', 'admin', 'create', 'rpc-listener',
 '00000000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-000000000001', 0, 1, 1787556000000000000);
`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store := openStore(t, path)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStore(t, path) // migration is idempotent across restart
	t.Cleanup(func() { _ = store.Close() })

	checkpoint, err := store.Checkpoint(ctx, 1)
	if err != nil || checkpoint.BlockNumber != 100 || checkpoint.BlockHash != "0xblock" {
		t.Fatalf("checkpoint after migration = %#v, %v", checkpoint, err)
	}
	items, err := store.DueDeliveries(ctx, time.Date(2026, 8, 24, 11, 0, 0, 0, time.UTC), 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("delivery after migration = %#v, %v", items, err)
	}
	if items[0].Delivery.Attempts != 3 || items[0].Delivery.TotalAttempts != 3 ||
		items[0].Delivery.Authentication.SecretRef != "env://HOOK_HMAC" ||
		items[0].Delivery.LeaseToken != "old-lease" {
		t.Fatalf("migrated delivery metadata = %#v", items[0])
	}
	var eventGUID, deliveryGUID string
	if err := store.db.QueryRowContext(ctx, `SELECT e.event_guid, d.delivery_guid FROM events e JOIN deliveries d USING (chain_id, transaction_hash, log_index) WHERE e.chain_id = 1 AND e.transaction_hash = '0xabc' AND e.log_index = 7`).Scan(&eventGUID, &deliveryGUID); err != nil || eventGUID == "" || deliveryGUID == "" {
		t.Fatalf("migrated operational GUIDs = (%q, %q), %v", eventGUID, deliveryGUID, err)
	}
	snapshot, err := store.RPCListenerSnapshot(ctx)
	if err != nil || snapshot.Revision != 1 || len(snapshot.Listeners) != 1 || snapshot.Listeners[0].Name != "Ethereum" {
		t.Fatalf("configuration after migration = %#v, %v", snapshot, err)
	}
	keys, err := store.APIKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].Name != "admin" || keys[0].Role != core.APIKeyAdmin {
		t.Fatalf("API keys after migration = %#v, %v", keys, err)
	}
	audits, err := store.RPCListenerAudit(ctx, 10, 0)
	if err != nil || len(audits) != 1 || audits[0].Action != core.AuditActionCreate {
		t.Fatalf("audit after migration = %#v, %v", audits, err)
	}
}
