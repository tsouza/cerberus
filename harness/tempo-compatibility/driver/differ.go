// Differ for the Tempo / TraceQL compatibility harness
// (PR 4 of docs/tempo-compliance-plan.md).
//
// The differ runs each corpus case against both backends (reference
// Tempo + cerberus) and produces a structured Diff describing what
// the two sides agreed / disagreed on. The diff is intentionally
// granular: every category of mismatch is reported as its own
// `DiffReason`, never collapsed into a single boolean, so the markdown
// report can surface actionable detail per case.
//
// Why not byte-equal? Cerberus's `TraceSummary.TraceID` is synthetic
// today (see internal/api/tempo/handler.go::toTraceSummaries — the
// stub key is `MetricName + "|" + Timestamp`, not the real 16-byte
// OTLP trace ID hex). Tempo's TraceID is the real hex. A literal
// byte-equal would false-positive on every case until cerberus
// projects the real TraceId column through to the search summary —
// outside PR 4's scope. The plan calls this out explicitly:
// "hash trace IDs deterministically (different orderings of equal
// sets don't false-positive)".
//
// So the differ:
//
//   1. Canonicalises each TraceSummary by deriving a stable key
//      `H(rootServiceName || rootTraceName)`. Both backends produce
//      the same hash for the same trace because they read the same
//      seeded fixture.
//   2. Sorts the trace list by the canonical key on each side.
//   3. Diffs the canonical-key multisets (matches "different
//      orderings of equal sets don't false-positive").
//   4. For matched entries, diffs the per-summary fields under a
//      relative-epsilon tolerance for `DurationMs` (clock vs
//      duration-column drift across backends is real).
//
// The differ is pure: a string of bytes (the two HTTP responses) goes
// in, a Diff comes out. Network + retry logic lives in main.go.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// SearchResponse is the minimal subset of Tempo's / cerberus's
// /api/search payload the differ needs. Field tags use the documented
// camelCase shape; both backends emit this.
//
// We define a private copy (rather than depending on
// internal/api/tempo/types.go) so the driver stays in `package main`
// without bridging through cerberus's API package. The shape is small
// and stable — TraceID, RootServiceName, RootTraceName, StartTime,
// DurationMs is the contract Grafana parses, so all three (Tempo,
// cerberus, the differ) read the same fields.
type SearchResponse struct {
	Traces  []TraceSummary `json:"traces"`
	Metrics any            `json:"metrics,omitempty"` // ignored by differ
}

// TraceSummary mirrors Tempo's /api/search trace element. All fields
// optional — Tempo sometimes omits StartTimeUnixNano + DurationMs in
// older builds; cerberus sometimes omits RootServiceName / RootTraceName
// when projections drop them. The differ tolerates either side missing
// fields by skipping that field's compare and recording a reason.
type TraceSummary struct {
	TraceID           string `json:"traceID"`
	RootServiceName   string `json:"rootServiceName,omitempty"`
	RootTraceName     string `json:"rootTraceName,omitempty"`
	StartTimeUnixNano string `json:"startTimeUnixNano,omitempty"`
	DurationMs        int    `json:"durationMs,omitempty"`
}

// CanonicalKey is the per-trace hash used to align the two backends'
// trace lists. Format: hex(SHA-256(rootServiceName + "\x00" +
// rootTraceName)) truncated to 16 hex chars (8 bytes of entropy is
// plenty: 100 traces would collide with p ≈ 100²/(2·2⁶⁴) ≈ 2.7e-16).
//
// Truncation keeps the key short in the markdown report. The full
// 64-hex hash is fine too — only readability suffers.
func CanonicalKey(t TraceSummary) string {
	h := sha256.Sum256([]byte(t.RootServiceName + "\x00" + t.RootTraceName))
	return hex.EncodeToString(h[:8])
}

// DiffOptions controls numeric-field tolerance. Mirrors the prom
// shadow differ's shape so an eventual unifier has a clear migration
// target.
type DiffOptions struct {
	// AbsEpsilon: absolute tolerance for fields near zero.
	AbsEpsilon float64
	// RelEpsilon: relative tolerance for non-zero fields.
	RelEpsilon float64
}

// DefaultDiffOptions returns 1e-9 abs + rel — same defaults as the
// PromQL shadow harness so the two converge if/when we factor a
// shared `compat/differ`.
func DefaultDiffOptions() DiffOptions {
	return DiffOptions{AbsEpsilon: 1e-9, RelEpsilon: 1e-9}
}

// DiffReason is one structured complaint surfaced by Compare. Kept as
// a struct (not a free-form string) so the markdown report can
// group / filter by Kind without parsing strings.
type DiffReason struct {
	Kind   string // "cardinality" | "missing_in_a" | "missing_in_b" | "field_mismatch"
	Detail string // human-readable; safe to embed in the markdown body
}

// Diff is the structured outcome of comparing two parsed
// SearchResponse bodies.
type Diff struct {
	// Equal is true iff the two sides match within tolerances.
	Equal bool
	// MatchedCount is the number of canonical-key intersections that
	// survived field-level diffing.
	MatchedCount int
	// Reasons enumerates every mismatch in deterministic order.
	Reasons []DiffReason
}

// Compare diffs two raw HTTP response bodies. `aLabel` / `bLabel` are
// embedded in the human-readable reasons so the report distinguishes
// "missing on tempo side" from "missing on cerberus side".
func Compare(aBody, bBody []byte, aLabel, bLabel string, opts DiffOptions) (Diff, error) {
	if opts.AbsEpsilon == 0 && opts.RelEpsilon == 0 {
		opts = DefaultDiffOptions()
	}

	a, err := decodeSearch(aBody)
	if err != nil {
		return Diff{}, fmt.Errorf("%s: %w", aLabel, err)
	}
	b, err := decodeSearch(bBody)
	if err != nil {
		return Diff{}, fmt.Errorf("%s: %w", bLabel, err)
	}

	out := Diff{Equal: true}

	// Cardinality is reported as a reason but does not by itself drive
	// Equal=false — the per-key intersection / extras below already
	// surface what's actually missing; reporting cardinality on top
	// keeps the high-level metric visible in the markdown summary
	// without double-counting against Equal.
	if len(a.Traces) != len(b.Traces) {
		out.Equal = false
		out.Reasons = append(out.Reasons, DiffReason{
			Kind:   "cardinality",
			Detail: fmt.Sprintf("%s=%d traces, %s=%d traces", aLabel, len(a.Traces), bLabel, len(b.Traces)),
		})
	}

	aByKey := indexByKey(a.Traces)
	bByKey := indexByKey(b.Traces)

	var missingInA, missingInB []string
	for key := range bByKey {
		if _, ok := aByKey[key]; !ok {
			missingInA = append(missingInA, key)
		}
	}
	for key := range aByKey {
		if _, ok := bByKey[key]; !ok {
			missingInB = append(missingInB, key)
		}
	}
	sort.Strings(missingInA)
	sort.Strings(missingInB)
	for _, key := range missingInA {
		out.Equal = false
		out.Reasons = append(out.Reasons, DiffReason{
			Kind:   "missing_in_a",
			Detail: fmt.Sprintf("key %s present in %s but missing in %s (rootServiceName=%q rootTraceName=%q)", key, bLabel, aLabel, bByKey[key].RootServiceName, bByKey[key].RootTraceName),
		})
	}
	for _, key := range missingInB {
		out.Equal = false
		out.Reasons = append(out.Reasons, DiffReason{
			Kind:   "missing_in_b",
			Detail: fmt.Sprintf("key %s present in %s but missing in %s (rootServiceName=%q rootTraceName=%q)", key, aLabel, bLabel, aByKey[key].RootServiceName, aByKey[key].RootTraceName),
		})
	}

	// Intersection — diff every per-summary field that both sides
	// populate. Skipping a side's blank field keeps the differ from
	// false-positing on optional-field omission (Tempo and cerberus
	// can each legitimately omit StartTimeUnixNano in some builds).
	matched := make([]string, 0, len(aByKey))
	for key := range aByKey {
		if _, ok := bByKey[key]; ok {
			matched = append(matched, key)
		}
	}
	sort.Strings(matched)

	for _, key := range matched {
		as := aByKey[key]
		bs := bByKey[key]
		reasons := compareSummary(key, as, bs, aLabel, bLabel, opts)
		if len(reasons) > 0 {
			out.Equal = false
			out.Reasons = append(out.Reasons, reasons...)
			continue
		}
		out.MatchedCount++
	}

	return out, nil
}

func decodeSearch(body []byte) (SearchResponse, error) {
	var out SearchResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return SearchResponse{}, fmt.Errorf("decode /api/search response: %w", err)
	}
	return out, nil
}

// indexByKey deduplicates by CanonicalKey, keeping the first occurrence
// — a single backend should never return two traces with the same
// (rootServiceName, rootTraceName) for the corpus shapes we ship in
// PR 4 (the seeder gives every trace a unique name); if it does, the
// per-field diff will catch any inconsistency in the second copy.
func indexByKey(ts []TraceSummary) map[string]TraceSummary {
	out := make(map[string]TraceSummary, len(ts))
	for _, t := range ts {
		k := CanonicalKey(t)
		if _, dup := out[k]; dup {
			continue
		}
		out[k] = t
	}
	return out
}

// compareSummary diffs two same-canonical-key TraceSummaries. The
// `aLabel` / `bLabel` strings end up in the reasons so a downstream
// reader knows which side reported which value.
func compareSummary(key string, a, b TraceSummary, aLabel, bLabel string, opts DiffOptions) []DiffReason {
	var reasons []DiffReason

	if a.RootServiceName != b.RootServiceName {
		// CanonicalKey hashes (rootServiceName, rootTraceName), so a
		// mismatch here only happens on hash collision — but we still
		// report it because a future canonical-key change would surface
		// here first.
		reasons = append(reasons, DiffReason{
			Kind:   "field_mismatch",
			Detail: fmt.Sprintf("key %s: rootServiceName %s=%q vs %s=%q", key, aLabel, a.RootServiceName, bLabel, b.RootServiceName),
		})
	}
	if a.RootTraceName != b.RootTraceName {
		reasons = append(reasons, DiffReason{
			Kind:   "field_mismatch",
			Detail: fmt.Sprintf("key %s: rootTraceName %s=%q vs %s=%q", key, aLabel, a.RootTraceName, bLabel, b.RootTraceName),
		})
	}

	// Numeric fields tolerate the configured epsilon. DurationMs varies
	// because Tempo computes it from wall-clock span boundaries and
	// cerberus reads the Duration column directly; on a fresh seed the
	// two should agree to the nanosecond, but float jitter in repeated
	// runs is real.
	if a.DurationMs != 0 || b.DurationMs != 0 {
		if !valuesClose(float64(a.DurationMs), float64(b.DurationMs), opts) {
			reasons = append(reasons, DiffReason{
				Kind:   "field_mismatch",
				Detail: fmt.Sprintf("key %s: durationMs %s=%d vs %s=%d", key, aLabel, a.DurationMs, bLabel, b.DurationMs),
			})
		}
	}

	// StartTimeUnixNano is a string in the JSON shape; treat blank on
	// either side as a non-comparison rather than a mismatch so older
	// Tempo builds (which omit this field) don't false-positive.
	if a.StartTimeUnixNano != "" && b.StartTimeUnixNano != "" && a.StartTimeUnixNano != b.StartTimeUnixNano {
		// Allow the configured epsilon on the parsed value to absorb
		// quantization differences between block-flush vs in-memory
		// reads.
		an, errA := strconv.ParseFloat(a.StartTimeUnixNano, 64)
		bn, errB := strconv.ParseFloat(b.StartTimeUnixNano, 64)
		if errA != nil || errB != nil || !valuesClose(an, bn, opts) {
			reasons = append(reasons, DiffReason{
				Kind:   "field_mismatch",
				Detail: fmt.Sprintf("key %s: startTimeUnixNano %s=%q vs %s=%q", key, aLabel, a.StartTimeUnixNano, bLabel, b.StartTimeUnixNano),
			})
		}
	}

	return reasons
}

// valuesClose mirrors harness/prometheus-compliance/shadow/differ.go's
// helper so the two harnesses share semantics.
func valuesClose(a, b float64, opts DiffOptions) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return a == b
	}
	delta := math.Abs(a - b)
	if delta <= opts.AbsEpsilon {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return delta <= opts.RelEpsilon*scale
}

// AssertCase checks one backend's parsed response against the corpus
// case's expectations. Separate from Compare because per-side
// assertions ("at least N traces", "root names match regex X") are
// orthogonal to differential equality between backends.
//
// Returns reasons for every expectation that failed. An empty slice
// means all expectations passed.
func AssertCase(tc CorpusCase, body []byte, backendLabel string) ([]DiffReason, error) {
	var reasons []DiffReason
	a, err := decodeSearch(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", backendLabel, err)
	}
	if tc.ExpectedMinTraces > 0 && len(a.Traces) < tc.ExpectedMinTraces {
		reasons = append(reasons, DiffReason{
			Kind:   "assertion",
			Detail: fmt.Sprintf("%s: got %d traces, want >= %d", backendLabel, len(a.Traces), tc.ExpectedMinTraces),
		})
	}
	if tc.ExpectedMaxTraces > 0 && len(a.Traces) > tc.ExpectedMaxTraces {
		reasons = append(reasons, DiffReason{
			Kind:   "assertion",
			Detail: fmt.Sprintf("%s: got %d traces, want <= %d", backendLabel, len(a.Traces), tc.ExpectedMaxTraces),
		})
	}
	if len(tc.ExpectedServices) > 0 {
		seen := map[string]bool{}
		for _, t := range a.Traces {
			seen[t.RootServiceName] = true
		}
		for _, svc := range tc.ExpectedServices {
			if !seen[svc] {
				reasons = append(reasons, DiffReason{
					Kind:   "assertion",
					Detail: fmt.Sprintf("%s: expected rootServiceName=%q to appear in results", backendLabel, svc),
				})
			}
		}
	}
	if tc.ExpectedRootNameRE != nil {
		for _, t := range a.Traces {
			if !tc.ExpectedRootNameRE.MatchString(t.RootTraceName) {
				reasons = append(reasons, DiffReason{
					Kind:   "assertion",
					Detail: fmt.Sprintf("%s: rootTraceName %q does not match /%s/", backendLabel, t.RootTraceName, tc.ExpectedRootNameRE.String()),
				})
				break // one report is enough; the report should highlight failures, not enumerate every span
			}
		}
	}
	return reasons, nil
}

// canonicalizeJSON re-marshals a JSON blob with sorted object keys so
// future call sites can byte-compare the canonicalised form. Kept here
// (vs in main.go) because the differ owns the canonicalisation policy
// for the entire harness.
//
// The function is generic over the body shape — it does NOT assume the
// /api/search envelope — so it remains usable for /api/traces/<id>
// when that endpoint gets diffed in a follow-up.
func canonicalizeJSON(body []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("canonicalize: parse: %w", err)
	}
	return marshalCanonical(v)
}

func marshalCanonical(v any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			b.Write(kb)
			b.WriteByte(':')
			vb, err := marshalCanonical(t[k])
			if err != nil {
				return nil, err
			}
			b.Write(vb)
		}
		b.WriteByte('}')
		return []byte(b.String()), nil
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			vb, err := marshalCanonical(e)
			if err != nil {
				return nil, err
			}
			b.Write(vb)
		}
		b.WriteByte(']')
		return []byte(b.String()), nil
	default:
		return json.Marshal(t)
	}
}
