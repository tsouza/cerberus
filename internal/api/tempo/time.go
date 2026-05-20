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

// parseTempoTime decodes a single timestamp string. Tempo accepts
// integers in three magnitudes plus float-seconds and RFC3339:
//
//   - `< 1e12`       → Unix seconds (10-digit, the typical Tempo wire).
//   - `1e12 .. 1e15` → Unix milliseconds (13–15 digits). Grafana 11.x
//     sends ms over `/api/datasources/uid/<ds>/resources/...` for the
//     Tempo datasource just as it does for Prom and Loki — the JS
//     frontend never converts to seconds on that path. Treating ms as
//     ns was the failure mode of #194: a 13-digit value like
//     `1737000000000` decoded as ns yields year ~58353 →
//     `toDateTime64('58353-...', 9)` overflows in ClickHouse → 500.
//   - `>= 1e15`     → Unix nanoseconds (16+ digits). Tempo's own
//     `tempo-vulture` and some Grafana plugins emit raw ns. 2026 in ns
//     is ~1.74e18; 2001-09 in ns is ~1.0e18 — so 1e15 is a safe split.
//
// An empty input returns the zero time without an error — callers
// treat that as "predicate omitted". RFC3339 is also accepted for
// parity with Loki / Prom.
func parseTempoTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		switch {
		case n >= 1_000_000_000_000_000:
			// >= 1e15 ⇒ nanoseconds.
			return time.Unix(0, n).UTC(), nil
		case n >= 1_000_000_000_000:
			// 1e12..1e15 ⇒ milliseconds (Grafana resources proxy).
			return time.UnixMilli(n).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Unix(int64(f), int64((f-float64(int64(f)))*1e9)).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, errors.New("time parameter must be Unix seconds/milliseconds/nanoseconds or RFC3339")
	}
	return t.UTC(), nil
}
