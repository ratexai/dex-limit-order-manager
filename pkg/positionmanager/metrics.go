package positionmanager

import (
	"sync"
	"sync/atomic"
	"time"
)

// Metrics collects operational metrics for the position manager.
// The host application reads these via snapshot methods and exports them
// to its monitoring system (Prometheus, Datadog, etc.).
//
// All methods are safe for concurrent use.
type Metrics struct {
	// Counters.
	triggersRegistered atomic.Int64
	triggersFired      atomic.Int64
	executionsOK       atomic.Int64
	executionsFailed   atomic.Int64
	positionsOpened    atomic.Int64
	positionsClosed   atomic.Int64
	positionsCancelled atomic.Int64

	// Gas tracking.
	gasMu      sync.Mutex
	gasSpent   map[uint64]uint64 // chainID → cumulative gas (wei approx from gasUsed * gasPrice).

	// Latency tracking (execution duration in milliseconds).
	latMu          sync.Mutex
	latencyBuckets []int64 // Raw execution latencies for host-side histograms.
	latencyCount   int64
	latencySum     int64
}

// NewMetrics creates a new Metrics collector.
func NewMetrics() *Metrics {
	return &Metrics{
		gasSpent: make(map[uint64]uint64),
	}
}

// --- Recording methods (called internally by Manager) ---

func (m *Metrics) incTriggersRegistered()  { m.triggersRegistered.Add(1) }
func (m *Metrics) incTriggersFired()       { m.triggersFired.Add(1) }
func (m *Metrics) incExecutionOK()         { m.executionsOK.Add(1) }
func (m *Metrics) incExecutionFailed()     { m.executionsFailed.Add(1) }
func (m *Metrics) incPositionsOpened()     { m.positionsOpened.Add(1) }
func (m *Metrics) incPositionsClosed()     { m.positionsClosed.Add(1) }
func (m *Metrics) incPositionsCancelled()  { m.positionsCancelled.Add(1) }

func (m *Metrics) recordGas(chainID uint64, gasUsed uint64) {
	m.gasMu.Lock()
	m.gasSpent[chainID] += gasUsed
	m.gasMu.Unlock()
}

func (m *Metrics) recordLatency(d time.Duration) {
	ms := d.Milliseconds()
	m.latMu.Lock()
	m.latencyCount++
	m.latencySum += ms
	// Keep last 1000 observations for percentile calculation.
	if len(m.latencyBuckets) < 1000 {
		m.latencyBuckets = append(m.latencyBuckets, ms)
	} else {
		m.latencyBuckets[m.latencyCount%1000] = ms
	}
	m.latMu.Unlock()
}

// --- Snapshot methods (called by host for export) ---

// MetricsSnapshot is a point-in-time snapshot of all metrics.
type MetricsSnapshot struct {
	TriggersRegistered int64
	TriggersFired      int64
	ExecutionsOK       int64
	ExecutionsFailed   int64
	PositionsOpened    int64
	PositionsClosed    int64
	PositionsCancelled int64
	GasSpent           map[uint64]uint64 // chainID → cumulative gas.
	LatencyCount       int64             // Total executions measured.
	LatencyAvgMs       int64             // Average execution latency (ms).
}

// Snapshot returns a point-in-time copy of all metrics.
func (m *Metrics) Snapshot() MetricsSnapshot {
	gas := make(map[uint64]uint64)
	m.gasMu.Lock()
	for k, v := range m.gasSpent {
		gas[k] = v
	}
	m.gasMu.Unlock()

	m.latMu.Lock()
	latCount := m.latencyCount
	var latAvg int64
	if latCount > 0 {
		latAvg = m.latencySum / latCount
	}
	m.latMu.Unlock()

	return MetricsSnapshot{
		TriggersRegistered: m.triggersRegistered.Load(),
		TriggersFired:      m.triggersFired.Load(),
		ExecutionsOK:       m.executionsOK.Load(),
		ExecutionsFailed:   m.executionsFailed.Load(),
		PositionsOpened:    m.positionsOpened.Load(),
		PositionsClosed:    m.positionsClosed.Load(),
		PositionsCancelled: m.positionsCancelled.Load(),
		GasSpent:           gas,
		LatencyCount:       latCount,
		LatencyAvgMs:       latAvg,
	}
}
