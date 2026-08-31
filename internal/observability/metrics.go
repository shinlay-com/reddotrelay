package observability

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type DeliveryCountSource interface {
	DeliveryStatusCounts(context.Context) (pending, delivered, dead int64, err error)
}

type Metrics struct {
	registry              *prometheus.Registry
	scanCycles            *prometheus.CounterVec
	rpcRequests           *prometheus.CounterVec
	checkpoint            *prometheus.GaugeVec
	latestBlock           *prometheus.GaugeVec
	confirmedBlock        *prometheus.GaugeVec
	scannerLag            *prometheus.GaugeVec
	eventsProcessed       *prometheus.CounterVec
	reorgs                *prometheus.CounterVec
	deliveryAttempts      *prometheus.CounterVec
	configRevision        prometheus.Gauge
	runtimeListeners      *prometheus.GaugeVec
	runtimeBuildFailures  *prometheus.CounterVec
	buildInfo             *prometheus.GaugeVec
	eventsProcessedTotal  atomic.Uint64
	scannerErrorsTotal    atomic.Uint64
	deliveryFailuresTotal atomic.Uint64
}

func New(source DeliveryCountSource, version string) *Metrics {
	m := &Metrics{
		registry:             prometheus.NewRegistry(),
		scanCycles:           prometheus.NewCounterVec(prometheus.CounterOpts{Name: "reddotrelay_scanner_cycles_total", Help: "Scanner cycles by bounded outcome."}, []string{"rpc_listener_id", "chain_id", "outcome"}),
		rpcRequests:          prometheus.NewCounterVec(prometheus.CounterOpts{Name: "reddotrelay_rpc_requests_total", Help: "EVM RPC attempts by operation and bounded outcome."}, []string{"rpc_listener_id", "chain_id", "operation", "outcome"}),
		checkpoint:           prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "reddotrelay_checkpoint_block", Help: "Latest block durably checkpointed by a scanner runtime."}, []string{"rpc_listener_id", "chain_id"}),
		latestBlock:          prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "reddotrelay_latest_block", Help: "Latest block reported by the configured RPC endpoint."}, []string{"rpc_listener_id", "chain_id"}),
		confirmedBlock:       prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "reddotrelay_confirmed_head_block", Help: "Latest block eligible for confirmed scanning."}, []string{"rpc_listener_id", "chain_id"}),
		scannerLag:           prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "reddotrelay_scanner_lag_blocks", Help: "Confirmed head minus the latest durable checkpoint."}, []string{"rpc_listener_id", "chain_id"}),
		eventsProcessed:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "reddotrelay_events_processed_total", Help: "Decoded event observations included in durably committed scanner batches."}, []string{"rpc_listener_id", "chain_id"}),
		reorgs:               prometheus.NewCounterVec(prometheus.CounterOpts{Name: "reddotrelay_reorgs_total", Help: "Detected chain reorganizations."}, []string{"rpc_listener_id", "chain_id"}),
		deliveryAttempts:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "reddotrelay_delivery_attempts_total", Help: "Webhook attempts by terminal attempt outcome."}, []string{"outcome"}),
		configRevision:       prometheus.NewGauge(prometheus.GaugeOpts{Name: "reddotrelay_config_revision", Help: "Latest desired configuration revision reconciled by the runtime manager."}),
		runtimeListeners:     prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "reddotrelay_runtime_listeners", Help: "Current listener runtime state; exactly one state is 1 per configured listener."}, []string{"rpc_listener_id", "chain_id", "state"}),
		runtimeBuildFailures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "reddotrelay_runtime_build_failures_total", Help: "Scanner runtime construction failures."}, []string{"rpc_listener_id", "chain_id"}),
		buildInfo:            prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "reddotrelay_build_info", Help: "RedDotRelay build information."}, []string{"version"}),
	}
	m.registry.MustRegister(m.scanCycles, m.rpcRequests, m.checkpoint, m.latestBlock, m.confirmedBlock, m.scannerLag, m.eventsProcessed, m.reorgs, m.deliveryAttempts, m.configRevision, m.runtimeListeners, m.runtimeBuildFailures, m.buildInfo)
	m.buildInfo.WithLabelValues(version).Set(1)
	m.registry.MustRegister(prometheus.NewBuildInfoCollector(), prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	if source != nil {
		m.registry.MustRegister(&deliveryCollector{source: source, desc: prometheus.NewDesc("reddotrelay_deliveries", "Durable delivery rows by status.", []string{"status"}, nil)})
	}
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
func metricChainID(value uint64) string { return strconv.FormatUint(value, 10) }
func (m *Metrics) ScanCycle(listenerID string, chain uint64, outcome string) {
	m.scanCycles.WithLabelValues(listenerID, metricChainID(chain), outcome).Inc()
	if outcome != "success" {
		m.scannerErrorsTotal.Add(1)
	}
}
func (m *Metrics) RPCRequest(listenerID string, chain uint64, operation, outcome string) {
	m.rpcRequests.WithLabelValues(listenerID, metricChainID(chain), operation, outcome).Inc()
}
func (m *Metrics) Head(listenerID string, chain, latest, confirmed uint64) {
	m.latestBlock.WithLabelValues(listenerID, metricChainID(chain)).Set(float64(latest))
	m.confirmedBlock.WithLabelValues(listenerID, metricChainID(chain)).Set(float64(confirmed))
}
func (m *Metrics) BatchCommitted(listenerID string, chain, checkpoint, confirmed uint64, events int) {
	labels := []string{listenerID, metricChainID(chain)}
	m.checkpoint.WithLabelValues(labels...).Set(float64(checkpoint))
	m.eventsProcessed.WithLabelValues(labels...).Add(float64(events))
	if events > 0 {
		m.eventsProcessedTotal.Add(uint64(events))
	}
	lag := uint64(0)
	if confirmed > checkpoint {
		lag = confirmed - checkpoint
	}
	m.scannerLag.WithLabelValues(labels...).Set(float64(lag))
}
func (m *Metrics) Reorg(listenerID string, chain uint64) {
	m.reorgs.WithLabelValues(listenerID, metricChainID(chain)).Inc()
}
func (m *Metrics) DeliveryAttempt(outcome string) {
	m.deliveryAttempts.WithLabelValues(outcome).Inc()
	if outcome != "delivered" {
		m.deliveryFailuresTotal.Add(1)
	}
}

type Summary struct {
	EventsProcessedTotal  uint64 `json:"eventsProcessedTotal"`
	ScannerErrorsTotal    uint64 `json:"scannerErrorsTotal"`
	DeliveryFailuresTotal uint64 `json:"deliveryFailuresTotal"`
}

func (m *Metrics) Summary() Summary {
	if m == nil {
		return Summary{}
	}
	return Summary{EventsProcessedTotal: m.eventsProcessedTotal.Load(), ScannerErrorsTotal: m.scannerErrorsTotal.Load(), DeliveryFailuresTotal: m.deliveryFailuresTotal.Load()}
}
func (m *Metrics) BeginRuntimeSnapshot(revision uint64) {
	m.configRevision.Set(float64(revision))
	m.runtimeListeners.Reset()
}
func (m *Metrics) RuntimeListener(listenerID string, chain uint64, state string) {
	m.runtimeListeners.WithLabelValues(listenerID, metricChainID(chain), state).Set(1)
}
func (m *Metrics) RuntimeBuildFailure(listenerID string, chain uint64) {
	m.runtimeBuildFailures.WithLabelValues(listenerID, metricChainID(chain)).Inc()
}

type deliveryCollector struct {
	source DeliveryCountSource
	desc   *prometheus.Desc
}

func (c *deliveryCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }
func (c *deliveryCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pending, delivered, dead, err := c.source.DeliveryStatusCounts(ctx)
	if err != nil {
		return
	}
	for status, value := range map[string]int64{"pending": pending, "delivered": delivered, "dead": dead} {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(value), status)
	}
}
