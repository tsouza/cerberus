package format

import (
	"errors"
	"strconv"
	"time"
)

// ParseDuration parses a Prom / Loki style step / range duration.
// Accepts plain floats (interpreted as seconds) or Go-style durations
// like "30s", "5m", "1h". Empty input is an error so callers can
// distinguish "missing" from "0".
func ParseDuration(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, errors.New("missing duration")
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	return time.ParseDuration(raw)
}

// ParseTimeProm parses a Prometheus-API time parameter — Unix-seconds
// float, Unix-milliseconds int, or RFC3339 timestamp. Empty input falls
// back to def.
//
// Grafana's Prometheus datasource plugin sends millisecond timestamps
// when it routes through `/api/datasources/uid/<ds>/resources/...`
// (the JS frontend never converts to seconds on that path). Treating
// a 13-digit ms value as seconds yields a year ~58353 timestamp and
// overflows ClickHouse's `toDateTime64`, so the heuristic below routes
// values >= 1e12 to the ms branch. Plain seconds (~1.78e9 today) and
// fractional seconds stay on the float branch.
//
// Loki accepts integer-nanoseconds as well — handled by ParseTimeLoki.
func ParseTimeProm(raw string, def time.Time) (time.Time, error) {
	if raw == "" {
		return def, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 1_000_000_000_000 {
		// >= 1e12 ⇒ milliseconds. A seconds value this large would be
		// year ~33658, which no real client sends deliberately.
		return time.UnixMilli(n).UTC(), nil
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Unix(int64(f), int64((f-float64(int64(f)))*1e9)).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, errors.New("time parameter must be Unix seconds/milliseconds or RFC3339")
	}
	return t.UTC(), nil
}

// ParseTimeLoki parses a Loki-API time parameter. Loki accepts three
// shapes: Unix-seconds float, Unix-nanoseconds int (heuristic: >1e12),
// or RFC3339. Empty input falls back to def.
func ParseTimeLoki(raw string, def time.Time) (time.Time, error) {
	if raw == "" {
		return def, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 1_000_000_000_000 {
		// Heuristic: > 1e12 means nanoseconds (Loki convention).
		return time.Unix(0, n).UTC(), nil
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
