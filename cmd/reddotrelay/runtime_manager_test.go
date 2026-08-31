package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestRuntimeManagerStatusEncodesEmptyListenersAsArray(t *testing.T) {
	manager, err := newRuntimeManager(newFakeRPCListenerSource(core.RPCListenerSnapshot{}), func(context.Context, core.RPCListenerSnapshot, core.RPCListener) (*scannerRuntime, error) {
		return nil, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manager.Status())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"rpcListeners":[]`)) {
		t.Fatalf("status JSON = %s", encoded)
	}
}

func TestRuntimeManagerStatusIdleRetryingAndSanitizedFailure(t *testing.T) {
	listener := runtimeListenerFixture()
	source := newFakeRPCListenerSource(core.RPCListenerSnapshot{Revision: 1, Listeners: []core.RPCListener{listener}})
	var logs bytes.Buffer
	manager, err := newRuntimeManager(source, func(context.Context, core.RPCListenerSnapshot, core.RPCListener) (*scannerRuntime, error) {
		return nil, errors.New("connect https://user:password@rpc.example.test?token=signed-secret ABI-secret")
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	manager.pollInterval = time.Hour
	manager.retryBase = 5 * time.Millisecond
	manager.retryMax = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntimeManager(manager, ctx)
	waitFor(t, func() bool {
		status := manager.Status()
		return len(status.RPCListeners) == 1 && status.RPCListeners[0].State == "retrying" && status.RPCListeners[0].Attempts >= 2
	}, "retrying runtime status")
	status := manager.Status().RPCListeners[0]
	if status.NextRetryAt == nil || status.LastError != "runtime construction failed" {
		t.Fatalf("retrying status = %#v", status)
	}
	for _, secret := range []string{"password", "signed-secret", "ABI-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("runtime logs leaked %q: %s", secret, logs.String())
		}
	}
	cancel()
	<-done

	idleSource := newFakeRPCListenerSource(core.RPCListenerSnapshot{Revision: 2, Listeners: []core.RPCListener{listener}})
	idleFactory := newFakeRuntimeFactory()
	idleManager := testRuntimeManager(t, idleSource, func(ctx context.Context, snapshot core.RPCListenerSnapshot, listener core.RPCListener) (*scannerRuntime, error) {
		runtime, err := idleFactory.build(ctx, snapshot, listener)
		if runtime != nil {
			runtime.idle = true
		}
		return runtime, err
	})
	idleCtx, idleCancel := context.WithCancel(context.Background())
	idleDone := runRuntimeManager(idleManager, idleCtx)
	waitFor(t, func() bool {
		current := idleManager.Status()
		return current.Ready && len(current.RPCListeners) == 1 && current.RPCListeners[0].State == "idle"
	}, "idle runtime status")
	idleCancel()
	<-idleDone
}

func TestRuntimeManagerAddsNoOpsReplacesAndDeletes(t *testing.T) {
	listener := runtimeListenerFixture()
	source := newFakeRPCListenerSource(core.RPCListenerSnapshot{Revision: 1, Listeners: []core.RPCListener{listener}})
	factory := newFakeRuntimeFactory()
	manager := testRuntimeManager(t, source, factory.build)

	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntimeManager(manager, ctx)
	waitFor(t, func() bool { return factory.startedCount() == 1 }, "initial scanner start")

	source.set(core.RPCListenerSnapshot{Revision: 1, Listeners: []core.RPCListener{listener}})
	time.Sleep(20 * time.Millisecond)
	if got := factory.buildCount(); got != 1 {
		t.Fatalf("no-op reconcile builds = %d, want 1", got)
	}

	listener.Name = "display-name-only"
	source.set(core.RPCListenerSnapshot{Revision: 2, Listeners: []core.RPCListener{listener}})
	time.Sleep(20 * time.Millisecond)
	if got := factory.buildCount(); got != 1 {
		t.Fatalf("metadata-only reconcile builds = %d, want 1", got)
	}

	listener.ChainID = 2
	source.set(core.RPCListenerSnapshot{Revision: 3, Listeners: []core.RPCListener{listener}})
	waitFor(t, func() bool { return factory.startedCount() == 2 && factory.stoppedCount() == 1 }, "scanner replacement")
	if got := factory.maxRunning.Load(); got != 1 {
		t.Fatalf("maximum concurrent scanners = %d, want 1", got)
	}

	source.set(core.RPCListenerSnapshot{Revision: 4})
	waitFor(t, func() bool { return factory.stoppedCount() == 2 && factory.closeCount() == 2 }, "scanner deletion")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
}

func TestRuntimeManagerStopsPausedListenerAndResumesFromDesiredState(t *testing.T) {
	listener := runtimeListenerFixture()
	source := newFakeRPCListenerSource(core.RPCListenerSnapshot{Revision: 1, Listeners: []core.RPCListener{listener}})
	factory := newFakeRuntimeFactory()
	manager := testRuntimeManager(t, source, factory.build)
	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntimeManager(manager, ctx)
	waitFor(t, func() bool { return factory.startedCount() == 1 }, "initial scanner start")
	listener.Paused = true
	source.set(core.RPCListenerSnapshot{Revision: 2, Listeners: []core.RPCListener{listener}})
	waitFor(t, func() bool {
		status := manager.Status()
		return factory.stoppedCount() == 1 && status.Ready && len(status.RPCListeners) == 1 && status.RPCListeners[0].State == "paused"
	}, "paused scanner state")
	if factory.buildCount() != 1 {
		t.Fatalf("pause built a scanner: %d", factory.buildCount())
	}
	listener.Paused = false
	source.set(core.RPCListenerSnapshot{Revision: 3, Listeners: []core.RPCListener{listener}})
	waitFor(t, func() bool {
		return factory.startedCount() == 2 && manager.Status().RPCListeners[0].State == "running"
	}, "resumed scanner")
	cancel()
	<-done
}

func TestRuntimeManagerKeepsOldRuntimeUntilReplacementBuildSucceeds(t *testing.T) {
	listener := runtimeListenerFixture()
	source := newFakeRPCListenerSource(core.RPCListenerSnapshot{Revision: 1, Listeners: []core.RPCListener{listener}})
	factory := newFakeRuntimeFactory()
	manager := testRuntimeManager(t, source, factory.build)
	manager.retryBase = 10 * time.Millisecond
	manager.retryMax = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntimeManager(manager, ctx)
	waitFor(t, func() bool { return factory.startedCount() == 1 }, "initial scanner start")

	factory.failNext.Store(1)
	listener.RPCURL = "https://replacement.example.test"
	source.set(core.RPCListenerSnapshot{Revision: 2, Listeners: []core.RPCListener{listener}})
	waitFor(t, func() bool { return factory.buildCount() >= 2 }, "failed replacement build")
	if factory.stoppedCount() != 0 || factory.running.Load() != 1 {
		t.Fatalf("old runtime stopped after failed build: stopped=%d running=%d", factory.stoppedCount(), factory.running.Load())
	}

	waitFor(t, func() bool { return factory.startedCount() == 2 && factory.stoppedCount() == 1 }, "replacement retry")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
}

func TestRuntimeManagerRetriesInitialBuildAndShutsDown(t *testing.T) {
	listener := runtimeListenerFixture()
	source := newFakeRPCListenerSource(core.RPCListenerSnapshot{Revision: 1, Listeners: []core.RPCListener{listener}})
	factory := newFakeRuntimeFactory()
	factory.failNext.Store(2)
	manager := testRuntimeManager(t, source, factory.build)
	manager.pollInterval = 2 * time.Millisecond
	manager.retryBase = 2 * time.Millisecond
	manager.retryMax = 4 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntimeManager(manager, ctx)
	waitFor(t, func() bool { return factory.startedCount() == 1 && factory.buildCount() >= 3 }, "successful retry")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	// Cancellation may race with a retry that has built, but not started, a
	// candidate. The manager must close both that candidate and the active
	// runtime; only the active runner should need to stop.
	if factory.stoppedCount() != 1 || factory.closeCount() < 1 || factory.running.Load() != 0 {
		t.Fatalf("shutdown state: stopped=%d closed=%d running=%d", factory.stoppedCount(), factory.closeCount(), factory.running.Load())
	}
}

func TestRuntimeManagerGlobalWebhookChangeReplacesEveryAffectedRuntime(t *testing.T) {
	first := runtimeListenerFixture()
	first.Contracts = []core.ContractConfig{{
		Address: "0x0000000000000000000000000000000000000001", ABI: []byte(`[]`),
		Events: []core.EventConfig{{Selector: "Transfer()"}},
	}}
	second := runtimeListenerFixture()
	second.ID = core.NewConfigID()
	second.ChainID = 2
	second.Contracts = first.Contracts
	second.Webhooks = []core.WebhookConfig{{URL: "https://chain.example.test"}}
	snapshot := core.RPCListenerSnapshot{
		Revision: 1, GlobalWebhooks: []core.WebhookConfig{{ID: core.NewConfigID(), URL: "https://one.example.test"}},
		Listeners: []core.RPCListener{first, second},
	}
	source := newFakeRPCListenerSource(snapshot)
	factory := newFakeRuntimeFactory()
	manager := testRuntimeManager(t, source, factory.build)

	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntimeManager(manager, ctx)
	waitFor(t, func() bool { return factory.startedCount() == 2 }, "initial scanner starts")
	snapshot.Revision = 2
	snapshot.GlobalWebhooks[0].URL = "https://two.example.test"
	source.set(snapshot)
	waitFor(t, func() bool { return factory.startedCount() == 3 && factory.stoppedCount() == 1 }, "global route replacements")
	if got := factory.maxRunning.Load(); got > 2 {
		t.Fatalf("maximum running scanners = %d, want at most 2", got)
	}
	cancel()
	<-done
}

func testRuntimeManager(t *testing.T, source rpcListenerSource, build scannerRuntimeBuilder) *runtimeManager {
	t.Helper()
	manager, err := newRuntimeManager(source, build, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	manager.pollInterval = time.Hour
	return manager
}

func runRuntimeManager(manager *runtimeManager, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	return done
}

func waitFor(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func runtimeListenerFixture() core.RPCListener {
	return core.RPCListener{
		ID: core.NewConfigID(), Name: "runtime", ChainID: 1, RPCURL: "https://rpc.example.test",
		StartBlock: 1, BatchSize: 100, PollInterval: time.Second, ReorgDepth: 12,
		RPCRetryAttempts: 2, RPCRetryBackoff: time.Millisecond, RPCTimeout: time.Second,
	}
}

type fakeRPCListenerSource struct {
	mu       sync.Mutex
	snapshot core.RPCListenerSnapshot
	changes  chan struct{}
}

func newFakeRPCListenerSource(snapshot core.RPCListenerSnapshot) *fakeRPCListenerSource {
	return &fakeRPCListenerSource{snapshot: snapshot, changes: make(chan struct{}, 1)}
}

func (source *fakeRPCListenerSource) RPCListenerSnapshot(context.Context) (core.RPCListenerSnapshot, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.snapshot, nil
}

func (source *fakeRPCListenerSource) RPCListenerChanges() <-chan struct{} { return source.changes }

func (source *fakeRPCListenerSource) set(snapshot core.RPCListenerSnapshot) {
	source.mu.Lock()
	source.snapshot = snapshot
	source.mu.Unlock()
	select {
	case source.changes <- struct{}{}:
	default:
	}
}

type fakeRuntimeFactory struct {
	mu         sync.Mutex
	runtimes   []*fakeScannerRunner
	builds     int
	closes     int
	failNext   atomic.Int32
	running    atomic.Int32
	maxRunning atomic.Int32
}

func newFakeRuntimeFactory() *fakeRuntimeFactory { return &fakeRuntimeFactory{} }

func (factory *fakeRuntimeFactory) build(context.Context, core.RPCListenerSnapshot, core.RPCListener) (*scannerRuntime, error) {
	factory.mu.Lock()
	factory.builds++
	factory.mu.Unlock()
	if factory.failNext.Load() > 0 && factory.failNext.Add(-1) >= 0 {
		return nil, errors.New("injected build failure")
	}
	runner := &fakeScannerRunner{factory: factory, started: make(chan struct{}), stopped: make(chan struct{})}
	factory.mu.Lock()
	factory.runtimes = append(factory.runtimes, runner)
	factory.mu.Unlock()
	return &scannerRuntime{runner: runner, close: func() {
		factory.mu.Lock()
		factory.closes++
		factory.mu.Unlock()
	}}, nil
}

func (factory *fakeRuntimeFactory) buildCount() int {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return factory.builds
}

func (factory *fakeRuntimeFactory) startedCount() int {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	count := 0
	for _, runtime := range factory.runtimes {
		select {
		case <-runtime.started:
			count++
		default:
		}
	}
	return count
}

func (factory *fakeRuntimeFactory) stoppedCount() int {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	count := 0
	for _, runtime := range factory.runtimes {
		select {
		case <-runtime.stopped:
			count++
		default:
		}
	}
	return count
}

func (factory *fakeRuntimeFactory) closeCount() int {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return factory.closes
}

type fakeScannerRunner struct {
	factory *fakeRuntimeFactory
	started chan struct{}
	stopped chan struct{}
}

func (runner *fakeScannerRunner) Run(ctx context.Context) error {
	running := runner.factory.running.Add(1)
	for {
		maximum := runner.factory.maxRunning.Load()
		if running <= maximum || runner.factory.maxRunning.CompareAndSwap(maximum, running) {
			break
		}
	}
	close(runner.started)
	<-ctx.Done()
	runner.factory.running.Add(-1)
	close(runner.stopped)
	return ctx.Err()
}
