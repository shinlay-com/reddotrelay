package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"reddotrelay/internal/config"
	retentionservice "reddotrelay/internal/retention"
	"reddotrelay/internal/store/sqlite"
)

type dynamicRetentionSettings struct {
	mu    sync.RWMutex
	value config.RetentionConfig
}

func (s *dynamicRetentionSettings) get() config.RetentionConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}
func (s *dynamicRetentionSettings) set(value config.RetentionConfig) {
	s.mu.Lock()
	s.value = value
	s.mu.Unlock()
}

func runDynamicRetentionWorker(ctx context.Context, settings *dynamicRetentionSettings, logger *slog.Logger, service *retentionservice.Service) error {
	for {
		current := settings.get()
		if current.DeliveredFor > 0 && current.PollInterval > 0 && current.BatchSize > 0 {
			cutoff := time.Now().UTC().Add(-current.DeliveredFor)
			result, err := service.Prune(ctx, cutoff, current.BatchSize, 0, 25*time.Millisecond, nil)
			if err != nil && !errors.Is(err, retentionservice.ErrRunning) {
				logger.Error("retention prune failed", "error", err)
			} else if err == nil && result.Deleted > 0 {
				logger.Info("retention pruned delivered events", "events", result.Deleted, "cutoff", cutoff)
			}
		}
		delay := current.PollInterval
		if delay <= 0 || delay > time.Minute {
			delay = time.Minute
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func runRetentionWorker(ctx context.Context, store *sqlite.Store, settings config.RetentionConfig, logger *slog.Logger, services ...*retentionservice.Service) error {
	service := retentionservice.New(store)
	if len(services) > 0 && services[0] != nil {
		service = services[0]
	}
	prune := func() error {
		cutoff := time.Now().UTC().Add(-settings.DeliveredFor)
		result, err := service.Prune(ctx, cutoff, settings.BatchSize, 0, 25*time.Millisecond, nil)
		if err != nil {
			return err
		}
		if result.Deleted > 0 {
			logger.Info("retention pruned delivered events", "events", result.Deleted, "cutoff", cutoff)
		}
		return nil
	}
	if err := prune(); err != nil {
		return fmt.Errorf("initial retention prune: %w", err)
	}
	ticker := time.NewTicker(settings.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := prune(); err != nil {
				logger.Error("retention prune failed", "error", err)
			}
		}
	}
}
