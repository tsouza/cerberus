package schema

import "fmt"

// MetricsTimestampPrecision is the physical type schema provisioning gives
// the metrics tables' timestamp columns (TimeUnix, StartTimeUnix and the
// Nested Exemplars.TimeUnix).
type MetricsTimestampPrecision string

const (
	// MetricsTimestampNanos types the timestamp columns DateTime64(9):
	// nanosecond precision.
	MetricsTimestampNanos MetricsTimestampPrecision = "nanos"
	// MetricsTimestampSeconds types the timestamp columns DateTime: second
	// precision, the OTel-CH exporter's own metrics schema from v0.157.
	// Sub-second sample timestamps are truncated on insert.
	MetricsTimestampSeconds MetricsTimestampPrecision = "seconds"
)

// ParseMetricsTimestampPrecision parses a configured precision. The empty
// string is MetricsTimestampNanos.
func ParseMetricsTimestampPrecision(s string) (MetricsTimestampPrecision, error) {
	switch p := MetricsTimestampPrecision(s); p {
	case "", MetricsTimestampNanos:
		return MetricsTimestampNanos, nil
	case MetricsTimestampSeconds:
		return p, nil
	default:
		return "", fmt.Errorf("invalid metrics timestamp precision %q (want %q or %q)", s, MetricsTimestampNanos, MetricsTimestampSeconds)
	}
}
