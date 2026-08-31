package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shirou/gopsutil/disk"
)

type StorageStatus struct {
	Path             string     `json:"-"`
	DatabaseBytes    int64      `json:"databaseBytes"`
	WALBytes         int64      `json:"walBytes"`
	SHMBytes         int64      `json:"shmBytes"`
	TotalBytes       int64      `json:"totalBytes"`
	PageSize         int64      `json:"pageSize"`
	PageCount        int64      `json:"pageCount"`
	FreePages        int64      `json:"freePages"`
	ReclaimableBytes int64      `json:"reclaimableBytes"`
	VolumeTotalBytes uint64     `json:"volumeTotalBytes"`
	VolumeFreeBytes  uint64     `json:"volumeFreeBytes"`
	VolumeUsedBytes  uint64     `json:"volumeUsedBytes"`
	VolumeUsedPct    float64    `json:"volumeUsedPercent"`
	EventCount       int64      `json:"eventCount"`
	PendingCount     int64      `json:"pendingDeliveries"`
	DeliveredCount   int64      `json:"deliveredDeliveries"`
	DeadCount        int64      `json:"deadDeliveries"`
	OldestEventAt    *time.Time `json:"oldestEventAt,omitempty"`
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (s *Store) StorageStatus(ctx context.Context) (StorageStatus, error) {
	path, err := filepath.Abs(s.path)
	if err != nil {
		return StorageStatus{}, fmt.Errorf("resolve SQLite path: %w", err)
	}
	result := StorageStatus{Path: path, DatabaseBytes: fileSize(path), WALBytes: fileSize(path + "-wal"), SHMBytes: fileSize(path + "-shm")}
	result.TotalBytes = result.DatabaseBytes + result.WALBytes + result.SHMBytes
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&result.PageSize); err != nil {
		return StorageStatus{}, fmt.Errorf("read SQLite page size: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&result.PageCount); err != nil {
		return StorageStatus{}, fmt.Errorf("read SQLite page count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&result.FreePages); err != nil {
		return StorageStatus{}, fmt.Errorf("read SQLite free pages: %w", err)
	}
	result.ReclaimableBytes = result.PageSize * result.FreePages
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(observed_at) FROM events`).Scan(&result.EventCount, newNullableTime(&result.OldestEventAt)); err != nil {
		return StorageStatus{}, fmt.Errorf("read event storage summary: %w", err)
	}
	result.PendingCount, result.DeliveredCount, result.DeadCount, err = s.DeliveryStatusCounts(ctx)
	if err != nil {
		return StorageStatus{}, err
	}
	if usage, usageErr := disk.Usage(filepath.Dir(path)); usageErr == nil {
		result.VolumeTotalBytes = usage.Total
		result.VolumeFreeBytes = usage.Free
		result.VolumeUsedBytes = usage.Used
		result.VolumeUsedPct = usage.UsedPercent
	}
	return result, nil
}

type nullableTimeTarget struct{ destination **time.Time }

func newNullableTime(destination **time.Time) *nullableTimeTarget {
	return &nullableTimeTarget{destination: destination}
}
func (target *nullableTimeTarget) Scan(value any) error {
	if value == nil {
		*target.destination = nil
		return nil
	}
	var nanos int64
	switch typed := value.(type) {
	case int64:
		nanos = typed
	case int:
		nanos = int64(typed)
	default:
		return fmt.Errorf("unexpected SQLite timestamp type %T", value)
	}
	parsed := time.Unix(0, nanos).UTC()
	*target.destination = &parsed
	return nil
}

func (s *Store) Optimize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint SQLite WAL: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		return fmt.Errorf("optimize SQLite: %w", err)
	}
	return nil
}
