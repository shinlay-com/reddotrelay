package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"reddotrelay/internal/core"
)

const (
	defaultRuntimePollInterval = 2 * time.Second
	defaultRuntimeRetryBase    = time.Second
	defaultRuntimeRetryMax     = 30 * time.Second
)

type rpcListenerSource interface {
	RPCListenerSnapshot(context.Context) (core.RPCListenerSnapshot, error)
	RPCListenerChanges() <-chan struct{}
}

type scannerRunner interface {
	Run(context.Context) error
}

type scannerRuntime struct {
	runner scannerRunner
	close  func()
	idle   bool
}

type scannerRuntimeBuilder func(context.Context, core.RPCListenerSnapshot, core.RPCListener) (*scannerRuntime, error)

type activeScannerRuntime struct {
	chainID uint64
	hash    [sha256.Size]byte
	idle    bool
	cancel  context.CancelFunc
	result  chan error
	close   func()
}

type failedRuntimeBuild struct {
	hash       [sha256.Size]byte
	attempts   int
	retryAfter time.Time
	lastError  string
}

type runtimeListenerStatus struct {
	ID          string     `json:"id"`
	ChainID     uint64     `json:"chainId"`
	State       string     `json:"state"`
	Attempts    int        `json:"attempts"`
	NextRetryAt *time.Time `json:"nextRetryAt,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
}

type runtimeManagerStatus struct {
	DesiredRevision          uint64                  `json:"desiredRevision"`
	InitialReconcileComplete bool                    `json:"initialReconcileComplete"`
	LastReconciledAt         *time.Time              `json:"lastReconciledAt,omitempty"`
	Ready                    bool                    `json:"ready"`
	RPCListeners             []runtimeListenerStatus `json:"rpcListeners"`
}

// runtimeManager is the single owner of live scanner goroutines. Reconcile is
// serialized, so an RPC listener can never have two running scanners.
type runtimeManager struct {
	source       rpcListenerSource
	build        scannerRuntimeBuilder
	logger       *slog.Logger
	pollInterval time.Duration
	retryBase    time.Duration
	retryMax     time.Duration

	active   map[string]*activeScannerRuntime
	failed   map[string]failedRuntimeBuild
	status   atomic.Value
	observer runtimeObserver
}

type runtimeObserver interface {
	BeginRuntimeSnapshot(uint64)
	RuntimeListener(string, uint64, string)
	RuntimeBuildFailure(string, uint64)
}
type noopRuntimeObserver struct{}

func (noopRuntimeObserver) BeginRuntimeSnapshot(uint64)            {}
func (noopRuntimeObserver) RuntimeListener(string, uint64, string) {}
func (noopRuntimeObserver) RuntimeBuildFailure(string, uint64)     {}

func newRuntimeManager(source rpcListenerSource, build scannerRuntimeBuilder, logger *slog.Logger) (*runtimeManager, error) {
	return newRuntimeManagerWithObserver(source, build, logger, noopRuntimeObserver{})
}

func newRuntimeManagerWithObserver(source rpcListenerSource, build scannerRuntimeBuilder, logger *slog.Logger, observer runtimeObserver) (*runtimeManager, error) {
	if source == nil || build == nil {
		return nil, errors.New("RPC listener source and runtime builder are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if observer == nil {
		observer = noopRuntimeObserver{}
	}
	manager := &runtimeManager{
		source: source, build: build, logger: logger,
		pollInterval: defaultRuntimePollInterval,
		retryBase:    defaultRuntimeRetryBase, retryMax: defaultRuntimeRetryMax,
		active: make(map[string]*activeScannerRuntime), failed: make(map[string]failedRuntimeBuild), observer: observer,
	}
	manager.status.Store(runtimeManagerStatus{RPCListeners: []runtimeListenerStatus{}})
	return manager, nil
}

func (m *runtimeManager) Status() runtimeManagerStatus {
	status := m.status.Load().(runtimeManagerStatus)
	status.RPCListeners = append([]runtimeListenerStatus{}, status.RPCListeners...)
	return status
}

func (m *runtimeManager) Ready() bool {
	status := m.Status()
	return status.InitialReconcileComplete && status.Ready
}

func (m *runtimeManager) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	retryTimer := time.NewTimer(time.Hour)
	if !retryTimer.Stop() {
		<-retryTimer.C
	}
	defer retryTimer.Stop()
	defer m.shutdown()

	m.reconcile(ctx)
	m.resetRetryTimer(retryTimer)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.source.RPCListenerChanges():
			m.reconcile(ctx)
			m.resetRetryTimer(retryTimer)
		case <-ticker.C:
			m.reconcile(ctx)
			m.resetRetryTimer(retryTimer)
		case <-retryTimer.C:
			m.reconcile(ctx)
			m.resetRetryTimer(retryTimer)
		}
	}
}

func (m *runtimeManager) resetRetryTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	if len(m.failed) == 0 {
		return
	}
	now := time.Now()
	var delay time.Duration
	first := true
	for _, failure := range m.failed {
		candidate := failure.retryAfter.Sub(now)
		if candidate < 0 {
			candidate = 0
		}
		if first || candidate < delay {
			delay = candidate
			first = false
		}
	}
	timer.Reset(delay)
}

func (m *runtimeManager) reconcile(ctx context.Context) {
	m.reapExited(ctx)
	snapshot, err := m.source.RPCListenerSnapshot(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.logger.Error("load RPC listener desired state", "error", err)
		}
		status := m.Status()
		status.Ready = false
		now := time.Now().UTC()
		status.LastReconciledAt = &now
		m.status.Store(status)
		return
	}
	desired := make(map[string]core.RPCListener, len(snapshot.Listeners))
	for _, listener := range snapshot.Listeners {
		desired[listener.ID] = listener
	}

	for id := range m.active {
		if _, exists := desired[id]; !exists {
			m.stop(id, "configuration deleted")
		}
	}
	for id := range m.failed {
		if _, exists := desired[id]; !exists {
			delete(m.failed, id)
		}
	}

	ids := make([]string, 0, len(desired))
	for id := range desired {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	now := time.Now()
	for _, id := range ids {
		listener := desired[id]
		if listener.Paused {
			if m.active[id] != nil {
				m.stop(id, "listener paused")
			}
			delete(m.failed, id)
			continue
		}
		hash, err := runtimeConfigHash(snapshot.GlobalWebhooks, listener)
		if err != nil {
			m.recordFailure(id, hash, fmt.Errorf("hash runtime configuration: %w", err), now)
			m.observer.RuntimeBuildFailure(id, listener.ChainID)
			continue
		}
		if current := m.active[id]; current != nil && current.hash == hash {
			delete(m.failed, id)
			continue
		}
		if failure, exists := m.failed[id]; exists && failure.hash == hash && now.Before(failure.retryAfter) {
			continue
		}

		candidate, err := m.build(ctx, snapshot, listener)
		if err != nil {
			m.recordFailure(id, hash, err, now)
			m.observer.RuntimeBuildFailure(id, listener.ChainID)
			continue
		}
		if candidate == nil || candidate.runner == nil {
			if candidate != nil {
				candidate.closeRuntime()
			}
			m.recordFailure(id, hash, errors.New("runtime builder returned no scanner"), now)
			continue
		}
		if ctx.Err() != nil {
			candidate.closeRuntime()
			return
		}
		if m.active[id] != nil {
			m.stop(id, "configuration replaced")
		}
		m.start(ctx, listener, hash, candidate)
		delete(m.failed, id)
	}
	m.publishStatus(snapshot, time.Now().UTC())
}

func (m *runtimeManager) start(parent context.Context, listener core.RPCListener, hash [sha256.Size]byte, runtime *scannerRuntime) {
	runCtx, cancel := context.WithCancel(parent)
	active := &activeScannerRuntime{
		chainID: listener.ChainID, hash: hash, idle: runtime.idle, cancel: cancel,
		result: make(chan error, 1), close: runtime.close,
	}
	m.active[listener.ID] = active
	go func() {
		active.result <- runtime.runner.Run(runCtx)
	}()
	m.logger.Info("scanner runtime started", "rpc_listener_id", listener.ID, "chain_id", listener.ChainID)
}

func (m *runtimeManager) stop(id, reason string) {
	active := m.active[id]
	if active == nil {
		return
	}
	active.cancel()
	err := <-active.result
	if active.close != nil {
		active.close()
	}
	delete(m.active, id)
	if err != nil && !errors.Is(err, context.Canceled) {
		m.logger.Warn("scanner runtime stopped", "rpc_listener_id", id, "chain_id", active.chainID, "reason", reason, "error_summary", "scanner runtime failed")
	} else {
		m.logger.Info("scanner runtime stopped", "rpc_listener_id", id, "chain_id", active.chainID, "reason", reason)
	}
}

func (m *runtimeManager) reapExited(ctx context.Context) {
	for id, active := range m.active {
		select {
		case <-active.result:
			if active.close != nil {
				active.close()
			}
			delete(m.active, id)
			if ctx.Err() == nil {
				m.logger.Error("scanner runtime exited unexpectedly", "rpc_listener_id", id, "chain_id", active.chainID, "error_summary", "scanner runtime failed")
			}
		default:
		}
	}
}

func (m *runtimeManager) recordFailure(id string, hash [sha256.Size]byte, err error, now time.Time) {
	attempts := 1
	if previous, exists := m.failed[id]; exists && previous.hash == hash {
		attempts = previous.attempts + 1
	}
	delay := boundedBackoff(m.retryBase, m.retryMax, attempts)
	m.failed[id] = failedRuntimeBuild{hash: hash, attempts: attempts, retryAfter: now.Add(delay), lastError: safeRuntimeBuildError(err)}
	m.logger.Error("build scanner runtime", "rpc_listener_id", id, "attempt", attempts, "retry_in", delay, "error_summary", safeRuntimeBuildError(err))
}

func safeRuntimeBuildError(error) string { return "runtime construction failed" }

func (m *runtimeManager) publishStatus(snapshot core.RPCListenerSnapshot, reconciledAt time.Time) {
	statuses := make([]runtimeListenerStatus, 0, len(snapshot.Listeners))
	ready := true
	for _, listener := range snapshot.Listeners {
		entry := runtimeListenerStatus{ID: listener.ID, ChainID: listener.ChainID}
		hash, hashErr := runtimeConfigHash(snapshot.GlobalWebhooks, listener)
		active := m.active[listener.ID]
		failure, failed := m.failed[listener.ID]
		switch {
		case listener.Paused:
			entry.State = "paused"
		case hashErr == nil && active != nil && active.hash == hash && (!failed || failure.hash != hash):
			if active.idle {
				entry.State = "idle"
			} else {
				entry.State = "running"
			}
		case failed && failure.hash == hash:
			ready = false
			entry.State = "build-failed"
			if failure.attempts > 1 {
				entry.State = "retrying"
			}
			entry.Attempts = failure.attempts
			next := failure.retryAfter.UTC()
			entry.NextRetryAt = &next
			entry.LastError = failure.lastError
		default:
			ready = false
			entry.State = "build-failed"
			entry.LastError = "runtime construction failed"
		}
		statuses = append(statuses, entry)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
	reconciledAt = reconciledAt.UTC()
	m.status.Store(runtimeManagerStatus{
		DesiredRevision: snapshot.Revision, InitialReconcileComplete: true,
		LastReconciledAt: &reconciledAt, Ready: ready, RPCListeners: statuses,
	})
	m.observer.BeginRuntimeSnapshot(snapshot.Revision)
	for _, entry := range statuses {
		m.observer.RuntimeListener(entry.ID, entry.ChainID, entry.State)
	}
}

func (m *runtimeManager) shutdown() {
	ids := make([]string, 0, len(m.active))
	for id := range m.active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m.stop(id, "service shutdown")
	}
}

func runtimeConfigHash(global []core.WebhookConfig, listener core.RPCListener) ([sha256.Size]byte, error) {
	type runtimeEvent struct {
		Selector     string
		Destinations []core.WebhookDestination
	}
	type runtimeContract struct {
		Address string
		ABI     json.RawMessage
		Events  []runtimeEvent
	}
	type runtimeConfiguration struct {
		ChainID          uint64
		RPCURL           string
		RPCURLRef        string
		StartBlock       uint64
		BatchSize        uint64
		PollInterval     time.Duration
		Confirmations    uint64
		ReorgDepth       uint64
		RPCRetryAttempts int
		RPCRetryBackoff  time.Duration
		RPCTimeout       time.Duration
		TLS              core.ListenerTLSConfig
		Contracts        []runtimeContract
	}
	configuration := runtimeConfiguration{
		ChainID: listener.ChainID, RPCURL: listener.RPCURL, RPCURLRef: listener.RPCURLRef, StartBlock: listener.StartBlock,
		BatchSize: listener.BatchSize, PollInterval: listener.PollInterval, Confirmations: listener.Confirmations,
		ReorgDepth: listener.ReorgDepth, RPCRetryAttempts: listener.RPCRetryAttempts,
		RPCRetryBackoff: listener.RPCRetryBackoff, RPCTimeout: listener.RPCTimeout, TLS: listener.TLS,
	}
	for _, contract := range listener.Contracts {
		if len(contract.Events) == 0 {
			continue
		}
		configured := runtimeContract{Address: contract.Address, ABI: contract.ABI}
		for _, event := range contract.Events {
			configured.Events = append(configured.Events, runtimeEvent{
				Selector: event.Selector,
				Destinations: effectiveWebhookURLs(
					global, listener.Webhooks, contract.Webhooks, event.Webhooks,
				),
			})
		}
		configuration.Contracts = append(configuration.Contracts, configured)
	}
	payload, err := json.Marshal(configuration)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(payload), nil
}

func boundedBackoff(base, maximum time.Duration, attempts int) time.Duration {
	if base <= 0 || maximum < base {
		return maximum
	}
	delay := base
	for i := 1; i < attempts && delay < maximum; i++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func (runtime *scannerRuntime) closeRuntime() {
	if runtime != nil && runtime.close != nil {
		runtime.close()
	}
}
