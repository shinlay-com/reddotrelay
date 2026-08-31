package retention

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrRunning = errors.New("retention cleanup is already running")

type Store interface {
	CountDeliveredBefore(context.Context, time.Time) (int64, error)
	PruneDeliveredBefore(context.Context, time.Time, int) (int64, error)
}

type Result struct {
	Cutoff    time.Time     `json:"cutoff"`
	Eligible  int64         `json:"eligible"`
	Deleted   int64         `json:"deleted"`
	Duration  time.Duration `json:"duration"`
	Completed time.Time     `json:"completedAt"`
	Error     string        `json:"error,omitempty"`
}

type Status struct {
	Running bool    `json:"running"`
	LastRun *Result `json:"lastRun,omitempty"`
}

type Service struct {
	store   Store
	mu      sync.Mutex
	running bool
	last    *Result
}

func New(store Store) *Service { return &Service{store: store} }

func (service *Service) Preview(ctx context.Context, cutoff time.Time) (Result, error) {
	started := time.Now()
	count, err := service.store.CountDeliveredBefore(ctx, cutoff)
	result := Result{Cutoff: cutoff.UTC(), Eligible: count, Duration: time.Since(started), Completed: time.Now().UTC()}
	if err != nil {
		result.Error = err.Error()
	}
	return result, err
}

func (service *Service) Prune(ctx context.Context, cutoff time.Time, batchSize int, maximum int64, pause time.Duration, progress func(int64)) (Result, error) {
	if batchSize <= 0 {
		return Result{}, errors.New("retention batch size must be greater than zero")
	}
	service.mu.Lock()
	if service.running {
		service.mu.Unlock()
		return Result{}, ErrRunning
	}
	service.running = true
	service.mu.Unlock()
	started := time.Now()
	result := Result{Cutoff: cutoff.UTC()}
	defer func() {
		service.mu.Lock()
		service.running = false
		copied := result
		service.last = &copied
		service.mu.Unlock()
	}()
	eligible, err := service.store.CountDeliveredBefore(ctx, cutoff)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	result.Eligible = eligible
	for maximum == 0 || result.Deleted < maximum {
		limit := batchSize
		if maximum > 0 && int64(limit) > maximum-result.Deleted {
			limit = int(maximum - result.Deleted)
		}
		count, pruneErr := service.store.PruneDeliveredBefore(ctx, cutoff, limit)
		if pruneErr != nil {
			result.Error = pruneErr.Error()
			err = pruneErr
			break
		}
		result.Deleted += count
		if progress != nil {
			progress(result.Deleted)
		}
		if count < int64(limit) {
			break
		}
		if pause > 0 {
			select {
			case <-ctx.Done():
				err = ctx.Err()
				result.Error = err.Error()
			case <-time.After(pause):
			}
			if err != nil {
				break
			}
		}
	}
	result.Duration = time.Since(started)
	result.Completed = time.Now().UTC()
	return result, err
}

func (service *Service) Status() Status {
	service.mu.Lock()
	defer service.mu.Unlock()
	var last *Result
	if service.last != nil {
		copied := *service.last
		last = &copied
	}
	return Status{Running: service.running, LastRun: last}
}
