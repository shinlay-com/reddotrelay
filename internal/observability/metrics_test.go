package observability

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

type fixedCounts struct{}

func (fixedCounts) DeliveryStatusCounts(context.Context) (int64, int64, int64, error) {
	return 2, 3, 1, nil
}

func TestMetricsExposeBoundedOperationalState(t *testing.T) {
	m := New(fixedCounts{}, "v0.3.0-test")
	m.Head("listener-1", 8453, 120, 118)
	m.RPCRequest("listener-1", 8453, "get_logs", "success")
	m.BatchCommitted("listener-1", 8453, 115, 118, 4)
	m.DeliveryAttempt("retry")
	m.DeliveryAttempt("delivered")
	m.ScanCycle("listener-1", 8453, "error")
	m.BeginRuntimeSnapshot(7)
	m.RuntimeListener("listener-1", 8453, "running")
	if summary := m.Summary(); summary.EventsProcessedTotal != 4 || summary.ScannerErrorsTotal != 1 || summary.DeliveryFailuresTotal != 1 {
		t.Fatalf("operational summary = %#v", summary)
	}

	response := httptest.NewRecorder()
	m.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if response.Code != 200 {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	for _, want := range []string{
		`reddotrelay_build_info{version="v0.3.0-test"} 1`,
		`reddotrelay_config_revision 7`,
		`reddotrelay_deliveries{status="pending"} 2`,
		`reddotrelay_delivery_attempts_total{outcome="retry"} 1`,
		`reddotrelay_scanner_lag_blocks{chain_id="8453",rpc_listener_id="listener-1"} 3`,
		`reddotrelay_runtime_listeners{chain_id="8453",rpc_listener_id="listener-1",state="running"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"url=", "destination=", "secret", "transaction_hash", "event_id", "address="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("metrics contain forbidden label or value %q", forbidden)
		}
	}
}
