package admit

import "go.opentelemetry.io/otel/metric"

// NewWithProvider exposes the unexported constructor for tests that
// need to point the rejection counter at a manual reader without
// touching the OTel global (which other parallel tests share). Builds
// the head's BudgetRequest limiter, mirroring New.
func NewWithProvider(head string, cap int, mp metric.MeterProvider) *Limiter {
	return newWithProvider(head, BudgetRequest, cap, mp)
}

// NewTailWithProvider is the NewTail counterpart of NewWithProvider:
// the head's BudgetTail limiter, with its rejection counter aimed at
// the caller's manual reader.
func NewTailWithProvider(head string, cap int, mp metric.MeterProvider) *Limiter {
	return newWithProvider(head, BudgetTail, cap, mp)
}
