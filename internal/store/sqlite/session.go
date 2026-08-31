package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"reddotrelay/internal/core"
	"time"
)

func (s *Store) SaveUISession(ctx context.Context, r core.UISessionRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO ui_sessions(token_hash,principal_id,principal_kind,csrf_token,created_at,last_seen) VALUES(?,?,'user',?,?,?) ON CONFLICT(token_hash) DO UPDATE SET last_seen=excluded.last_seen`, r.TokenHash, r.PrincipalID, r.CSRFToken, r.CreatedAt.UTC().UnixNano(), r.LastSeen.UTC().UnixNano())
	return err
}
func (s *Store) UISession(ctx context.Context, hash []byte) (core.UISessionRecord, error) {
	var r core.UISessionRecord
	var created, last int64
	err := s.db.QueryRowContext(ctx, `SELECT token_hash,principal_id,csrf_token,created_at,last_seen FROM ui_sessions WHERE token_hash=? AND principal_kind='user'`, hash).Scan(&r.TokenHash, &r.PrincipalID, &r.CSRFToken, &created, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	r.CreatedAt = time.Unix(0, created).UTC()
	r.LastSeen = time.Unix(0, last).UTC()
	return r, err
}
func (s *Store) DeleteUISession(ctx context.Context, hash []byte) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ui_sessions WHERE token_hash=?`, hash)
	return err
}
func (s *Store) DeleteExpiredUISessions(ctx context.Context, idle, absolute time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ui_sessions WHERE last_seen<? OR created_at<?`, idle.UTC().UnixNano(), absolute.UTC().UnixNano())
	return err
}
