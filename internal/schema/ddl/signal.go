package ddl

// Signal identifies a telemetry signal type. Each Signal value selects a
// fixed bundle of CREATE TABLE statements rendered from the upstream OTel
// ClickHouse Exporter's templates — see [Apply] for the per-signal table
// list.
type Signal int

const (
	// Metrics covers the 5 metrics tables: gauge, sum, histogram,
	// exp_histogram, summary.
	Metrics Signal = iota
	// Logs covers the single OTel logs table.
	Logs
	// Traces covers the spans table, the trace_id_ts lookup table, and
	// its materialized view (3 statements total).
	Traces
)

// All is shorthand for every signal cerberus knows about. Order matches
// the upstream exporter's per-signal start() hooks.
var All = []Signal{Metrics, Logs, Traces}

// String returns the human-readable signal name, used in error messages
// surfaced from [Apply].
func (s Signal) String() string {
	switch s {
	case Metrics:
		return "metrics"
	case Logs:
		return "logs"
	case Traces:
		return "traces"
	default:
		return "unknown"
	}
}
