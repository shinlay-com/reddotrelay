package sqlite

import (
	"context"
	"database/sql"
	"reddotrelay/internal/config"
	"time"
)

func (s *Store) RetentionSettings(ctx context.Context) (config.RetentionConfig, bool, error) {
	var enabled int
	var delivered, poll int64
	var batch int
	err := s.db.QueryRowContext(ctx, `SELECT enabled, delivered_for, poll_interval, batch_size FROM retention_settings WHERE singleton=1`).Scan(&enabled, &delivered, &poll, &batch)
	if err == sql.ErrNoRows {
		return config.RetentionConfig{}, false, nil
	}
	if err != nil {
		return config.RetentionConfig{}, false, err
	}
	if enabled == 0 {
		return config.RetentionConfig{}, true, nil
	}
	return config.RetentionConfig{DeliveredFor: time.Duration(delivered), PollInterval: time.Duration(poll), BatchSize: batch}, true, nil
}
func (s *Store) SaveRetentionSettings(ctx context.Context, settings config.RetentionConfig) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO retention_settings(singleton,enabled,delivered_for,poll_interval,batch_size,updated_at) VALUES(1,?,?,?,?,?) ON CONFLICT(singleton) DO UPDATE SET enabled=excluded.enabled,delivered_for=excluded.delivered_for,poll_interval=excluded.poll_interval,batch_size=excluded.batch_size,updated_at=excluded.updated_at`, boolInt(settings.DeliveredFor > 0), int64(settings.DeliveredFor), int64(settings.PollInterval), settings.BatchSize, time.Now().UTC().Unix())
	return err
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
