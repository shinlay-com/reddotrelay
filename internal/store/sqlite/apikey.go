package sqlite

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"reddotrelay/internal/auth"
	"reddotrelay/internal/core"
)

var ErrInvalidAPIKey = errors.New("invalid API key")

func (s *Store) CreateAPIKey(ctx context.Context, key core.APIKey, secretHash [32]byte) error {
	if err := validateAPIKey(key); err != nil {
		return err
	}
	if secretHash == ([32]byte{}) {
		return errors.New("API key secret hash is required")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO api_keys (id, name, role, secret_hash, secret_prefix, created_at)
VALUES (?, ?, ?, ?, ?, ?)`, key.ID, key.Name, key.Role, secretHash[:], key.Prefix, key.CreatedAt.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("create API key: %w", err)
	}
	return nil
}

func (s *Store) APIKeys(ctx context.Context) ([]core.APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, role, secret_prefix, created_at, last_used_at, revoked_at
FROM api_keys ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list API keys: %w", err)
	}
	defer rows.Close()
	var keys []core.APIKey
	for rows.Next() {
		var key core.APIKey
		var createdAt int64
		var lastUsedAt, revokedAt sql.NullInt64
		if err := rows.Scan(&key.ID, &key.Name, &key.Role, &key.Prefix, &createdAt, &lastUsedAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("scan API key: %w", err)
		}
		key.CreatedAt = time.Unix(0, createdAt).UTC()
		key.LastUsedAt = nullableTime(lastUsedAt)
		key.RevokedAt = nullableTime(revokedAt)
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate API keys: %w", err)
	}
	return keys, nil
}

func (s *Store) RevokeAPIKey(ctx context.Context, id string, at time.Time) error {
	if err := validConfigID(id); err != nil {
		return fmt.Errorf("API key id: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, at.UTC().UnixNano(), id)
	if err != nil {
		return fmt.Errorf("revoke API key: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read API key revoke result: %w", err)
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetAPIKeyRole(ctx context.Context, id string, role core.APIKeyRole) error {
	if role != core.APIKeyAdmin && role != core.APIKeyReadOnly {
		return errors.New("unsupported role")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE api_keys SET role=? WHERE id=? AND revoked_at IS NULL`, role, id)
	if err != nil {
		return fmt.Errorf("set API key role: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read API key role result: %w", err)
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

// AuthenticateAPIKey compares the presented digest against every active key
// using constant-time comparison. Revoked keys never authenticate.
func (s *Store) AuthenticateAPIKey(ctx context.Context, secret string, at time.Time) (core.APIKeyPrincipal, error) {
	presented, err := auth.HashAPIKeySecret(secret)
	if err != nil {
		return core.APIKeyPrincipal{}, ErrInvalidAPIKey
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, role, secret_hash FROM api_keys WHERE revoked_at IS NULL`)
	if err != nil {
		return core.APIKeyPrincipal{}, fmt.Errorf("load active API keys: %w", err)
	}
	type candidate struct {
		principal core.APIKeyPrincipal
		hash      []byte
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.principal.ID, &item.principal.Name, &item.principal.Role, &item.hash); err != nil {
			_ = rows.Close()
			return core.APIKeyPrincipal{}, fmt.Errorf("scan active API key: %w", err)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return core.APIKeyPrincipal{}, fmt.Errorf("close active API keys: %w", err)
	}
	if err := rows.Err(); err != nil {
		return core.APIKeyPrincipal{}, fmt.Errorf("iterate active API keys: %w", err)
	}
	match := -1
	for i := range candidates {
		if len(candidates[i].hash) == len(presented) && subtle.ConstantTimeCompare(candidates[i].hash, presented[:]) == 1 {
			match = i
		}
	}
	if match < 0 {
		return core.APIKeyPrincipal{}, ErrInvalidAPIKey
	}
	principal := candidates[match].principal
	result, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ? AND revoked_at IS NULL`, at.UTC().UnixNano(), principal.ID)
	if err != nil {
		return core.APIKeyPrincipal{}, fmt.Errorf("record API key use: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return core.APIKeyPrincipal{}, fmt.Errorf("read API key use result: %w", err)
	}
	if changed != 1 {
		return core.APIKeyPrincipal{}, ErrInvalidAPIKey
	}
	return principal, nil
}

func (s *Store) ActiveAPIKeyPrincipal(ctx context.Context, id string) (core.APIKeyPrincipal, error) {
	if err := validConfigID(id); err != nil {
		return core.APIKeyPrincipal{}, ErrInvalidAPIKey
	}
	var principal core.APIKeyPrincipal
	err := s.db.QueryRowContext(ctx, `
SELECT id, name, role FROM api_keys WHERE id = ? AND revoked_at IS NULL`, id).
		Scan(&principal.ID, &principal.Name, &principal.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return core.APIKeyPrincipal{}, ErrInvalidAPIKey
	}
	if err != nil {
		return core.APIKeyPrincipal{}, fmt.Errorf("load active API key principal: %w", err)
	}
	return principal, nil
}

func validateAPIKey(key core.APIKey) error {
	if err := validConfigID(key.ID); err != nil {
		return fmt.Errorf("API key id: %w", err)
	}
	if strings.TrimSpace(key.Name) == "" {
		return errors.New("API key name is required")
	}
	if key.Role != core.APIKeyAdmin && key.Role != core.APIKeyReadOnly {
		return fmt.Errorf("unsupported API key role %q", key.Role)
	}
	if key.Prefix == "" {
		return errors.New("API key prefix is required")
	}
	if key.CreatedAt.IsZero() {
		return errors.New("API key creation time is required")
	}
	return nil
}

func nullableTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	result := time.Unix(0, value.Int64).UTC()
	return &result
}
