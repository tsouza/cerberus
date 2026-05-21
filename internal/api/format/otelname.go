package format

import (
	"sort"
	"strings"
)

// PromLabelToOTelCandidates returns the list of OTel-attribute-key
// candidates a Prom matcher name `s` could resolve to inside CH-stored
// rows. The first candidate is always `s` verbatim — the canonical
// Prom-grammar form callers already wrote. Subsequent candidates are
// the dotted re-expansions: every subset of the underscore positions
// in `s` is rewritten as a `.`, in order from "most underscores
// remain" to "every underscore is a dot". Leading underscores are
// preserved verbatim (Prom's `_1invalid` shape never came from a
// dotted form).
//
// Rationale: OTel writes attribute keys with dots (`service.name`,
// `cerberus.ql`, `http.request.method`). PR #657 normalised the
// WIRE-EMIT side so Grafana shows underscored aliases in the label
// picker, but the LOOKUP side of every PromQL matcher still hits
// `Attributes[<verbatim key>]`. When the user types `cerberus_ql`
// the lookup misses because the row's key is `cerberus.ql`. Listing
// all dotted re-expansions lets the caller emit a coalesce / if-chain
// against the Map so the lookup hits regardless of which form the
// data was written under.
//
// Ambiguity: `cerberus_query_total` could mean `cerberus.query.total`,
// `cerberus.query_total`, `cerberus_query.total`, or the original
// `cerberus_query_total`. The function enumerates every subset of the
// `_` positions; for `k` rewritable underscores it emits `2^k`
// candidates (deduped against `s` itself). To keep query SQL bounded
// the enumeration caps at `maxRewritableUnderscores = 6`
// (`2^6 = 64` candidates). Beyond the cap the function falls back to
// the two endpoints: `s` verbatim, plus the all-dots form. Real-world
// OTel keys carry 1-4 dots; the cap only kicks in for pathological
// names.
//
// Output ordering: `s` is always candidates[0]; the rest are sorted
// alphabetically so the emitted coalesce chain is deterministic
// across runs (regenerated golden snapshots stay byte-stable).
//
// Empty input returns `[""]` — callers can detect the "nothing to
// expand" shape via `len(out) == 1`. A name with no rewritable
// underscores (e.g. `job`, `__name__`) returns `[s]` as well; callers
// short-circuit when the slice has a single entry and emit the plain
// MapAccess they always did.
func PromLabelToOTelCandidates(s string) []string {
	if s == "" {
		return []string{""}
	}
	// Prom's synthetic labels (`__name__`, `__address__`,
	// `__metrics_path__`, ...) carry leading `__`. They never originate
	// as OTel-dotted keys, so the function returns them verbatim — no
	// dotted re-expansion.
	if strings.HasPrefix(s, "__") {
		return []string{s}
	}
	// Strip a single leading underscore from the "rewritable" set:
	// Prom's leading-underscore is a grammar marker (`_1invalid` from
	// `1invalid`) and never originated as a dotted segment.
	start := 0
	if start < len(s) && s[start] == '_' {
		start++
	}
	// Collect the indices of every internal underscore — those are the
	// positions we may rewrite to `.`.
	var positions []int
	for i := start; i < len(s); i++ {
		if s[i] == '_' {
			positions = append(positions, i)
		}
	}
	if len(positions) == 0 {
		return []string{s}
	}

	const maxRewritableUnderscores = 6
	if len(positions) > maxRewritableUnderscores {
		// Fallback: emit just `s` + the all-dots expansion. Two-entry
		// shape keeps the emitter footprint small for pathologically
		// long names.
		allDots := []byte(s)
		for _, p := range positions {
			allDots[p] = '.'
		}
		out := []string{s, string(allDots)}
		// Dedup in case s == allDots is impossible (positions non-empty),
		// but keep the explicit check for symmetry with the powerset path.
		if out[1] == out[0] {
			return out[:1]
		}
		return out
	}

	// Enumerate the powerset of `positions` — every subset is the set
	// of underscores that become dots. Bit i of the mask flips
	// positions[i].
	total := 1 << len(positions)
	seen := make(map[string]struct{}, total)
	out := make([]string, 0, total)
	seen[s] = struct{}{}
	out = append(out, s)
	buf := make([]byte, len(s))
	for mask := 1; mask < total; mask++ {
		copy(buf, s)
		for i, p := range positions {
			if mask&(1<<i) != 0 {
				buf[p] = '.'
			}
		}
		v := string(buf)
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	// Stable order: keep `s` first (so callers that only want the
	// canonical form can read [0]), sort the rest alphabetically.
	if len(out) > 2 {
		rest := out[1:]
		sort.Strings(rest)
	}
	return out
}

// PromLabelNeedsDottedFallback reports whether `s` has at least one
// candidate dotted expansion — i.e. the underlying CH row might hold
// the key under a dotted form even though the matcher addresses the
// underscored one. The function short-circuits the powerset
// enumeration with a simple "any internal underscore?" check; callers
// in lowering paths use this to gate the coalesce-emit branch so the
// fast path stays a single MapAccess when no rewrite is possible.
func PromLabelNeedsDottedFallback(s string) bool {
	// Prom synthetic labels (`__name__`, `__address__`, ...) are never
	// OTel-dotted; align with the PromLabelToOTelCandidates early-out
	// so the two functions agree on what's "rewritable".
	if strings.HasPrefix(s, "__") {
		return false
	}
	start := 0
	if start < len(s) && s[start] == '_' {
		start++
	}
	for i := start; i < len(s); i++ {
		if s[i] == '_' {
			return true
		}
	}
	return false
}

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
