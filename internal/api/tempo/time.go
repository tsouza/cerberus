package tempo

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

// parseTempoStartEnd reads optional `start` / `end` query parameters.
// Tempo accepts Unix seconds (typical), but the same nanosecond
// heuristic Loki / Prom apply here keeps the wire compatible with
// clients that send raw nanos (e.g. some Grafana plugins).
//
// Both bounds are optional; an absent value yields the zero time.Time,
// which the SelectBuilder treats as "no predicate".
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

// parseTempoTime decodes a single Unix-seconds-as-int (or
// nanoseconds-as-int when > 1e12) timestamp string. An empty input
// returns the zero time without an error — callers treat that as
// "predicate omitted". RFC3339 is also accepted for parity with Loki.
func parseTempoTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n > 1_000_000_000_000 {
			// Heuristic: > 1e12 means nanoseconds.
			return time.Unix(0, n).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Unix(int64(f), int64((f-float64(int64(f)))*1e9)).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, errors.New("time parameter must be Unix seconds/nanoseconds or RFC3339")
	}
	return t.UTC(), nil
}
