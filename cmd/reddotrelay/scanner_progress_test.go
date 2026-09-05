package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestScannerProgressReturnsDashboardJSONSummary(t *testing.T) {
	tracker := newScannerProgressTracker()
	tracker.Head("listener-1", 1, 120, 118)
	tracker.BatchCommitted("listener-1", 1, 116, 118, 2)
	tracker.BatchFetched("listener-1", 1, 100, 116, 125*time.Millisecond)
	tracker.BatchFetched("listener-1", 1, 117, 118, 75*time.Millisecond)
	failedAt := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	tracker.ScanError("listener-1", 1, "RPC endpoint refused the connection while fetching event logs", failedAt)

	response := httptest.NewRecorder()
	handleScannerProgress(tracker, response, httptest.NewRequest(http.MethodGet, "/api/v1/scanner-progress", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("scanner progress status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		RPCListeners []scannerProgress `json:"rpcListeners"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode scanner progress: %v", err)
	}
	if len(payload.RPCListeners) != 1 || payload.RPCListeners[0].ListenerID != "listener-1" || payload.RPCListeners[0].LagBlocks != 2 || payload.RPCListeners[0].LastFetchMS != 75 || payload.RPCListeners[0].AverageFetchMS != 100 || payload.RPCListeners[0].LastError != "RPC endpoint refused the connection while fetching event logs" || !payload.RPCListeners[0].LastErrorAt.Equal(failedAt) {
		t.Fatalf("scanner progress = %#v", payload.RPCListeners)
	}
}

func TestScannerProgressUsesInitialCheckpointForLag(t *testing.T) {
	tracker := newScannerProgressTracker()
	tracker.CheckpointLoaded("listener-1", 1, 1_030_032)
	tracker.Head("listener-1", 1, 1_052_937, 1_052_925)
	progress := tracker.snapshot()
	if len(progress) != 1 || progress[0].Checkpoint != 1_030_032 || progress[0].LagBlocks != 22_893 {
		t.Fatalf("initial progress = %#v", progress)
	}
}
