package main

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"reddotrelay/internal/scanner"
)

type scannerProgress struct {
	ListenerID      string    `json:"rpcListenerId"`
	ChainID         uint64    `json:"chainId"`
	LatestBlock     uint64    `json:"latestBlock"`
	ConfirmedHead   uint64    `json:"confirmedHead"`
	Checkpoint      uint64    `json:"checkpoint"`
	LagBlocks       uint64    `json:"lagBlocks"`
	LastFetchMS     uint64    `json:"lastFetchMs,omitempty"`
	AverageFetchMS  uint64    `json:"averageFetchMs,omitempty"`
	LastVerifyMS    uint64    `json:"lastVerifyMs,omitempty"`
	AverageVerifyMS uint64    `json:"averageVerifyMs,omitempty"`
	UpdatedAt       time.Time `json:"updatedAt"`
	LastError       string    `json:"lastError,omitempty"`
	LastErrorAt     time.Time `json:"lastErrorAt,omitempty"`
}
type scannerProgressTracker struct {
	mu        sync.RWMutex
	listeners map[string]scannerProgress
	fetches   map[string]fetchTiming
	verifies  map[string]fetchTiming
}

type fetchTiming struct {
	samples uint64
	total   time.Duration
}

func newScannerProgressTracker() *scannerProgressTracker {
	return &scannerProgressTracker{listeners: make(map[string]scannerProgress), fetches: make(map[string]fetchTiming), verifies: make(map[string]fetchTiming)}
}
func (t *scannerProgressTracker) ScanCycle(id string, chain uint64, outcome string) {
	if outcome != "success" {
		return
	}
	t.mu.Lock()
	value := t.listeners[id]
	value.ListenerID, value.ChainID = id, chain
	value.LastError = ""
	value.LastErrorAt = time.Time{}
	t.listeners[id] = value
	t.mu.Unlock()
}
func (t *scannerProgressTracker) ScanError(id string, chain uint64, detail string, at time.Time) {
	t.mu.Lock()
	value := t.listeners[id]
	value.ListenerID, value.ChainID = id, chain
	value.LastError, value.LastErrorAt = detail, at.UTC()
	value.UpdatedAt = at.UTC()
	t.listeners[id] = value
	t.mu.Unlock()
}
func (t *scannerProgressTracker) RPCRequest(string, uint64, string, string) {}
func (t *scannerProgressTracker) Reorg(string, uint64)                      {}
func (t *scannerProgressTracker) BatchFetched(id string, chain, _, _ uint64, duration time.Duration) {
	t.mu.Lock()
	value := t.listeners[id]
	value.ListenerID, value.ChainID = id, chain
	value.LastFetchMS = uint64(duration.Milliseconds())
	timing := t.fetches[id]
	timing.samples++
	timing.total += duration
	value.AverageFetchMS = uint64((timing.total / time.Duration(timing.samples)).Milliseconds())
	t.fetches[id] = timing
	t.listeners[id] = value
	t.mu.Unlock()
}
func (t *scannerProgressTracker) BatchVerified(id string, chain, _, _ uint64, duration time.Duration) {
	t.mu.Lock()
	value := t.listeners[id]
	value.ListenerID, value.ChainID = id, chain
	value.LastVerifyMS = uint64(duration.Milliseconds())
	timing := t.verifies[id]
	timing.samples++
	timing.total += duration
	value.AverageVerifyMS = uint64((timing.total / time.Duration(timing.samples)).Milliseconds())
	t.verifies[id] = timing
	t.listeners[id] = value
	t.mu.Unlock()
}
func (t *scannerProgressTracker) CheckpointLoaded(listenerID string, chainID, checkpoint uint64) {
	t.mu.Lock()
	value := t.listeners[listenerID]
	value.ListenerID, value.ChainID, value.Checkpoint = listenerID, chainID, checkpoint
	t.listeners[listenerID] = value
	t.mu.Unlock()
}
func (t *scannerProgressTracker) Head(listenerID string, chainID, latest, confirmed uint64) {
	t.mu.Lock()
	value := t.listeners[listenerID]
	value.ListenerID = listenerID
	value.ChainID = chainID
	value.LatestBlock = latest
	value.ConfirmedHead = confirmed
	if confirmed > value.Checkpoint {
		value.LagBlocks = confirmed - value.Checkpoint
	} else {
		value.LagBlocks = 0
	}
	value.UpdatedAt = time.Now().UTC()
	t.listeners[listenerID] = value
	t.mu.Unlock()
}
func (t *scannerProgressTracker) BatchCommitted(listenerID string, chainID, checkpoint, confirmed uint64, _ int) {
	t.mu.Lock()
	value := t.listeners[listenerID]
	value.ListenerID = listenerID
	value.ChainID = chainID
	value.Checkpoint = checkpoint
	value.ConfirmedHead = confirmed
	if confirmed > checkpoint {
		value.LagBlocks = confirmed - checkpoint
	} else {
		value.LagBlocks = 0
	}
	value.UpdatedAt = time.Now().UTC()
	t.listeners[listenerID] = value
	t.mu.Unlock()
}
func (t *scannerProgressTracker) snapshot() []scannerProgress {
	t.mu.RLock()
	result := make([]scannerProgress, 0, len(t.listeners))
	for _, value := range t.listeners {
		result = append(result, value)
	}
	t.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].ListenerID < result[j].ListenerID })
	return result
}

type scannerObserverGroup []scanner.Observer

func (g scannerObserverGroup) ScanCycle(id string, chain uint64, outcome string) {
	for _, observer := range g {
		observer.ScanCycle(id, chain, outcome)
	}
}
func (g scannerObserverGroup) ScanError(id string, chain uint64, detail string, at time.Time) {
	for _, observer := range g {
		if errorObserver, ok := observer.(scanner.ErrorObserver); ok {
			errorObserver.ScanError(id, chain, detail, at)
		}
	}
}
func (g scannerObserverGroup) RPCRequest(id string, chain uint64, operation, outcome string) {
	for _, observer := range g {
		observer.RPCRequest(id, chain, operation, outcome)
	}
}
func (g scannerObserverGroup) BatchFetched(id string, chain, from, to uint64, duration time.Duration) {
	for _, observer := range g {
		if fetchObserver, ok := observer.(scanner.BatchFetchObserver); ok {
			fetchObserver.BatchFetched(id, chain, from, to, duration)
		}
	}
}
func (g scannerObserverGroup) BatchVerified(id string, chain, from, to uint64, duration time.Duration) {
	for _, observer := range g {
		if verificationObserver, ok := observer.(scanner.BatchVerificationObserver); ok {
			verificationObserver.BatchVerified(id, chain, from, to, duration)
		}
	}
}
func (g scannerObserverGroup) CheckpointLoaded(id string, chain, checkpoint uint64) {
	for _, observer := range g {
		if checkpointObserver, ok := observer.(scanner.CheckpointObserver); ok {
			checkpointObserver.CheckpointLoaded(id, chain, checkpoint)
		}
	}
}
func (g scannerObserverGroup) Head(id string, chain, latest, confirmed uint64) {
	for _, observer := range g {
		observer.Head(id, chain, latest, confirmed)
	}
}
func (g scannerObserverGroup) BatchCommitted(id string, chain, checkpoint, confirmed uint64, events int) {
	for _, observer := range g {
		observer.BatchCommitted(id, chain, checkpoint, confirmed, events)
	}
}
func (g scannerObserverGroup) Reorg(id string, chain uint64) {
	for _, observer := range g {
		observer.Reorg(id, chain)
	}
}

func handleScannerProgress(tracker *scannerProgressTracker, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if tracker == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"rpcListeners": []scannerProgress{}})
		return
	}
	progress := tracker.snapshot()
	writeJSON(writer, http.StatusOK, map[string]any{"rpcListeners": progress})
}
