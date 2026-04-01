package positionmanager

import "time"

// MetricsCollector defines the metrics interface for observability.
// The host application provides a concrete implementation (e.g. Prometheus).
// If nil in Config, a no-op implementation is used.
type MetricsCollector interface {
	// IncTriggerCount increments the counter for triggered levels.
	// Labels: chainID, levelType ("SL" or "TP"), direction ("LONG" or "SHORT").
	IncTriggerCount(chainID uint64, levelType string, direction string)

	// ObserveExecutionLatency records the time taken to execute a swap on-chain.
	// Labels: chainID, levelType.
	ObserveExecutionLatency(chainID uint64, levelType string, duration time.Duration)

	// IncErrorTotal increments the error counter.
	// Labels: chainID, operation (e.g. "execute_swap", "get_fee", "store_update").
	IncErrorTotal(chainID uint64, operation string)

	// ObserveGasSpent records gas spent on a transaction.
	// Labels: chainID, levelType.
	ObserveGasSpent(chainID uint64, levelType string, gasUsed uint64, gasPriceWei uint64)
}

// noopMetrics is a no-op implementation used when no MetricsCollector is configured.
type noopMetrics struct{}

func (noopMetrics) IncTriggerCount(uint64, string, string)                         {}
func (noopMetrics) ObserveExecutionLatency(uint64, string, time.Duration)           {}
func (noopMetrics) IncErrorTotal(uint64, string)                                    {}
func (noopMetrics) ObserveGasSpent(uint64, string, uint64, uint64)                  {}
