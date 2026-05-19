// Differ + semantic-consistency layer for the Tempo /api/metrics/*
// endpoints (PR 5 of docs/tempo-compliance-plan.md).
//
// PR 4's differ.go handles /api/search exclusively; PR 5 extends the
// driver with metrics endpoints. Rather than retrofit Compare() to
// switch on response shape, this file owns the metrics-side pipeline
// end to end:
//
//   1. CompareMetrics  — structural diff of two MetricsResponse bodies
//                        (label-set canonicalisation, sample epsilon).
//   2. SemanticChecks  — registry of per-backend consistency invariants
//                        (samples_non_negative, labels_match_query, etc.)
//                        that catch the "both backends are wrong in
//                        different ways" failure mode the plan calls out.
//   3. AssertMetricsCase + RunSemanticChecks — driven from diffCase.
//
// Both backends emit Tempo's tempopb KeyValue + AnyValue label shape
// on the wire — `{"labels":[{"key":"X","value":{"stringValue":"Y"}}], ...}`
// — matching `pkg/tempopb/common/v1` rendered via gogo `jsonpb`.
//
// Cerberus's `/api/metrics/query_range` historically emitted a flatter
// `{"key":"X","value":"Y"}` form; that was fixed to match the tempopb
// projection (EF #398). The decoder below keeps a fallback path for the
// flat string shape so old replay fixtures (and any consumer still on
// the legacy shape) keep round-tripping — the structural diff compares
// the extracted string value, not the JSON envelope.

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

// FlexInt64 is an int64 that decodes from either a JSON number or a
// JSON string-encoded integer. Tempo's /api/metrics response encodes
// `timestampMs` as a JSON string in recent tags (the protobuf-to-JSON
// projection emits int64 fields as strings to dodge JS-number precision
// loss), while cerberus emits the same field as a plain number. The
// differ needs to decode both shapes.
//
// The underlying type is int64 so existing callers that read the field
// (`%d` formatting, `<` comparisons, arithmetic) keep working without
// any conversion.
type FlexInt64 int64

// UnmarshalJSON accepts either a JSON number or a JSON string holding
// a base-10 integer. Any other shape (object, bool, null) returns an
// error so a malformed body still fails loudly.
func (f *FlexInt64) UnmarshalJSON(b []byte) error {
	// Strip outer quotes if present so we can parse the inner integer
	// uniformly. JSON strings are guaranteed to start with `"` and end
	// with `"`; everything else is forwarded to strconv as-is.
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("FlexInt64: cannot parse %q as int64: %w", string(b), err)
	}
	*f = FlexInt64(n)
	return nil
}

// MetricsResponse is the differ-side struct mirroring Tempo's
// /api/metrics/query_range envelope (and the /api/metrics/query
// `series` envelope — Tempo's InstantSeries carries a single Value
// instead of a Samples slice, which we expose via the Value field).
//
// Both endpoints share the same Series + Exemplars layout for label
// canonicalisation purposes; only the per-sample shape differs.
type MetricsResponse struct {
	Series []MetricsSeriesEntry `json:"series"`
}

// MetricsSeriesEntry is one entry of MetricsResponse.Series. The
// Labels field decodes either {key, value:"X"} or {key, value:{stringValue:"X"}}
// via the custom MetricsLabel.UnmarshalJSON. The Samples + Exemplars +
// Value fields are mutually-optional: range responses populate Samples,
// instant responses populate Value, and either may carry Exemplars.
type MetricsSeriesEntry struct {
	Labels    []MetricsLabel    `json:"labels"`
	Samples   []MetricsSample   `json:"samples,omitempty"`
	Exemplars []MetricsExemplar `json:"exemplars,omitempty"`
	// Value is the single-point value for instant responses
	// (Tempo's InstantSeries shape). Zero is a legitimate value, so the
	// differ relies on the Samples-vs-Value distinction at the response
	// level (instant responses omit Samples) rather than a zero check.
	Value *float64 `json:"value,omitempty"`
}

// MetricsLabel is a label entry tolerant to both wire shapes (see the
// custom UnmarshalJSON below).
type MetricsLabel struct {
	Key   string
	Value string
}

// metricsLabelWire is the union shape we accept on the wire. Either
// Value is a plain string (cerberus today) or it is a nested object
// {stringValue: "..."} (Tempo's tempopb projection of AnyValue).
//
// The struct is intentionally not exported; only UnmarshalJSON consumes it.
type metricsLabelWire struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// UnmarshalJSON parses a label entry in either wire shape.
func (l *MetricsLabel) UnmarshalJSON(data []byte) error {
	var w metricsLabelWire
	if err := json.Unmarshal(data, &w); err != nil {
		return fmt.Errorf("metrics label decode: %w", err)
	}
	l.Key = w.Key
	if len(w.Value) == 0 {
		return nil
	}
	// Try flat string first — cheap.
	var s string
	if err := json.Unmarshal(w.Value, &s); err == nil {
		l.Value = s
		return nil
	}
	// Try AnyValue-shaped object. We pull only the string form; other
	// AnyValue variants (int_value, double_value, bool_value) get
	// stringified via fmt.Sprint so the differ has a comparable string.
	var anyV struct {
		StringValue *string  `json:"stringValue"`
		IntValue    *string  `json:"intValue"` // tempopb wire shape: string-encoded int64
		DoubleValue *float64 `json:"doubleValue"`
		BoolValue   *bool    `json:"boolValue"`
	}
	if err := json.Unmarshal(w.Value, &anyV); err != nil {
		return fmt.Errorf("metrics label %q value: not a recognized shape: %w", l.Key, err)
	}
	switch {
	case anyV.StringValue != nil:
		l.Value = *anyV.StringValue
	case anyV.IntValue != nil:
		l.Value = *anyV.IntValue
	case anyV.DoubleValue != nil:
		l.Value = fmt.Sprint(*anyV.DoubleValue)
	case anyV.BoolValue != nil:
		l.Value = fmt.Sprint(*anyV.BoolValue)
	default:
		// Empty AnyValue. Tempo emits this for nil group-by values
		// (the "nil" bucket in cardinality cases). The empty string is
		// a stable canonical form both backends agree on.
		l.Value = ""
	}
	return nil
}

// MetricsSample is a single (timestampMs, value) point. TimestampMs
// uses FlexInt64 so the differ accepts both wire shapes Tempo and
// cerberus emit (number vs string-encoded int64); see FlexInt64.
type MetricsSample struct {
	TimestampMs FlexInt64 `json:"timestampMs"`
	Value       float64   `json:"value"`
}

// MetricsExemplar is one exemplar attached to a series. The Labels are
// the span-level attributes that produced the exemplar (subset of the
// span's tags); Value is the sample value at that exemplar's timestamp.
type MetricsExemplar struct {
	Labels      []MetricsLabel `json:"labels,omitempty"`
	Value       float64        `json:"value"`
	TimestampMs FlexInt64      `json:"timestampMs"`
}

// metricsCanonicalKey produces the differ-internal series identifier
// for label-set alignment across the two backends. The key is the hex
// SHA-256 of the sorted "k=v\n" form so distinct label permutations
// fold to the same key; truncated to 16 hex chars for readability in
// the markdown report (collision floor ~2.7e-16 at 100 series).
func metricsCanonicalKey(s MetricsSeriesEntry) string {
	pairs := make([]string, 0, len(s.Labels))
	for _, l := range s.Labels {
		pairs = append(pairs, l.Key+"="+l.Value)
	}
	sort.Strings(pairs)
	h := sha256.Sum256([]byte(strings.Join(pairs, "\n")))
	return hex.EncodeToString(h[:8])
}

// decodeMetrics parses one body. Errors carry the side-label so
// downstream reasons distinguish "tempo decode failed" from "cerberus
// decode failed".
func decodeMetrics(body []byte) (MetricsResponse, error) {
	var out MetricsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return MetricsResponse{}, fmt.Errorf("decode /api/metrics response: %w", err)
	}
	return out, nil
}

// CompareMetrics is the metrics-shape counterpart to Compare. It
// canonicalises both sides by label-set hash, reports cardinality +
// missing-on-either-side reasons, and for matched series diffs the
// sample stream (or instant value) under the configured epsilon.
//
// Exemplar counts are reported as a reason when they diverge but they
// do not by themselves drive Equal=false; the structural-diff layer
// is intentionally lenient on exemplars because both backends may
// sample exemplars differently (Tempo caps at 100 by default, cerberus
// emits zero today). The structural-equality contract is "same series
// label sets + same sample stream"; exemplar parity is a follow-up
// goal tracked in the open questions section of the plan.
func CompareMetrics(aBody, bBody []byte, aLabel, bLabel string, opts DiffOptions) (Diff, error) {
	if opts.AbsEpsilon == 0 && opts.RelEpsilon == 0 {
		opts = DefaultDiffOptions()
	}

	a, err := decodeMetrics(aBody)
	if err != nil {
		return Diff{}, fmt.Errorf("%s: %w", aLabel, err)
	}
	b, err := decodeMetrics(bBody)
	if err != nil {
		return Diff{}, fmt.Errorf("%s: %w", bLabel, err)
	}

	out := Diff{Equal: true}

	if len(a.Series) != len(b.Series) {
		out.Equal = false
		out.Reasons = append(out.Reasons, DiffReason{
			Kind:   "cardinality",
			Detail: fmt.Sprintf("%s=%d series, %s=%d series", aLabel, len(a.Series), bLabel, len(b.Series)),
		})
	}

	aByKey := indexMetricsByKey(a.Series)
	bByKey := indexMetricsByKey(b.Series)

	var missingInA, missingInB []string
	for k := range bByKey {
		if _, ok := aByKey[k]; !ok {
			missingInA = append(missingInA, k)
		}
	}
	for k := range aByKey {
		if _, ok := bByKey[k]; !ok {
			missingInB = append(missingInB, k)
		}
	}
	sort.Strings(missingInA)
	sort.Strings(missingInB)
	for _, k := range missingInA {
		out.Equal = false
		out.Reasons = append(out.Reasons, DiffReason{
			Kind:   "missing_in_a",
			Detail: fmt.Sprintf("series key %s present in %s but missing in %s (%s)", k, bLabel, aLabel, formatLabels(bByKey[k].Labels)),
		})
	}
	for _, k := range missingInB {
		out.Equal = false
		out.Reasons = append(out.Reasons, DiffReason{
			Kind:   "missing_in_b",
			Detail: fmt.Sprintf("series key %s present in %s but missing in %s (%s)", k, aLabel, bLabel, formatLabels(aByKey[k].Labels)),
		})
	}

	matched := make([]string, 0, len(aByKey))
	for k := range aByKey {
		if _, ok := bByKey[k]; ok {
			matched = append(matched, k)
		}
	}
	sort.Strings(matched)

	for _, k := range matched {
		reasons, informational := compareMetricsSeries(k, aByKey[k], bByKey[k], aLabel, bLabel, opts)
		// Informational reasons (today: exemplar_count) are surfaced in
		// the report but never drive Equal=false; see the function's
		// doc-comment for why.
		out.Reasons = append(out.Reasons, informational...)
		if len(reasons) > 0 {
			out.Equal = false
			out.Reasons = append(out.Reasons, reasons...)
			continue
		}
		out.MatchedCount++
	}

	return out, nil
}

func indexMetricsByKey(ss []MetricsSeriesEntry) map[string]MetricsSeriesEntry {
	out := make(map[string]MetricsSeriesEntry, len(ss))
	for _, s := range ss {
		k := metricsCanonicalKey(s)
		if _, dup := out[k]; dup {
			continue
		}
		out[k] = s
	}
	return out
}

// compareMetricsSeries diffs two same-canonical-key series.
//
// For range responses, the sample streams are compared element-wise
// after sort-by-timestamp (both backends should already emit ascending
// order, but the comparator tolerates an out-of-order stream because
// the differ's job is structural correctness, not wire-order
// enforcement — Grafana sorts on its end).
//
// For instant responses, the single Value is compared; a Value-pointer
// difference between a populated and nil pointer is itself a mismatch
// (one side returned an instant point, the other didn't).
//
// Returns (reasons, informational). `reasons` are mismatches that
// drive Equal=false (cardinality of samples, sample-value drift, etc.).
// `informational` are reasons surfaced in the report but explicitly
// excluded from the Equal verdict — today, only exemplar count
// divergence falls in this bucket (see CompareMetrics doc-comment).
func compareMetricsSeries(key string, a, b MetricsSeriesEntry, aLabel, bLabel string, opts DiffOptions) (reasons, informational []DiffReason) {
	switch {
	case a.Value != nil && b.Value != nil:
		if !valuesClose(*a.Value, *b.Value, opts) {
			reasons = append(reasons, DiffReason{
				Kind:   "field_mismatch",
				Detail: fmt.Sprintf("key %s: instant value %s=%g vs %s=%g", key, aLabel, *a.Value, bLabel, *b.Value),
			})
		}
	case a.Value != nil && b.Value == nil:
		reasons = append(reasons, DiffReason{
			Kind:   "field_mismatch",
			Detail: fmt.Sprintf("key %s: %s returned instant value but %s did not", key, aLabel, bLabel),
		})
	case a.Value == nil && b.Value != nil:
		reasons = append(reasons, DiffReason{
			Kind:   "field_mismatch",
			Detail: fmt.Sprintf("key %s: %s returned instant value but %s did not", key, bLabel, aLabel),
		})
	}

	if len(a.Samples) != len(b.Samples) {
		reasons = append(reasons, DiffReason{
			Kind:   "field_mismatch",
			Detail: fmt.Sprintf("key %s: samples count %s=%d vs %s=%d", key, aLabel, len(a.Samples), bLabel, len(b.Samples)),
		})
		// Fall through to a per-position best-effort diff anyway; both
		// sides may share a prefix worth flagging individually.
	}

	aSamples := append([]MetricsSample(nil), a.Samples...)
	bSamples := append([]MetricsSample(nil), b.Samples...)
	sort.Slice(aSamples, func(i, j int) bool { return aSamples[i].TimestampMs < aSamples[j].TimestampMs })
	sort.Slice(bSamples, func(i, j int) bool { return bSamples[i].TimestampMs < bSamples[j].TimestampMs })

	n := len(aSamples)
	if len(bSamples) < n {
		n = len(bSamples)
	}
	for i := 0; i < n; i++ {
		if aSamples[i].TimestampMs != bSamples[i].TimestampMs {
			reasons = append(reasons, DiffReason{
				Kind:   "field_mismatch",
				Detail: fmt.Sprintf("key %s: sample[%d] ts %s=%d vs %s=%d", key, i, aLabel, aSamples[i].TimestampMs, bLabel, bSamples[i].TimestampMs),
			})
			continue
		}
		if !valuesClose(aSamples[i].Value, bSamples[i].Value, opts) {
			reasons = append(reasons, DiffReason{
				Kind:   "field_mismatch",
				Detail: fmt.Sprintf("key %s: sample[%d]@%d value %s=%g vs %s=%g", key, i, aSamples[i].TimestampMs, aLabel, aSamples[i].Value, bLabel, bSamples[i].Value),
			})
		}
	}

	// Exemplar count divergence is informational only — see the
	// CompareMetrics doc-comment for why we don't drive Equal=false on
	// exemplar disagreement.
	if len(a.Exemplars) != len(b.Exemplars) {
		informational = append(informational, DiffReason{
			Kind:   "exemplar_count",
			Detail: fmt.Sprintf("key %s: exemplars %s=%d vs %s=%d (informational)", key, aLabel, len(a.Exemplars), bLabel, len(b.Exemplars)),
		})
	}

	return reasons, informational
}

// AssertMetricsCase runs the per-side cardinality / samples-per-series
// expectations declared on the corpus case. Mirrors AssertCase's role
// but for the metrics shape.
func AssertMetricsCase(tc CorpusCase, body []byte, backendLabel string) ([]DiffReason, error) {
	var reasons []DiffReason
	m, err := decodeMetrics(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", backendLabel, err)
	}
	if tc.ExpectedMinSeries > 0 && len(m.Series) < tc.ExpectedMinSeries {
		reasons = append(reasons, DiffReason{
			Kind:   "assertion",
			Detail: fmt.Sprintf("%s: got %d series, want >= %d", backendLabel, len(m.Series), tc.ExpectedMinSeries),
		})
	}
	if tc.ExpectedMaxSeries > 0 && len(m.Series) > tc.ExpectedMaxSeries {
		reasons = append(reasons, DiffReason{
			Kind:   "assertion",
			Detail: fmt.Sprintf("%s: got %d series, want <= %d", backendLabel, len(m.Series), tc.ExpectedMaxSeries),
		})
	}
	if tc.Endpoint == "metrics_range" && tc.ExpectedSamplesPerSeries > 0 {
		for _, s := range m.Series {
			if len(s.Samples) < tc.ExpectedSamplesPerSeries {
				reasons = append(reasons, DiffReason{
					Kind:   "assertion",
					Detail: fmt.Sprintf("%s: series %s has %d samples, want >= %d", backendLabel, formatLabels(s.Labels), len(s.Samples), tc.ExpectedSamplesPerSeries),
				})
				break
			}
		}
	}
	return reasons, nil
}

// formatLabels renders the label set as a compact, deterministic
// `{k1=v1, k2=v2}` string for use inside DiffReason details. The
// labels are sorted so two different orderings of equal label sets
// render the same way.
func formatLabels(labels []MetricsLabel) string {
	pairs := make([]string, 0, len(labels))
	for _, l := range labels {
		pairs = append(pairs, l.Key+"="+l.Value)
	}
	sort.Strings(pairs)
	return "{" + strings.Join(pairs, ", ") + "}"
}

// SemanticCheckFn is one consistency invariant. Receives one backend's
// parsed metrics body and returns a slice of human-readable failures.
// Empty return = invariant held.
type SemanticCheckFn func(m MetricsResponse, arg string) []string

// SemanticChecks is the registry of named per-backend invariants. The
// corpus case's `-- semantic_checks --` list looks up by name (split on
// the first ":"; the remainder becomes the check's `arg`). An unknown
// name surfaces as an assertion reason so a typo in the corpus shows
// up in the report rather than silently passing.
//
// Today's registry covers the three invariants the plan calls out:
//
//   - samples_non_negative — every sample value >= 0 (rate / count /
//     duration aggregates are non-negative by construction; a negative
//     value indicates either a parser bug or a backend mis-projection).
//
//   - labels_match_query — every returned series carries at least one
//     label (Tempo never returns an empty label set for a successful
//     metrics-pipeline query; an empty label-set series is a sign the
//     handler stripped the group-by columns).
//
//   - groupby_labels_present:<label.key> — when the query has
//     `by (label.key)`, every series must carry that label. The arg
//     names the expected key.
//
//   - sum_eq_count_times_avg — the avg ≡ sum / count invariant lifted
//     from grafana/tempo:integration/api/query_range_test.go::
//     "avg_over_time instant query". Only runnable in a follow-up that
//     orchestrates three queries per case (avg + sum + count); ships
//     today as a placeholder so the plan's invariant set is documented
//     and a follow-up just flips the wiring.
//
//   - exemplar_in_series — for every exemplar, every label key declared
//     on the exemplar must appear on the parent series (Tempo's
//     "Verify that all exemplars in this series belongs to the right
//     series by matching attribute values" invariant).
var SemanticChecks = map[string]SemanticCheckFn{
	"samples_non_negative":   semCheckSamplesNonNegative,
	"labels_match_query":     semCheckLabelsMatchQuery,
	"groupby_labels_present": semCheckGroupByLabelsPresent,
	"exemplar_in_series":     semCheckExemplarInSeries,
	"sum_eq_count_times_avg": semCheckSumEqCountTimesAvgPlaceholder,
}

// RunSemanticChecks executes every named check on `body`. Unknown
// names surface as an explicit assertion so a typo is visible.
func RunSemanticChecks(tc CorpusCase, body []byte, backendLabel string) ([]DiffReason, error) {
	if len(tc.SemanticChecks) == 0 {
		return nil, nil
	}
	m, err := decodeMetrics(body)
	if err != nil {
		return nil, fmt.Errorf("%s semantic decode: %w", backendLabel, err)
	}
	var reasons []DiffReason
	for _, raw := range tc.SemanticChecks {
		name, arg, _ := strings.Cut(raw, ":")
		fn, ok := SemanticChecks[name]
		if !ok {
			reasons = append(reasons, DiffReason{
				Kind:   "assertion",
				Detail: fmt.Sprintf("%s: unknown semantic check %q (registered: %s)", backendLabel, name, registeredSemanticCheckNames()),
			})
			continue
		}
		for _, msg := range fn(m, arg) {
			reasons = append(reasons, DiffReason{
				Kind:   "semantic",
				Detail: fmt.Sprintf("%s [%s]: %s", backendLabel, name, msg),
			})
		}
	}
	return reasons, nil
}

func registeredSemanticCheckNames() string {
	names := make([]string, 0, len(SemanticChecks))
	for n := range SemanticChecks {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func semCheckSamplesNonNegative(m MetricsResponse, _ string) []string {
	var msgs []string
	for _, s := range m.Series {
		for i, smp := range s.Samples {
			if math.IsNaN(smp.Value) {
				continue // NaN is a legitimate "no sample" sentinel in some buckets.
			}
			if smp.Value < 0 {
				msgs = append(msgs, fmt.Sprintf("series %s sample[%d]@%d = %g (< 0)", formatLabels(s.Labels), i, smp.TimestampMs, smp.Value))
				break // one report per series is enough; the differ surfaces the rest.
			}
		}
		if s.Value != nil && *s.Value < 0 && !math.IsNaN(*s.Value) {
			msgs = append(msgs, fmt.Sprintf("series %s instant value = %g (< 0)", formatLabels(s.Labels), *s.Value))
		}
	}
	return msgs
}

func semCheckLabelsMatchQuery(m MetricsResponse, _ string) []string {
	var msgs []string
	for i, s := range m.Series {
		if len(s.Labels) == 0 {
			msgs = append(msgs, fmt.Sprintf("series[%d] has empty label set", i))
		}
	}
	return msgs
}

func semCheckGroupByLabelsPresent(m MetricsResponse, arg string) []string {
	if arg == "" {
		return []string{"groupby_labels_present requires an argument (e.g. groupby_labels_present:resource.service.name)"}
	}
	expectedKeys := strings.Split(arg, ",")
	var msgs []string
	for _, s := range m.Series {
		present := map[string]bool{}
		for _, l := range s.Labels {
			present[l.Key] = true
		}
		for _, want := range expectedKeys {
			want = strings.TrimSpace(want)
			if want == "" {
				continue
			}
			if !present[want] {
				msgs = append(msgs, fmt.Sprintf("series %s missing group-by label %q", formatLabels(s.Labels), want))
				break
			}
		}
	}
	return msgs
}

func semCheckExemplarInSeries(m MetricsResponse, _ string) []string {
	var msgs []string
	for _, s := range m.Series {
		seriesKeys := map[string]string{}
		for _, l := range s.Labels {
			seriesKeys[l.Key] = l.Value
		}
		for i, ex := range s.Exemplars {
			for _, el := range ex.Labels {
				if v, ok := seriesKeys[el.Key]; ok && v != el.Value {
					msgs = append(msgs, fmt.Sprintf("series %s exemplar[%d] label %s=%q disagrees with series value %q", formatLabels(s.Labels), i, el.Key, el.Value, v))
				}
			}
		}
	}
	return msgs
}

// semCheckSumEqCountTimesAvgPlaceholder records that the avg ≡ sum/count
// invariant is part of the registered check set without yet executing
// the three-query orchestration the real check needs. Returns an
// empty slice so the placeholder is a no-op at runtime; a follow-up PR
// that wires per-case multi-query plans replaces this with a real
// implementation.
//
// Keeping the name registered today means a corpus case may list
// `sum_eq_count_times_avg` in `-- semantic_checks --` to declare
// "this is the invariant I want once we can express it" — and the
// differ won't error out on the unknown name. When the real check
// lands, no corpus edit is needed.
func semCheckSumEqCountTimesAvgPlaceholder(_ MetricsResponse, _ string) []string {
	return nil
}
