package format

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// msEpochFloor / nsEpochFloor split an integer Unix timestamp by scale:
// seconds stay below 1e12 (year 33658+), milliseconds below 1e15, and
// nanoseconds at or above it.
const (
	msEpochFloor = 1_000_000_000_000
	nsEpochFloor = 1_000_000_000_000_000
)

// A timestamp cerberus can carry end to end is one that fits an int64
// count of nanoseconds since the Unix epoch. Every scan bound this
// package feeds is rendered through `time.Time.UnixNano()` (see
// internal/api/tempo/handler.go's tsScanBound and internal/api/loki's
// timeBoundFrag), and the ClickHouse column it lands on is a
// DateTime64(9) — both are exactly int64 nanoseconds, and both fail
// silently or loudly outside that range: UnixNano() is documented as
// undefined and wraps, DateTime64(9) rejects the literal.
//
// Requests naming a bound past either edge are therefore clamped to the
// edge rather than converted. No row can exist outside the storage
// range, so the clamped window selects the identical set of rows, and
// the answer becomes the reference engines' "200 with whatever is in
// range" instead of a wrapped window (a silently wrong answer) or a
// toDateTime64 overflow surfaced as a 502.
var (
	minStorableTime = time.Unix(0, math.MinInt64).UTC()
	maxStorableTime = time.Unix(0, math.MaxInt64).UTC()
)

// The same two bounds expressed as Unix seconds, for range-checking a
// float or integer seconds value *before* converting it — the
// conversion itself is what overflows, so it cannot be the thing that
// detects the overflow.
const (
	minStorableUnixSeconds = float64(math.MinInt64) / float64(time.Second)
	maxStorableUnixSeconds = float64(math.MaxInt64) / float64(time.Second)
)

// clampStorable pins t into the representable window described above.
// Callers must hand it a time.Time built without an overflowing
// conversion; the seconds helpers below guarantee that.
func clampStorable(t time.Time) time.Time {
	switch {
	case t.Before(minStorableTime):
		return minStorableTime
	case t.After(maxStorableTime):
		return maxStorableTime
	}
	return t
}

// timeFromUnixSeconds converts whole Unix seconds, clamping instead of
// overflowing time.Unix's internal year-1 epoch arithmetic.
func timeFromUnixSeconds(sec int64) time.Time {
	switch {
	case float64(sec) < minStorableUnixSeconds:
		return minStorableTime
	case float64(sec) > maxStorableUnixSeconds:
		return maxStorableTime
	}
	return time.Unix(sec, 0).UTC()
}

// timeFromUnixFloatSeconds converts fractional Unix seconds. NaN fails
// every ordered comparison, so it is matched explicitly rather than
// being allowed to fall through to an implementation-defined
// float64 → int64 conversion.
func timeFromUnixFloatSeconds(f float64) time.Time {
	switch {
	case math.IsNaN(f), f < minStorableUnixSeconds:
		return minStorableTime
	case f > maxStorableUnixSeconds:
		return maxStorableTime
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * float64(time.Second))
	return clampStorable(time.Unix(sec, nsec).UTC())
}

// ParseDuration parses a Prom / Loki style step / range duration.
// Accepts plain floats (interpreted as seconds) or Go-style durations
// like "30s", "5m", "1h". Empty input is an error so callers can
// distinguish "missing" from "0".
//
// The int64-overflow guard on the float branch is not cerberus-specific
// caution: all three reference engines carry the identical check, and
// all three answer 400 rather than a wrapped duration —
// Prometheus web/api/v1/api.go:2273-2279, Loki
// pkg/loghttp/params.go:202-209, Tempo pkg/api/http.go:661-668. Without
// it `step=1e30` becomes an implementation-defined (in practice
// negative) time.Duration and the step grid is built from nonsense.
func ParseDuration(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, errors.New("missing duration")
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		ns := f * float64(time.Second)
		if math.IsNaN(ns) || ns > float64(math.MaxInt64) || ns < float64(math.MinInt64) {
			return 0, errors.New("duration overflows int64")
		}
		return time.Duration(ns), nil
	}
	return time.ParseDuration(raw)
}

// MinPositiveDuration returns the smaller of a and b, treating a
// non-positive value as "unbounded" (so it never wins the min). When
// both are non-positive the result is 0 (no cap). This is the
// effective-timeout rule the Prom and Loki heads share, and mirrors
// Prometheus's own: the engine uses the smaller of the configured query
// timeout and the per-request ?timeout=, and a disabled (zero) cap on
// either side does not clamp the other.
func MinPositiveDuration(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
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
// The float branch is deliberately NOT gated on the value containing a
// decimal point, unlike [ParseTimeUnixScaled]: Prometheus's own
// parseTime (web/api/v1/api.go:2249-2254) calls strconv.ParseFloat
// first and unconditionally, so `time=1e19` parses there rather than
// erroring. Prometheus then answers 200-with-no-samples for such a
// timestamp — MinTime / MaxTime are the defaults for an ABSENT
// parameter plus an RFC3339 boundary-string special case, never a
// rejection of a supplied one. Clamping to the storable window (above)
// is what reproduces that 200: the same query reached ClickHouse as
// `toDateTime64('58353-…', 9)` before, which is a CH error and a 502.
//
// Loki and Tempo accept integer-nanoseconds as well, and gate their
// float branch — handled by ParseTimeUnixScaled.
func ParseTimeProm(raw string, def time.Time) (time.Time, error) {
	if raw == "" {
		return def, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n >= msEpochFloor {
			// >= 1e12 ⇒ milliseconds. A seconds value this large would be
			// year ~33658, which no real client sends deliberately.
			return clampStorable(time.UnixMilli(n).UTC()), nil
		}
		return timeFromUnixSeconds(n), nil
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return timeFromUnixFloatSeconds(f), nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, errors.New("time parameter must be Unix seconds/milliseconds or RFC3339")
	}
	return clampStorable(t.UTC()), nil
}

// ParseTimeUnixScaled parses a time parameter in the widest wire shape
// cerberus accepts — the Loki and Tempo APIs both use it. Four integer
// shapes plus float-seconds and RFC3339:
//
//   - `< 1e12`      → Unix seconds (10-digit, current epoch).
//   - `1e12 .. 1e15` → Unix milliseconds (13–15 digits). Grafana 11.x
//     sends ms over `/api/datasources/uid/<ds>/resources/...` for the
//     Loki and Tempo datasources just as it does for Prom — the JS
//     frontend never converts to seconds on that path. Treating ms as
//     ns was the failure mode of #194: a 13-digit value like
//     `1737000000000` decoded as ns yields year ~58353 →
//     `toDateTime64('58353-...', 9)` overflows → ClickHouse returns 500
//     → Grafana sees empty results.
//   - `>= 1e15`     → Unix nanoseconds (16+ digits — the `logcli`
//     convention, and what Tempo's own `tempo-vulture` emits). 2026 in
//     ns is ~1.74e18 (19 digits); 2001-09 in ns is ~1.0e18 — so 1e15 is
//     a safe split well below every realistic ns timestamp and well
//     above every realistic ms timestamp (year 33658+ in ms).
//
// The float branch is reached only when the value contains a decimal
// point, exactly as reference Loki (pkg/loghttp/params.go:168-174) and
// reference Tempo (pkg/api/http.go:631-637) gate theirs. Both then fall
// to `strconv.ParseInt(value, 10, 64)`, which REJECTS an integer too
// large for int64, and finally to RFC3339 — so `start=10000000000000000000`
// is a 400 on both references. Ungated, cerberus accepted it as a float
// and answered 200 over a window the client never asked for.
//
// RFC3339 inputs fall through the int branch. Empty input falls back to
// def — pass the zero time where an absent parameter means "predicate
// omitted" rather than "use this default".
func ParseTimeUnixScaled(raw string, def time.Time) (time.Time, error) {
	if raw == "" {
		return def, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		switch {
		case n >= nsEpochFloor:
			// >= 1e15 ⇒ nanoseconds (logcli convention). Always
			// storable by construction: it already IS an int64
			// nanosecond count.
			return time.Unix(0, n).UTC(), nil
		case n >= msEpochFloor:
			// 1e12..1e15 ⇒ milliseconds (Grafana resources proxy).
			return clampStorable(time.UnixMilli(n).UTC()), nil
		default:
			return timeFromUnixSeconds(n), nil
		}
	}
	if strings.Contains(raw, ".") {
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return timeFromUnixFloatSeconds(f), nil
		}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, errors.New("time parameter must be Unix seconds/milliseconds/nanoseconds or RFC3339")
	}
	return clampStorable(t.UTC()), nil
}
