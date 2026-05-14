package admit

import "go.opentelemetry.io/otel/metric"

// NewWithProvider exposes the unexported constructor for tests that
// need to point the rejection counter at a manual reader without
// touching the OTel global (which other parallel tests share).
func NewWithProvider(head string, cap int, mp metric.MeterProvider) *Limiter {
	return newWithProvider(head, cap, mp)
}
