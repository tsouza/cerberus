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
// As of PR #439, cerberus emits the real hex(TraceId) on
// `/api/search` (see internal/api/tempo/handler.go::toTraceSummaries),
// so both backends produce identical 32-hex-char TraceIDs for the
// same seeded span set. The differ keys its trace-summary multisets
// directly on `TraceSummary.TraceID` — no hashing, no canonicalisation.
// "Different orderings of equal sets don't false-positive" still holds
// because the index is a map keyed by TraceID, not a positional list.
//
// So the differ:
//
//   1. Indexes each TraceSummary by its real hex(TraceId).
//   2. Diffs the TraceID multisets directly (the index is a map, so
//      order on either side is irrelevant).
//   3. For matched entries, diffs the per-summary fields under a
//      relative-epsilon tolerance for `DurationMs` (clock vs
//      duration-column drift across backends is real).
//
// The differ is pure: a string of bytes (the two HTTP responses) goes
// in, a Diff comes out. Network + retry logic lives in main.go.

package main

import (
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

// traceKey is the per-trace identifier used to align the two backends'
// trace lists. Since PR #439, cerberus and Tempo both emit the real
// hex(TraceId) on `/api/search`, so the raw TraceID string is the
// natural key — no canonicalisation needed.
func traceKey(t TraceSummary) string {
	return t.TraceID
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
	// MatchedCount is the number of TraceID intersections that survived
	// field-level diffing.
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

// indexByKey deduplicates by traceKey (the real hex(TraceId)), keeping
// the first occurrence — a single backend should never return two
// traces with the same TraceID; if it does, the per-field diff will
// catch any inconsistency in the second copy.
func indexByKey(ts []TraceSummary) map[string]TraceSummary {
	out := make(map[string]TraceSummary, len(ts))
	for _, t := range ts {
		k := traceKey(t)
		if _, dup := out[k]; dup {
			continue
		}
		out[k] = t
	}
	return out
}

// compareSummary diffs two same-TraceID TraceSummaries. The `aLabel` /
// `bLabel` strings end up in the reasons so a downstream reader knows
// which side reported which value.
func compareSummary(key string, a, b TraceSummary, aLabel, bLabel string, opts DiffOptions) []DiffReason {
	var reasons []DiffReason

	if a.RootServiceName != b.RootServiceName {
		// Same TraceID, different rootServiceName — a real backend
		// regression in how span -> trace rollup picks the root.
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

// valuesClose mirrors compatibility/prometheus/shadow/differ.go's
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
	switch tc.Endpoint {
	case "tags_v1", "tags_v2":
		return assertTagsCase(tc, body, backendLabel)
	case "tag_values_v1", "tag_values_v2":
		return assertTagValuesCase(tc, body, backendLabel)
	default:
		return assertTraceSearchCase(tc, body, backendLabel)
	}
}

// assertTraceSearchCase is the original AssertCase body — applies the
// trace-search assertions (min/max traces, expected services, root-name
// regexp) to a SearchResponse body.
func assertTraceSearchCase(tc CorpusCase, body []byte, backendLabel string) ([]DiffReason, error) {
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

// assertTagsCase applies the tag-endpoint assertions: min/max list
// cardinality, expected-values subset (every value in tc.ExpectedValues
// must appear in the response), and (for tags_v2) expected-scopes
// subset.
func assertTagsCase(tc CorpusCase, body []byte, backendLabel string) ([]DiffReason, error) {
	v2 := tc.Endpoint == "tags_v2"
	tagNames, scopeNames, err := decodeTagNames(body, v2)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", backendLabel, err)
	}
	var reasons []DiffReason
	if tc.ExpectedMinValues > 0 && len(tagNames) < tc.ExpectedMinValues {
		reasons = append(reasons, DiffReason{
			Kind:   "assertion",
			Detail: fmt.Sprintf("%s: got %d tag names, want >= %d", backendLabel, len(tagNames), tc.ExpectedMinValues),
		})
	}
	if tc.ExpectedMaxValues > 0 && len(tagNames) > tc.ExpectedMaxValues {
		reasons = append(reasons, DiffReason{
			Kind:   "assertion",
			Detail: fmt.Sprintf("%s: got %d tag names, want <= %d", backendLabel, len(tagNames), tc.ExpectedMaxValues),
		})
	}
	if len(tc.ExpectedValues) > 0 {
		seen := stringSet(tagNames)
		for _, want := range tc.ExpectedValues {
			if !seen[want] {
				reasons = append(reasons, DiffReason{
					Kind:   "assertion",
					Detail: fmt.Sprintf("%s: expected tag name %q in response", backendLabel, want),
				})
			}
		}
	}
	if len(tc.ExpectedScopes) > 0 {
		if !v2 {
			reasons = append(reasons, DiffReason{
				Kind:   "assertion",
				Detail: fmt.Sprintf("%s: expected_scopes only meaningful for tags_v2", backendLabel),
			})
		} else {
			seen := stringSet(scopeNames)
			for _, want := range tc.ExpectedScopes {
				if !seen[want] {
					reasons = append(reasons, DiffReason{
						Kind:   "assertion",
						Detail: fmt.Sprintf("%s: expected scope %q in response (got %v)", backendLabel, want, scopeNames),
					})
				}
			}
		}
	}
	return reasons, nil
}

// assertTagValuesCase applies the tag-values assertions: min/max list
// cardinality + expected-values subset.
func assertTagValuesCase(tc CorpusCase, body []byte, backendLabel string) ([]DiffReason, error) {
	v2 := tc.Endpoint == "tag_values_v2"
	values, _, err := decodeTagValues(body, v2)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", backendLabel, err)
	}
	var reasons []DiffReason
	if tc.ExpectedMinValues > 0 && len(values) < tc.ExpectedMinValues {
		reasons = append(reasons, DiffReason{
			Kind:   "assertion",
			Detail: fmt.Sprintf("%s: got %d tag values, want >= %d", backendLabel, len(values), tc.ExpectedMinValues),
		})
	}
	if tc.ExpectedMaxValues > 0 && len(values) > tc.ExpectedMaxValues {
		reasons = append(reasons, DiffReason{
			Kind:   "assertion",
			Detail: fmt.Sprintf("%s: got %d tag values, want <= %d", backendLabel, len(values), tc.ExpectedMaxValues),
		})
	}
	if len(tc.ExpectedValues) > 0 {
		seen := stringSet(values)
		for _, want := range tc.ExpectedValues {
			if !seen[want] {
				reasons = append(reasons, DiffReason{
					Kind:   "assertion",
					Detail: fmt.Sprintf("%s: expected tag value %q in response (tag_name=%q)", backendLabel, want, tc.TagName),
				})
			}
		}
	}
	return reasons, nil
}

// stringSet is a tiny helper: returns a presence map keyed on the slice
// entries. Inlined in two places (CompareTagNames + assertion paths) so
// extracting it keeps both sites identical.
func stringSet(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, s := range in {
		out[s] = true
	}
	return out
}

// TagNamesResponseV1 mirrors `/api/search/tags`.
type TagNamesResponseV1 struct {
	TagNames []string `json:"tagNames"`
}

// TagNamesResponseV2 mirrors `/api/v2/search/tags`. Each scope carries
// a name (resource / span / intrinsic) and the keys belonging to it.
type TagNamesResponseV2 struct {
	Scopes []TagNamesScope `json:"scopes"`
}

// TagNamesScope is one V2 scope entry.
type TagNamesScope struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// TagValuesResponseV1 mirrors `/api/search/tag/{name}/values`.
type TagValuesResponseV1 struct {
	TagValues []string `json:"tagValues"`
}

// TagValuesResponseV2 mirrors `/api/v2/search/tag/{name}/values`. Each
// entry is a typed object so the autocomplete UI can render the type
// suffix on dynamic attributes.
type TagValuesResponseV2 struct {
	TagValues []TagValueV2 `json:"tagValues"`
}

// TagValueV2 is one typed value entry.
type TagValueV2 struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// decodeTagNames parses a tags response and returns the flattened set of
// tag names plus (for v2) the list of scope names that appeared. The
// flatten-then-set approach lets CompareTagNames lean on the same path
// for V1 and V2; the V2 scope partition is reported separately via the
// scopeNames return so the differ can surface scope-set mismatches.
func decodeTagNames(body []byte, v2 bool) (tagNames, scopeNames []string, err error) {
	if v2 {
		var out TagNamesResponseV2
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, nil, fmt.Errorf("decode /api/v2/search/tags response: %w", err)
		}
		seen := map[string]struct{}{}
		for _, sc := range out.Scopes {
			scopeNames = append(scopeNames, sc.Name)
			for _, t := range sc.Tags {
				if _, dup := seen[t]; dup {
					continue
				}
				seen[t] = struct{}{}
				tagNames = append(tagNames, t)
			}
		}
		sort.Strings(tagNames)
		sort.Strings(scopeNames)
		return tagNames, scopeNames, nil
	}
	var out TagNamesResponseV1
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, nil, fmt.Errorf("decode /api/search/tags response: %w", err)
	}
	tagNames = append(tagNames, out.TagNames...)
	sort.Strings(tagNames)
	return tagNames, nil, nil
}

// decodeTagValues parses a tag-values response and returns the flat
// list of values plus (for v2) the per-value Type strings keyed by
// value. The byValueType map lets CompareTagValues report mismatched
// type annotations on the intersection without false-positing when one
// side omits the V2 envelope.
func decodeTagValues(body []byte, v2 bool) (values []string, byValueType map[string]string, err error) {
	if v2 {
		var out TagValuesResponseV2
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, nil, fmt.Errorf("decode /api/v2/search/tag/{name}/values response: %w", err)
		}
		byValueType = make(map[string]string, len(out.TagValues))
		for _, tv := range out.TagValues {
			values = append(values, tv.Value)
			byValueType[tv.Value] = tv.Type
		}
		sort.Strings(values)
		return values, byValueType, nil
	}
	var out TagValuesResponseV1
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, nil, fmt.Errorf("decode /api/search/tag/{name}/values response: %w", err)
	}
	values = append(values, out.TagValues...)
	sort.Strings(values)
	return values, nil, nil
}

// CompareTagNames diffs two tags-endpoint response bodies. The diff is
// a set-difference on the flattened tag-name set: a key present on one
// side and missing on the other is reported as missing_in_a /
// missing_in_b. For tags_v2 the per-scope set is also diffed so a
// missing or extra scope name surfaces as a `scope_mismatch` reason.
func CompareTagNames(aBody, bBody []byte, aLabel, bLabel string, v2 bool) (Diff, error) {
	aNames, aScopes, err := decodeTagNames(aBody, v2)
	if err != nil {
		return Diff{}, fmt.Errorf("%s: %w", aLabel, err)
	}
	bNames, bScopes, err := decodeTagNames(bBody, v2)
	if err != nil {
		return Diff{}, fmt.Errorf("%s: %w", bLabel, err)
	}

	out := Diff{Equal: true}

	if len(aNames) != len(bNames) {
		out.Equal = false
		out.Reasons = append(out.Reasons, DiffReason{
			Kind:   "cardinality",
			Detail: fmt.Sprintf("%s=%d tag names, %s=%d tag names", aLabel, len(aNames), bLabel, len(bNames)),
		})
	}

	addSetDiff(&out, aNames, bNames, aLabel, bLabel)
	out.MatchedCount = countIntersection(aNames, bNames)

	if v2 {
		// Scope-set differences ride on top of the tag-name diff because
		// they describe a different axis (cerberus today returns the same
		// scope set regardless of ?scope=, real Tempo filters). Reporting
		// the scope axis explicitly keeps the markdown report actionable.
		addScopeDiff(&out, aScopes, bScopes, aLabel, bLabel)
	}

	return out, nil
}

// CompareTagValues diffs two tag-values-endpoint response bodies. The
// diff is a set-difference on the value list. For tag_values_v2 the
// per-value `Type` field is also compared on the intersection — a
// disagreement there is reported as `field_mismatch`.
func CompareTagValues(aBody, bBody []byte, aLabel, bLabel string, v2 bool) (Diff, error) {
	aValues, aTypes, err := decodeTagValues(aBody, v2)
	if err != nil {
		return Diff{}, fmt.Errorf("%s: %w", aLabel, err)
	}
	bValues, bTypes, err := decodeTagValues(bBody, v2)
	if err != nil {
		return Diff{}, fmt.Errorf("%s: %w", bLabel, err)
	}

	out := Diff{Equal: true}

	if len(aValues) != len(bValues) {
		out.Equal = false
		out.Reasons = append(out.Reasons, DiffReason{
			Kind:   "cardinality",
			Detail: fmt.Sprintf("%s=%d tag values, %s=%d tag values", aLabel, len(aValues), bLabel, len(bValues)),
		})
	}

	addSetDiff(&out, aValues, bValues, aLabel, bLabel)
	out.MatchedCount = countIntersection(aValues, bValues)

	if v2 && len(aTypes) > 0 && len(bTypes) > 0 {
		// Walk the intersection in deterministic order so the report is
		// reproducible; only report a type mismatch when both sides
		// populated the field (mirrors the trace differ's "skip blanks"
		// rule).
		intersection := intersect(aValues, bValues)
		for _, v := range intersection {
			at := aTypes[v]
			bt := bTypes[v]
			if at != "" && bt != "" && at != bt {
				out.Equal = false
				out.Reasons = append(out.Reasons, DiffReason{
					Kind:   "field_mismatch",
					Detail: fmt.Sprintf("value %q: type %s=%q vs %s=%q", v, aLabel, at, bLabel, bt),
				})
			}
		}
	}

	return out, nil
}

// addSetDiff appends missing_in_a / missing_in_b reasons for the
// symmetric difference of a + b. Order is deterministic so the report
// is reproducible across runs.
func addSetDiff(out *Diff, a, b []string, aLabel, bLabel string) {
	aSet := stringSet(a)
	bSet := stringSet(b)
	var missingInA, missingInB []string
	for s := range bSet {
		if !aSet[s] {
			missingInA = append(missingInA, s)
		}
	}
	for s := range aSet {
		if !bSet[s] {
			missingInB = append(missingInB, s)
		}
	}
	sort.Strings(missingInA)
	sort.Strings(missingInB)
	for _, s := range missingInA {
		out.Equal = false
		out.Reasons = append(out.Reasons, DiffReason{
			Kind:   "missing_in_a",
			Detail: fmt.Sprintf("%q present in %s but missing in %s", s, bLabel, aLabel),
		})
	}
	for _, s := range missingInB {
		out.Equal = false
		out.Reasons = append(out.Reasons, DiffReason{
			Kind:   "missing_in_b",
			Detail: fmt.Sprintf("%q present in %s but missing in %s", s, aLabel, bLabel),
		})
	}
}

// addScopeDiff is the V2-tags-specific scope-set diff. Reasons get a
// `scope_mismatch` kind so the report distinguishes them from the
// tag-name set diff.
func addScopeDiff(out *Diff, a, b []string, aLabel, bLabel string) {
	aSet := stringSet(a)
	bSet := stringSet(b)
	var missingInA, missingInB []string
	for s := range bSet {
		if !aSet[s] {
			missingInA = append(missingInA, s)
		}
	}
	for s := range aSet {
		if !bSet[s] {
			missingInB = append(missingInB, s)
		}
	}
	sort.Strings(missingInA)
	sort.Strings(missingInB)
	for _, s := range missingInA {
		out.Equal = false
		out.Reasons = append(out.Reasons, DiffReason{
			Kind:   "scope_mismatch",
			Detail: fmt.Sprintf("scope %q present in %s but missing in %s", s, bLabel, aLabel),
		})
	}
	for _, s := range missingInB {
		out.Equal = false
		out.Reasons = append(out.Reasons, DiffReason{
			Kind:   "scope_mismatch",
			Detail: fmt.Sprintf("scope %q present in %s but missing in %s", s, aLabel, bLabel),
		})
	}
}

// countIntersection counts the size of the set intersection of a + b.
// O(|a| + |b|); duplicates inside one side count once.
func countIntersection(a, b []string) int {
	aSet := stringSet(a)
	n := 0
	seen := map[string]bool{}
	for _, s := range b {
		if seen[s] {
			continue
		}
		seen[s] = true
		if aSet[s] {
			n++
		}
	}
	return n
}

// intersect returns the lexicographically sorted intersection of two
// string slices. Used by CompareTagValues to walk matched values in a
// reproducible order.
func intersect(a, b []string) []string {
	aSet := stringSet(a)
	out := make([]string, 0, len(b))
	seen := map[string]bool{}
	for _, s := range b {
		if seen[s] {
			continue
		}
		seen[s] = true
		if aSet[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
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
