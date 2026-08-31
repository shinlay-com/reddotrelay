package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"reddotrelay/internal/core"
)

var ErrInvalidUserCredentials = errors.New("invalid user credentials")
var ErrUserSetupComplete = errors.New("initial user setup is complete")

func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	return count > 0, nil
}

func (s *Store) CreateInitialAdmin(ctx context.Context, username, password string, at time.Time) (core.APIKeyPrincipal, error) {
	username = strings.TrimSpace(username)
	if len(username) < 3 || len(username) > 64 {
		return core.APIKeyPrincipal{}, errors.New("username must be between 3 and 64 characters")
	}
	if len(password) < 12 {
		return core.APIKeyPrincipal{}, errors.New("password must be at least 12 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return core.APIKeyPrincipal{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.APIKeyPrincipal{}, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return core.APIKeyPrincipal{}, err
	}
	if count != 0 {
		return core.APIKeyPrincipal{}, ErrUserSetupComplete
	}
	id := core.NewConfigID()
	now := at.UTC().UnixNano()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,enabled,created_at,updated_at) VALUES(?,?,?,'admin',1,?,?)`, id, username, hash, now, now); err != nil {
		return core.APIKeyPrincipal{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.APIKeyPrincipal{}, err
	}
	return core.APIKeyPrincipal{ID: id, Name: username, Role: core.APIKeyAdmin}, nil
}

func (s *Store) AuthenticateUser(ctx context.Context, username, password string, at time.Time) (core.APIKeyPrincipal, error) {
	var principal core.APIKeyPrincipal
	var hash []byte
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT id,username,role,password_hash,enabled FROM users WHERE username=?`, strings.TrimSpace(username)).Scan(&principal.ID, &principal.Name, &principal.Role, &hash, &enabled)
	if errors.Is(err, sql.ErrNoRows) || enabled != 1 {
		return core.APIKeyPrincipal{}, ErrInvalidUserCredentials
	}
	if err != nil {
		return core.APIKeyPrincipal{}, err
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		return core.APIKeyPrincipal{}, ErrInvalidUserCredentials
	}
	_, err = s.db.ExecContext(ctx, `UPDATE users SET last_login_at=? WHERE id=?`, at.UTC().UnixNano(), principal.ID)
	return principal, err
}

func (s *Store) ActiveUserPrincipal(ctx context.Context, id string) (core.APIKeyPrincipal, error) {
	var p core.APIKeyPrincipal
	err := s.db.QueryRowContext(ctx, `SELECT id,username,role FROM users WHERE id=? AND enabled=1`, id).Scan(&p.ID, &p.Name, &p.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrInvalidUserCredentials
	}
	return p, err
}

func (s *Store) Users(ctx context.Context) ([]core.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,username,role,enabled,created_at,updated_at,last_login_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.User
	for rows.Next() {
		var u core.User
		var enabled int
		var created, updated int64
		var last sql.NullInt64
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &enabled, &created, &updated, &last); err != nil {
			return nil, err
		}
		u.Enabled = enabled == 1
		u.CreatedAt = time.Unix(0, created).UTC()
		u.UpdatedAt = time.Unix(0, updated).UTC()
		u.LastLoginAt = nullableTime(last)
		result = append(result, u)
	}
	return result, rows.Err()
}

func (s *Store) CreateUser(ctx context.Context, username, password string, role core.APIKeyRole, at time.Time) (core.User, error) {
	username = strings.TrimSpace(username)
	if len(username) < 3 || len(username) > 64 {
		return core.User{}, errors.New("username must be between 3 and 64 characters")
	}
	if len(password) < 12 {
		return core.User{}, errors.New("password must be at least 12 characters")
	}
	if role != core.APIKeyAdmin && role != core.APIKeyReadOnly {
		return core.User{}, errors.New("unsupported role")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return core.User{}, err
	}
	u := core.User{ID: core.NewConfigID(), Username: username, Role: role, Enabled: true, CreatedAt: at.UTC(), UpdatedAt: at.UTC()}
	_, err = s.db.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,enabled,created_at,updated_at) VALUES(?,?,?,?,1,?,?)`, u.ID, u.Username, hash, u.Role, u.CreatedAt.UnixNano(), u.UpdatedAt.UnixNano())
	return u, err
}
func (s *Store) SetUserEnabled(ctx context.Context, id string, enabled bool, at time.Time) error {
	if !enabled {
		var role core.APIKeyRole
		if err := s.db.QueryRowContext(ctx, `SELECT role FROM users WHERE id=?`, id).Scan(&role); err != nil {
			return ErrNotFound
		}
		if role == core.APIKeyAdmin {
			var count int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND enabled=1`).Scan(&count); err != nil {
				return err
			}
			if count <= 1 {
				return errors.New("the last enabled administrator cannot be disabled")
			}
		}
	}
	result, err := s.db.ExecContext(ctx, `UPDATE users SET enabled=?,updated_at=? WHERE id=?`, boolInt(enabled), at.UTC().UnixNano(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrNotFound
	}
	return nil
}
func (s *Store) ResetUserPassword(ctx context.Context, id, password string, at time.Time) error {
	if len(password) < 12 {
		return errors.New("password must be at least 12 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash=?,updated_at=? WHERE id=?`, hash, at.UTC().UnixNano(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetUserRole(ctx context.Context, id string, role core.APIKeyRole, at time.Time) error {
	if role != core.APIKeyAdmin && role != core.APIKeyReadOnly {
		return errors.New("unsupported role")
	}
	var current core.APIKeyRole
	var enabled int
	if err := s.db.QueryRowContext(ctx, `SELECT role,enabled FROM users WHERE id=?`, id).Scan(&current, &enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if current == role {
		return nil
	}
	if current == core.APIKeyAdmin && enabled == 1 {
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND enabled=1`).Scan(&count); err != nil {
			return err
		}
		if count <= 1 {
			return errors.New("the last enabled administrator cannot be demoted")
		}
	}
	result, err := s.db.ExecContext(ctx, `UPDATE users SET role=?,updated_at=? WHERE id=?`, role, at.UTC().UnixNano(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrNotFound
	}
	return nil
}
