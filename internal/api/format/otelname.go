package format

import "sort"

// OTelToPromLabel returns s rewritten so it satisfies the Prometheus
// label-name grammar `[a-zA-Z_][a-zA-Z0-9_]*`.
//
// Rationale: cerberus stores OTel telemetry verbatim in ClickHouse —
// the per-row Attributes map (Prom metrics) and ResourceAttributes map
// (Loki logs) carry OTel semantic-convention keys like
// `service.name`, `http.request.method`, `network.protocol.version`.
// PromQL's grammar forbids `.` in identifiers, so a `sum by (service.name)`
// panel against cerberus's `/labels` output would silently fail to
// reference any of these keys. Reference Prometheus solves this on the
// receive side (the OTLP-to-Prom translator normalises before storing);
// cerberus translates on the wire-emit side instead so the CH-resident
// data stays OTel-shaped while the wire envelope stays Prom-shaped.
//
// Translation rules (per the OTel-Prometheus interop spec for labels):
//
//  1. Every byte outside `[A-Za-z0-9_]` is replaced with `_`. The Prom
//     label grammar also allows `:` for metric names but NOT for label
//     names — labels are stricter than metric names.
//  2. If the result starts with a digit, a single leading `_` is
//     prepended (`1invalid` → `_1invalid`).
//  3. Empty input returns empty (callers strip empty results separately).
//
// Examples:
//
//	service.name           → service_name
//	http.request.method    → http_request_method
//	network.protocol.name  → network_protocol_name
//	with-dash              → with_dash
//	1invalid               → _1invalid
//	already_ok             → already_ok
//	服务名                  → ___ (multibyte runes are byte-replaced)
//
// The function operates on bytes (not runes) because Prom's grammar is
// ASCII-only: any multibyte UTF-8 sequence is by definition outside
// `[A-Za-z0-9_]` and must be replaced. Byte-level replacement gives one
// `_` per byte of the multibyte rune, which matches the OTel-Prom
// interop spec wording.
func OTelToPromLabel(s string) string {
	if s == "" {
		return ""
	}
	if isPromLabelName(s) {
		return s
	}
	b := make([]byte, 0, len(s)+1)
	if s[0] >= '0' && s[0] <= '9' {
		b = append(b, '_')
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}

// OTelToPromMetric returns s rewritten so it satisfies the Prometheus
// metric-name grammar `[a-zA-Z_:][a-zA-Z0-9_:]*` — the same rule as
// labels except `:` is allowed. Used for `__name__` projection and
// `/api/v1/metadata` metric keys.
//
// Semantics match OTelToPromLabel except that `:` passes through
// untouched. OTel's own metric-name conventions don't currently use
// `:` (Prom recording rules do), so this is mostly belt-and-braces for
// pre-existing colon-shaped names that flow through cerberus.
func OTelToPromMetric(s string) string {
	if s == "" {
		return ""
	}
	if isPromMetricName(s) {
		return s
	}
	b := make([]byte, 0, len(s)+1)
	if s[0] >= '0' && s[0] <= '9' {
		b = append(b, '_')
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_', c == ':':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}

// isPromLabelName reports whether s already matches the Prom label-name
// grammar `[a-zA-Z_][a-zA-Z0-9_]*`. Fast-path so already-normalised
// keys pass through without allocation. The `__name__` synthetic label
// matches naturally (double underscores are valid).
func isPromLabelName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// isPromMetricName reports whether s already matches the Prom metric-name
// grammar `[a-zA-Z_:][a-zA-Z0-9_:]*` (the same as label except `:` is
// allowed).
func isPromMetricName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '_', c == ':':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// NormalizeLabelMap rewrites the keys of `in` through OTelToPromLabel
// and returns a fresh map. Values pass through unchanged (Prom values
// are arbitrary strings — only identifiers go through the grammar).
//
// Collision policy (per the task brief): when two source keys normalise
// to the same target — e.g. both `service.name` and `service_name` are
// present — the ALREADY-NORMALISED form wins. The dotted OTel form is
// the "telemetry alias" and the underscored form is the user's intended
// Prom-style identifier; surfacing only the underscored form keeps
// Grafana panels written against `{service_name="x"}` working without
// double-counting the same series.
//
// Implementation: iterate keys in sorted order so the result is
// deterministic regardless of Go's map iteration ordering. For each
// source key K we compute its normalised form N. If N != K (K needed
// rewriting) AND the input already contains N, we drop K — the natural
// form is the authoritative value. Otherwise the (normalised key →
// value) entry lands in the output. Among colliding rewrites where
// neither side is naturally-shaped (`a.b` and `a-b` both → `a_b`), the
// sorted-iteration order picks the lexically-first source — stable but
// arbitrary; the brief tolerates last-write-wins for this edge case.
//
// A nil / empty input returns nil so downstream consumers retain the
// `len(m) == 0` shortcut without an allocation.
func NormalizeLabelMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(in))
	for _, k := range keys {
		n := OTelToPromLabel(k)
		if n == "" {
			continue
		}
		if n != k {
			// k needed rewriting. If the natural form is already in the
			// input, drop k entirely (policy C: underscored form wins).
			if _, ok := in[n]; ok {
				continue
			}
		}
		// First writer wins for non-natural collisions. n == k means
		// `out[n]` was previously unset (sorted iteration visits each
		// natural key exactly once); the `_, exists` guard covers the
		// `a.b` + `a-b` shape so the later one doesn't clobber the
		// earlier.
		if _, exists := out[n]; exists {
			continue
		}
		out[n] = in[k]
	}
	return out
}

// NormalizeLabelNames returns a sorted, de-duplicated slice of label
// names with each entry passed through OTelToPromLabel. Collisions
// follow the same policy as NormalizeLabelMap: a naturally-shaped
// entry wins over a rewrite that would land on it.
//
// Used by the /labels endpoints in both prom + loki, which return a
// flat string slice rather than a label map. nil / empty input returns
// nil so the JSON envelope's `null`-vs-`[]` distinction is left to the
// caller.
func NormalizeLabelNames(in []string) []string {
	if len(in) == 0 {
		return in
	}
	natural := make(map[string]struct{}, len(in))
	for _, s := range in {
		if isPromLabelName(s) {
			natural[s] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		n := OTelToPromLabel(s)
		if n == "" {
			continue
		}
		if n != s {
			if _, ok := natural[n]; ok {
				// rewrite would collide with an already-natural form;
				// drop this one.
				continue
			}
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
