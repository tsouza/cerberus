package chclient

import "time"

// logrow.go — the log-stream row decode.
//
// [Sample] is the POSITIONAL row of the four/five-column projection every
// head's SQL emits: the scan binds column 1 into a String, column 2 into the
// label map, column 3 into a timestamp, column 4 into a float, and the
// optional fifth column into per-row metadata. What column 1 MEANS is
// per-query: a metric query projects the metric name into it, a log-stream
// query projects the log line (a log line is a String, so it cannot ride in
// the Float64 slot the metric value occupies).
//
// [LogRow] is the semantic row for the log-stream shape, and
// [DecodeLogRows] is the single place that positional knowledge is turned
// into named fields — so no consumer above chclient reads a log line out of
// a field named for the metric shape.

// LogRow is one row of a Loki log-stream result set: a single log line, the
// instant it was recorded, the stream labels that give it its stream
// identity, and its per-line structured metadata.
type LogRow struct {
	// Timestamp is the instant the line was recorded — the projection's
	// TimeUnix column.
	Timestamp time.Time
	// Line is the log line body.
	Line string
	// Labels is the stream identity: the label set a Loki `streams`
	// response groups entries under. The cursor interns it per series, so
	// consumers MUST treat it as read-only — the same contract [Sample]
	// documents for its own Labels.
	Labels map[string]string
	// Metadata is the per-line structured metadata Loki surfaces as the
	// third element of its `[ts, line, {metadata}]` value tuple. It is nil
	// when the projection carries no Metadata column, or when the row's
	// metadata map was empty.
	Metadata map[string]string
}

// DecodeLogRows decodes a log-stream result set into the named [LogRow]
// shape. The rows it takes are the positional [Sample] rows the shared
// cursor produced for a log-stream projection — `(<line>, Attributes,
// TimeUnix, Value[, Metadata])`.
//
// The projection's Value column is a constant placeholder: it exists only so
// the log-stream shape is as wide as the positional scan the shared cursor
// runs, and carries no log-stream meaning. Decoding drops it.
//
// A nil or empty input returns nil. Labels and Metadata are carried over by
// reference — Labels is the cursor's interned per-series map, so the
// read-only contract travels with it.
func DecodeLogRows(rows []Sample) []LogRow {
	if len(rows) == 0 {
		return nil
	}
	out := make([]LogRow, len(rows))
	for i, r := range rows {
		out[i] = LogRow{
			Timestamp: r.Timestamp,
			Line:      r.MetricName,
			Labels:    r.Labels,
			Metadata:  r.Metadata,
		}
	}
	return out
}
