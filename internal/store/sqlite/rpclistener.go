package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/rpcauth"
	"reddotrelay/internal/secrets"

	"github.com/google/uuid"
)

// RPCListenerSnapshot loads the complete configuration in stable display
// order. It is the persistence boundary consumed by the runtime manager.
func (s *Store) RPCListenerSnapshot(ctx context.Context) (core.RPCListenerSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return core.RPCListenerSnapshot{}, fmt.Errorf("begin RPC listener snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	revision, updatedAt, err := s.rpcListenerRevision(ctx, tx)
	if err != nil {
		return core.RPCListenerSnapshot{}, err
	}
	listeners, err := s.loadListeners(ctx, tx, "", false)
	if err != nil {
		return core.RPCListenerSnapshot{}, err
	}
	global, err := loadWebhooks(ctx, tx, `
SELECT id, url, auth_type, auth_secret_ref, auth_key_id, created_at, updated_at
FROM global_webhook_configs ORDER BY position`)
	if err != nil {
		return core.RPCListenerSnapshot{}, fmt.Errorf("load global webhook configurations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return core.RPCListenerSnapshot{}, fmt.Errorf("commit RPC listener snapshot: %w", err)
	}
	return core.RPCListenerSnapshot{Revision: revision, UpdatedAt: updatedAt, GlobalWebhooks: global, Listeners: listeners}, nil
}

// RPCListenerChanges emits a coalesced notification after each durable
// configuration commit. Consumers must always reload the complete snapshot.
func (s *Store) RPCListenerChanges() <-chan struct{} { return s.rpcListenerChanges }

func (s *Store) notifyRPCListenerChange() {
	select {
	case s.rpcListenerChanges <- struct{}{}:
	default:
	}
}

func (s *Store) RPCListener(ctx context.Context, id string) (core.RPCListener, uint64, error) {
	if err := validConfigID(id); err != nil {
		return core.RPCListener{}, 0, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return core.RPCListener{}, 0, fmt.Errorf("begin RPC listener read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	revision, _, err := s.rpcListenerRevision(ctx, tx)
	if err != nil {
		return core.RPCListener{}, 0, err
	}
	listeners, err := s.loadListeners(ctx, tx, id, true)
	if err != nil {
		return core.RPCListener{}, 0, err
	}
	if len(listeners) == 0 {
		return core.RPCListener{}, revision, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return core.RPCListener{}, 0, fmt.Errorf("commit RPC listener read: %w", err)
	}
	return listeners[0], revision, nil
}

// CreateRPCListener atomically creates a chain and all nested resources.
// expectedRevision prevents a stale administrator from overwriting changes.
func (s *Store) CreateRPCListener(ctx context.Context, config core.RPCListener, expectedRevision uint64, now time.Time) (uint64, error) {
	return s.CreateRPCListenerAudited(ctx, config, expectedRevision, now, nil)
}

func (s *Store) CreateRPCListenerAudited(ctx context.Context, config core.RPCListener, expectedRevision uint64, now time.Time, audit *core.RPCListenerAudit) (uint64, error) {
	if err := validateStoredListener(config); err != nil {
		return 0, err
	}
	return s.mutateRPCListeners(ctx, expectedRevision, now, audit, func(tx *sql.Tx, timestamp int64) error {
		return s.insertListener(ctx, tx, config, timestamp, false)
	})
}

// ReplaceRPCListener atomically replaces one complete aggregate. CreatedAt
// values are retained for existing UUIDs while newly added nested UUIDs use now.
func (s *Store) ReplaceRPCListener(ctx context.Context, config core.RPCListener, expectedRevision uint64, now time.Time) (uint64, error) {
	return s.ReplaceRPCListenerAudited(ctx, config, expectedRevision, now, nil)
}

func (s *Store) ReplaceRPCListenerAudited(ctx context.Context, config core.RPCListener, expectedRevision uint64, now time.Time, audit *core.RPCListenerAudit) (uint64, error) {
	if err := validateStoredListener(config); err != nil {
		return 0, err
	}
	return s.mutateRPCListeners(ctx, expectedRevision, now, audit, func(tx *sql.Tx, timestamp int64) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM rpc_listeners WHERE id = ?`, config.ID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("find RPC listener: %w", err)
		}
		created := make(map[string]int64)
		for _, table := range []string{"rpc_listeners", "contract_configs", "event_configs", "chain_webhook_configs", "contract_webhook_configs", "event_webhook_configs"} {
			rows, err := tx.QueryContext(ctx, "SELECT id, created_at FROM "+table)
			if err != nil {
				return fmt.Errorf("load %s creation times: %w", table, err)
			}
			for rows.Next() {
				var id string
				var at int64
				if err := rows.Scan(&id, &at); err != nil {
					_ = rows.Close()
					return err
				}
				created[id] = at
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rpc_listeners WHERE id = ?`, config.ID); err != nil {
			return fmt.Errorf("delete replaced RPC listener: %w", err)
		}
		return s.insertListenerWithCreationTimes(ctx, tx, config, timestamp, created)
	})
}

func (s *Store) DeleteRPCListener(ctx context.Context, id string, expectedRevision uint64, now time.Time) (uint64, error) {
	return s.DeleteRPCListenerAudited(ctx, id, expectedRevision, now, nil)
}

func (s *Store) DeleteRPCListenerAudited(ctx context.Context, id string, expectedRevision uint64, now time.Time, audit *core.RPCListenerAudit) (uint64, error) {
	if err := validConfigID(id); err != nil {
		return 0, err
	}
	return s.mutateRPCListeners(ctx, expectedRevision, now, audit, func(tx *sql.Tx, _ int64) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM rpc_listeners WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete RPC listener: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read RPC listener delete result: %w", err)
		}
		if count == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *Store) ReplaceGlobalWebhooks(ctx context.Context, webhooks []core.WebhookConfig, expectedRevision uint64, now time.Time) (uint64, error) {
	return s.ReplaceGlobalWebhooksAudited(ctx, webhooks, expectedRevision, now, nil)
}

func (s *Store) ReplaceGlobalWebhooksAudited(ctx context.Context, webhooks []core.WebhookConfig, expectedRevision uint64, now time.Time, audit *core.RPCListenerAudit) (uint64, error) {
	if err := validateWebhooks(webhooks); err != nil {
		return 0, err
	}
	return s.mutateRPCListeners(ctx, expectedRevision, now, audit, func(tx *sql.Tx, timestamp int64) error {
		created := make(map[string]int64)
		rows, err := tx.QueryContext(ctx, `SELECT id, created_at FROM global_webhook_configs`)
		if err != nil {
			return fmt.Errorf("load global webhook creation times: %w", err)
		}
		for rows.Next() {
			var id string
			var at int64
			if err := rows.Scan(&id, &at); err != nil {
				_ = rows.Close()
				return err
			}
			created[id] = at
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM global_webhook_configs`); err != nil {
			return fmt.Errorf("replace global webhook configurations: %w", err)
		}
		return insertWebhooks(ctx, tx, "global_webhook_configs", "", "", webhooks, timestamp, created)
	})
}

// ReplaceRPCListenerSnapshotAudited atomically replaces the complete desired
// configuration, advances the global revision once, and records one import
// audit entry. Operational events, deliveries, and checkpoints are untouched.
func (s *Store) ReplaceRPCListenerSnapshotAudited(ctx context.Context, snapshot core.RPCListenerSnapshot, expectedRevision uint64, now time.Time, audit *core.RPCListenerAudit) (uint64, error) {
	if err := validateWebhooks(snapshot.GlobalWebhooks); err != nil {
		return 0, fmt.Errorf("global webhooks: %w", err)
	}
	seenListeners := make(map[string]struct{}, len(snapshot.Listeners))
	for _, listener := range snapshot.Listeners {
		if _, exists := seenListeners[listener.ID]; exists {
			return 0, fmt.Errorf("duplicate RPC listener ID %s", listener.ID)
		}
		seenListeners[listener.ID] = struct{}{}
		if err := validateStoredListener(listener); err != nil {
			return 0, err
		}
	}
	return s.mutateRPCListeners(ctx, expectedRevision, now, audit, func(tx *sql.Tx, timestamp int64) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM global_webhook_configs`); err != nil {
			return fmt.Errorf("delete imported global webhooks: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rpc_listeners`); err != nil {
			return fmt.Errorf("delete imported RPC listeners: %w", err)
		}
		if err := insertWebhooks(ctx, tx, "global_webhook_configs", "", "", snapshot.GlobalWebhooks, timestamp, nil); err != nil {
			return err
		}
		for _, listener := range snapshot.Listeners {
			if err := s.insertListener(ctx, tx, listener, timestamp, false); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) mutateRPCListeners(ctx context.Context, expectedRevision uint64, now time.Time, audit *core.RPCListenerAudit, mutation func(*sql.Tx, int64) error) (revision uint64, err error) {
	if expectedRevision > math.MaxInt64 {
		return 0, fmt.Errorf("revision %d exceeds SQLite integer range", expectedRevision)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin RPC listener transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	current, _, err := s.rpcListenerRevision(ctx, tx)
	if err != nil {
		return 0, err
	}
	if current != expectedRevision {
		return current, ErrRevisionConflict
	}
	timestamp := now.UTC().UnixNano()
	if err := mutation(tx, timestamp); err != nil {
		return current, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE rpc_listener_state SET revision = revision + 1, updated_at = ?
WHERE singleton = 1 AND revision = ?`, timestamp, current)
	if err != nil {
		return current, fmt.Errorf("advance RPC listener revision: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return current, fmt.Errorf("read RPC listener revision result: %w", err)
	}
	if changed != 1 {
		return current, ErrRevisionConflict
	}
	if audit != nil {
		if err := insertRPCListenerAudit(ctx, tx, *audit, current, current+1, timestamp); err != nil {
			return current, err
		}
	}
	if err = tx.Commit(); err != nil {
		return current, fmt.Errorf("commit RPC listener transaction: %w", err)
	}
	s.notifyRPCListenerChange()
	return current + 1, nil
}

type revisionQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) rpcListenerRevision(ctx context.Context, query revisionQuerier) (uint64, time.Time, error) {
	var revision uint64
	var updatedAt int64
	if err := query.QueryRowContext(ctx, `SELECT revision, updated_at FROM rpc_listener_state WHERE singleton = 1`).Scan(&revision, &updatedAt); err != nil {
		return 0, time.Time{}, fmt.Errorf("load RPC listener revision: %w", err)
	}
	if updatedAt == 0 {
		return revision, time.Time{}, nil
	}
	return revision, time.Unix(0, updatedAt).UTC(), nil
}

func (s *Store) insertListener(ctx context.Context, tx *sql.Tx, config core.RPCListener, timestamp int64, _ bool) error {
	return s.insertListenerWithCreationTimes(ctx, tx, config, timestamp, nil)
}

func (s *Store) insertListenerWithCreationTimes(ctx context.Context, tx *sql.Tx, config core.RPCListener, timestamp int64, created map[string]int64) error {
	createdAt := creationTime(created, config.ID, timestamp)
	secret, err := s.sealRPCSecret(config.RPCAuthentication.Secret)
	if err != nil {
		return fmt.Errorf("protect RPC credential: %w", err)
	}
	apiKey, err := s.sealRPCSecret(config.RPCAuthentication.TokenAPIKey)
	if err != nil {
		return fmt.Errorf("protect RPC token API key: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO rpc_listeners (
    id, name, paused, chain_id, rpc_url, rpc_auth_type, rpc_auth_username, rpc_auth_header_name, rpc_auth_secret, rpc_auth_token_url, rpc_auth_token_api_key, start_block, batch_size, poll_interval,
    confirmations, reorg_depth, rpc_retry_attempts, rpc_retry_backoff,
    rpc_timeout, tls_ca_pem, tls_server_name, tls_insecure_skip_verify,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		config.ID, config.Name, config.Paused, config.ChainID, configuredLocator(config.RPCURL, config.RPCURLRef), config.RPCAuthentication.Type, config.RPCAuthentication.Username, config.RPCAuthentication.HeaderName, secret, config.RPCAuthentication.TokenURL, apiKey, config.StartBlock, config.BatchSize,
		config.PollInterval.Nanoseconds(), config.Confirmations, config.ReorgDepth, config.RPCRetryAttempts,
		config.RPCRetryBackoff.Nanoseconds(), config.RPCTimeout.Nanoseconds(), config.TLS.CAPEM,
		config.TLS.ServerName, config.TLS.InsecureSkipVerify, createdAt, timestamp)
	if err != nil {
		return fmt.Errorf("create RPC listener: %w", err)
	}
	if err := insertWebhooks(ctx, tx, "chain_webhook_configs", "rpc_listener_id", config.ID, config.Webhooks, timestamp, created); err != nil {
		return err
	}
	for position, contract := range config.Contracts {
		contractCreatedAt := creationTime(created, contract.ID, timestamp)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO contract_configs (id, rpc_listener_id, address, abi_json, position, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, contract.ID, config.ID, contract.Address, []byte(contract.ABI), position, contractCreatedAt, timestamp); err != nil {
			return fmt.Errorf("create contract configuration %s: %w", contract.ID, err)
		}
		if err := insertWebhooks(ctx, tx, "contract_webhook_configs", "contract_config_id", contract.ID, contract.Webhooks, timestamp, created); err != nil {
			return err
		}
		for eventPosition, event := range contract.Events {
			eventCreatedAt := creationTime(created, event.ID, timestamp)
			if _, err := tx.ExecContext(ctx, `
INSERT INTO event_configs (id, contract_config_id, selector, position, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)`, event.ID, contract.ID, event.Selector, eventPosition, eventCreatedAt, timestamp); err != nil {
				return fmt.Errorf("create event configuration %s: %w", event.ID, err)
			}
			if err := insertWebhooks(ctx, tx, "event_webhook_configs", "event_config_id", event.ID, event.Webhooks, timestamp, created); err != nil {
				return err
			}
		}
	}
	return nil
}

func insertWebhooks(ctx context.Context, tx *sql.Tx, table, parentColumn, parentID string, webhooks []core.WebhookConfig, timestamp int64, created map[string]int64) error {
	for position, webhook := range webhooks {
		createdAt := creationTime(created, webhook.ID, timestamp)
		var err error
		if parentColumn == "" {
			_, err = tx.ExecContext(ctx, "INSERT INTO "+table+" (id, url, auth_type, auth_secret_ref, auth_key_id, position, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", webhook.ID, configuredLocator(webhook.URL, webhook.URLRef), webhook.Authentication.Type, webhook.Authentication.SecretRef, webhook.Authentication.KeyID, position, createdAt, timestamp)
		} else {
			_, err = tx.ExecContext(ctx, "INSERT INTO "+table+" (id, "+parentColumn+", url, auth_type, auth_secret_ref, auth_key_id, position, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", webhook.ID, parentID, configuredLocator(webhook.URL, webhook.URLRef), webhook.Authentication.Type, webhook.Authentication.SecretRef, webhook.Authentication.KeyID, position, createdAt, timestamp)
		}
		if err != nil {
			return fmt.Errorf("create webhook configuration %s: %w", webhook.ID, err)
		}
	}
	return nil
}

func creationTime(created map[string]int64, id string, fallback int64) int64 {
	if value := created[id]; value != 0 {
		return value
	}
	return fallback
}

func (s *Store) loadListeners(ctx context.Context, executor rowsQuerier, id string, filter bool) ([]core.RPCListener, error) {
	statement := `
SELECT id, name, paused, chain_id, rpc_url, rpc_auth_type, rpc_auth_username, rpc_auth_header_name, rpc_auth_secret, rpc_auth_token_url, rpc_auth_token_api_key, start_block, batch_size, poll_interval,
       confirmations, reorg_depth, rpc_retry_attempts, rpc_retry_backoff,
       rpc_timeout, tls_ca_pem, tls_server_name, tls_insecure_skip_verify,
       created_at, updated_at
FROM rpc_listeners`
	args := []any{}
	if filter {
		statement += ` WHERE id = ?`
		args = append(args, id)
	}
	statement += ` ORDER BY name, chain_id, id`
	rows, err := executor.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query RPC listeners: %w", err)
	}
	var listeners []core.RPCListener
	for rows.Next() {
		var listener core.RPCListener
		var pollInterval, retryBackoff, rpcTimeout, createdAt, updatedAt int64
		var protectedSecret, protectedAPIKey []byte
		if err := rows.Scan(&listener.ID, &listener.Name, &listener.Paused, &listener.ChainID, &listener.RPCURL, &listener.RPCAuthentication.Type, &listener.RPCAuthentication.Username, &listener.RPCAuthentication.HeaderName, &protectedSecret, &listener.RPCAuthentication.TokenURL, &protectedAPIKey, &listener.StartBlock,
			&listener.BatchSize, &pollInterval, &listener.Confirmations, &listener.ReorgDepth,
			&listener.RPCRetryAttempts, &retryBackoff, &rpcTimeout, &listener.TLS.CAPEM,
			&listener.TLS.ServerName, &listener.TLS.InsecureSkipVerify, &createdAt, &updatedAt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan RPC listener: %w", err)
		}
		secret, err := s.openRPCSecret(protectedSecret)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		listener.RPCAuthentication.Secret = secret
		apiKey, err := s.openRPCSecret(protectedAPIKey)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		listener.RPCAuthentication.TokenAPIKey = apiKey
		listener.PollInterval = time.Duration(pollInterval)
		listener.RPCRetryBackoff = time.Duration(retryBackoff)
		listener.RPCTimeout = time.Duration(rpcTimeout)
		listener.CreatedAt = time.Unix(0, createdAt).UTC()
		listener.UpdatedAt = time.Unix(0, updatedAt).UTC()
		if secrets.IsReference(listener.RPCURL) {
			listener.RPCURLRef, listener.RPCURL = listener.RPCURL, ""
		}
		listeners = append(listeners, listener)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range listeners {
		if err := s.loadListenerChildren(ctx, executor, &listeners[i]); err != nil {
			return nil, err
		}
	}
	return listeners, nil
}

func (s *Store) loadListenerChildren(ctx context.Context, executor rowsQuerier, listener *core.RPCListener) error {
	var err error
	listener.Webhooks, err = loadWebhooks(ctx, executor, `SELECT id, url, auth_type, auth_secret_ref, auth_key_id, created_at, updated_at FROM chain_webhook_configs WHERE rpc_listener_id = ? ORDER BY position`, listener.ID)
	if err != nil {
		return err
	}
	rows, err := executor.QueryContext(ctx, `SELECT id, address, abi_json, created_at, updated_at FROM contract_configs WHERE rpc_listener_id = ? ORDER BY position`, listener.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var contract core.ContractConfig
		var abi []byte
		var createdAt, updatedAt int64
		if err := rows.Scan(&contract.ID, &contract.Address, &abi, &createdAt, &updatedAt); err != nil {
			_ = rows.Close()
			return err
		}
		contract.ABI = append(json.RawMessage(nil), abi...)
		contract.CreatedAt = time.Unix(0, createdAt).UTC()
		contract.UpdatedAt = time.Unix(0, updatedAt).UTC()
		listener.Contracts = append(listener.Contracts, contract)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for i := range listener.Contracts {
		contract := &listener.Contracts[i]
		contract.Webhooks, err = loadWebhooks(ctx, executor, `SELECT id, url, auth_type, auth_secret_ref, auth_key_id, created_at, updated_at FROM contract_webhook_configs WHERE contract_config_id = ? ORDER BY position`, contract.ID)
		if err != nil {
			return err
		}
		eventRows, err := executor.QueryContext(ctx, `SELECT id, selector, created_at, updated_at FROM event_configs WHERE contract_config_id = ? ORDER BY position`, contract.ID)
		if err != nil {
			return err
		}
		for eventRows.Next() {
			var event core.EventConfig
			var createdAt, updatedAt int64
			if err := eventRows.Scan(&event.ID, &event.Selector, &createdAt, &updatedAt); err != nil {
				_ = eventRows.Close()
				return err
			}
			event.CreatedAt = time.Unix(0, createdAt).UTC()
			event.UpdatedAt = time.Unix(0, updatedAt).UTC()
			contract.Events = append(contract.Events, event)
		}
		if err := eventRows.Close(); err != nil {
			return err
		}
		for j := range contract.Events {
			contract.Events[j].Webhooks, err = loadWebhooks(ctx, executor, `SELECT id, url, auth_type, auth_secret_ref, auth_key_id, created_at, updated_at FROM event_webhook_configs WHERE event_config_id = ? ORDER BY position`, contract.Events[j].ID)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

type rowsQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadWebhooks(ctx context.Context, query rowsQuerier, statement string, args ...any) ([]core.WebhookConfig, error) {
	rows, err := query.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var webhooks []core.WebhookConfig
	for rows.Next() {
		var webhook core.WebhookConfig
		var createdAt, updatedAt int64
		if err := rows.Scan(&webhook.ID, &webhook.URL, &webhook.Authentication.Type, &webhook.Authentication.SecretRef, &webhook.Authentication.KeyID, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		webhook.CreatedAt = time.Unix(0, createdAt).UTC()
		webhook.UpdatedAt = time.Unix(0, updatedAt).UTC()
		if secrets.IsReference(webhook.URL) {
			webhook.URLRef, webhook.URL = webhook.URL, ""
		}
		webhooks = append(webhooks, webhook)
	}
	return webhooks, rows.Err()
}

func validateStoredListener(config core.RPCListener) error {
	if err := validConfigID(config.ID); err != nil {
		return fmt.Errorf("RPC listener id: %w", err)
	}
	if strings.TrimSpace(config.Name) == "" || (config.RPCURL == "") == (config.RPCURLRef == "") {
		return errors.New("RPC listener name and exactly one RPC URL or reference are required")
	}
	if config.RPCURLRef != "" {
		if err := secrets.ValidateReference(config.RPCURLRef); err != nil {
			return fmt.Errorf("RPC URL reference: %w", err)
		}
	}
	if err := (rpcauth.Config{Type: config.RPCAuthentication.Type, Username: config.RPCAuthentication.Username, HeaderName: config.RPCAuthentication.HeaderName, Secret: config.RPCAuthentication.Secret, TokenURL: config.RPCAuthentication.TokenURL, TokenAPIKey: config.RPCAuthentication.TokenAPIKey}).Validate(); err != nil {
		return fmt.Errorf("RPC authentication: %w", err)
	}
	if config.ChainID == 0 || config.ChainID > math.MaxInt64 || config.StartBlock > math.MaxInt64 || config.BatchSize == 0 || config.BatchSize > math.MaxInt64 ||
		config.PollInterval <= 0 || config.ReorgDepth == 0 || config.ReorgDepth > math.MaxInt64 || config.Confirmations > math.MaxInt64 ||
		config.RPCRetryAttempts <= 0 || config.RPCRetryBackoff <= 0 || config.RPCTimeout <= 0 {
		return errors.New("RPC listener numeric values are outside supported ranges")
	}
	if err := validateWebhooks(config.Webhooks); err != nil {
		return fmt.Errorf("chain webhooks: %w", err)
	}
	seenContracts := make(map[string]struct{}, len(config.Contracts))
	for _, contract := range config.Contracts {
		if err := validConfigID(contract.ID); err != nil {
			return fmt.Errorf("contract config id: %w", err)
		}
		if strings.TrimSpace(contract.Address) == "" || !json.Valid(contract.ABI) {
			return fmt.Errorf("contract %s requires an address and valid ABI JSON", contract.ID)
		}
		address := strings.ToLower(contract.Address)
		if _, exists := seenContracts[address]; exists {
			return fmt.Errorf("duplicate contract address %s", contract.Address)
		}
		seenContracts[address] = struct{}{}
		if err := validateWebhooks(contract.Webhooks); err != nil {
			return fmt.Errorf("contract %s webhooks: %w", contract.ID, err)
		}
		seenEvents := make(map[string]struct{}, len(contract.Events))
		for _, event := range contract.Events {
			if err := validConfigID(event.ID); err != nil {
				return fmt.Errorf("event config id: %w", err)
			}
			if strings.TrimSpace(event.Selector) == "" {
				return fmt.Errorf("event %s selector is required", event.ID)
			}
			if _, exists := seenEvents[event.Selector]; exists {
				return fmt.Errorf("duplicate event selector %s", event.Selector)
			}
			seenEvents[event.Selector] = struct{}{}
			if err := validateWebhooks(event.Webhooks); err != nil {
				return fmt.Errorf("event %s webhooks: %w", event.ID, err)
			}
		}
	}
	return nil
}

func validateWebhooks(webhooks []core.WebhookConfig) error {
	seen := make(map[string]struct{}, len(webhooks))
	for _, webhook := range webhooks {
		if err := validConfigID(webhook.ID); err != nil {
			return fmt.Errorf("webhook config id: %w", err)
		}
		if (webhook.URL == "") == (webhook.URLRef == "") {
			return fmt.Errorf("webhook %s requires exactly one URL or reference", webhook.ID)
		}
		if webhook.URLRef != "" {
			if err := secrets.ValidateReference(webhook.URLRef); err != nil {
				return fmt.Errorf("webhook %s URL reference: %w", webhook.ID, err)
			}
		}
		if err := validateStoredWebhookAuthentication(webhook.Authentication); err != nil {
			return fmt.Errorf("webhook %s authentication: %w", webhook.ID, err)
		}
		locator := configuredLocator(webhook.URL, webhook.URLRef)
		if _, exists := seen[locator]; exists {
			return fmt.Errorf("duplicate webhook destination")
		}
		seen[locator] = struct{}{}
	}
	return nil
}

var storedWebhookKeyID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func validateStoredWebhookAuthentication(authentication core.WebhookAuthentication) error {
	if authentication.Type == "" {
		if authentication.SecretRef != "" || authentication.KeyID != "" {
			return errors.New("authentication type is required")
		}
		return nil
	}
	if authentication.Type != "hmac-sha256" {
		return errors.New("unsupported authentication type")
	}
	if err := secrets.ValidateReference(authentication.SecretRef); err != nil {
		return fmt.Errorf("secret reference: %w", err)
	}
	if authentication.KeyID != "" && !storedWebhookKeyID.MatchString(authentication.KeyID) {
		return errors.New("invalid key ID")
	}
	return nil
}

func configuredLocator(value, reference string) string {
	if reference != "" {
		return reference
	}
	return value
}

func validConfigID(id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != strings.ToLower(id) {
		return fmt.Errorf("%q must be a canonical UUID", id)
	}
	return nil
}
