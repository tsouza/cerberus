package tempo

import (
	"errors"
	"net/http"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
)

// parseTempoStartEnd reads optional `start` / `end` query parameters.
// Tempo accepts Unix seconds (typical), but the same nanosecond
// heuristic Loki / Prom apply here keeps the wire compatible with
// clients that send raw nanos (e.g. some Grafana plugins).
//
// Both bounds are optional; an absent value yields the zero time.Time,
// which the QueryBuilder treats as "no predicate".
func parseTempoStartEnd(r *http.Request) (time.Time, time.Time, error) {
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")

	start, err := parseTempoTime(startStr)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := parseTempoTime(endStr)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !start.IsZero() && !end.IsZero() && end.Before(start) {
		return time.Time{}, time.Time{}, errors.New("'end' must not be before 'start'")
	}
	return start, end, nil
}

// boundDiscoveryWindow defaults a windowless tag / tag-value discovery
// request to the most recent DefaultSearchLookback. The discovery SQL
// (`SELECT DISTINCT arrayJoin(mapKeys(ResourceAttributes))` and the
// tag-value `mapContains` lookups) only emits a `Timestamp` predicate
// when start/end are non-zero; with no predicate the scan reads every
// row of otel_traces and explodes the Map column, running for minutes
// and dying on the per-query max_execution_time (CH code 159) — so the
// drilldown "never loads". otel_traces is `PARTITION BY toDate(Timestamp)`,
// so bounding to the last hour part-prunes the read dramatically (prod
// 814M-row table: ~839M rows / 30s+ timeout → ~8M rows / ~1s, the
// bounded query then completing well inside the existing deadline).
//
// Only the fully-windowless case is defaulted: a one-sided window is a
// deliberate open-ended bound and is left as-is, mirroring handleSearch.
// Returning [now-DefaultSearchLookback, now] also matches reference
// Tempo, which restricts windowless tag discovery to recent data.
//
// Exported so the gRPC tag endpoints (internal/api/tempo/grpc) apply the
// identical default after decoding their uint32 Start/End fields.
func BoundDiscoveryWindow(start, end time.Time) (time.Time, time.Time) {
	if start.IsZero() && end.IsZero() {
		end = time.Now().UTC()
		start = end.Add(-DefaultSearchLookback)
	}
	return start, end
}

// parseTempoTime decodes a single timestamp string. Tempo's wire shapes
// (Unix seconds / milliseconds / nanoseconds, float-seconds, RFC3339)
// are exactly the set [format.ParseTimeUnixScaled] decodes, including
// the ms-vs-ns split that #194 got wrong — so the scale thresholds live
// there once rather than being restated per head.
//
// An empty input returns the zero time without an error — callers treat
// that as "predicate omitted", which is why def is the zero time here
// rather than a now-anchored fallback.
func parseTempoTime(raw string) (time.Time, error) {
	return format.ParseTimeUnixScaled(raw, time.Time{})
}
