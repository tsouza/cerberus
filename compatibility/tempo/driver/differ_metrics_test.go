package main

import (
	"strings"
	"testing"
)

func TestMetricsLabel_UnmarshalFlatString(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[{"labels":[{"key":"k","value":"v"}],"samples":[{"timestampMs":1000,"value":1.5}]}]}`)
	m, err := decodeMetrics(body)
	if err != nil {
		t.Fatalf("decodeMetrics: %v", err)
	}
	if len(m.Series) != 1 || len(m.Series[0].Labels) != 1 {
		t.Fatalf("unexpected shape: %+v", m)
	}
	if m.Series[0].Labels[0].Key != "k" || m.Series[0].Labels[0].Value != "v" {
		t.Fatalf("label = %+v", m.Series[0].Labels[0])
	}
}

func TestMetricsLabel_UnmarshalAnyValueShape(t *testing.T) {
	t.Parallel()
	// Tempo's tempopb wire shape: value is a nested AnyValue object.
	body := []byte(`{"series":[{"labels":[{"key":"k","value":{"stringValue":"v"}}],"samples":[{"timestampMs":1000,"value":1.5}]}]}`)
	m, err := decodeMetrics(body)
	if err != nil {
		t.Fatalf("decodeMetrics: %v", err)
	}
	if m.Series[0].Labels[0].Value != "v" {
		t.Fatalf("AnyValue stringValue not extracted: %+v", m.Series[0].Labels[0])
	}
}

func TestMetricsLabel_UnmarshalEmptyAnyValue(t *testing.T) {
	t.Parallel()
	// Tempo emits empty AnyValue for nil group-by buckets.
	body := []byte(`{"series":[{"labels":[{"key":"k","value":{}}],"samples":[]}]}`)
	m, err := decodeMetrics(body)
	if err != nil {
		t.Fatalf("decodeMetrics: %v", err)
	}
	if m.Series[0].Labels[0].Value != "" {
		t.Fatalf("empty AnyValue should map to empty string, got %q", m.Series[0].Labels[0].Value)
	}
}

// TestMetricsSample_TimestampMs_NumberWire pins the cerberus-side wire
// shape: `timestampMs` is a JSON number. Both endpoints' decoders must
// keep handling it after the FlexInt64 swap.
func TestMetricsSample_TimestampMs_NumberWire(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[{"labels":[{"key":"k","value":"v"}],"samples":[{"timestampMs":1747000000,"value":1.5}]}]}`)
	m, err := decodeMetrics(body)
	if err != nil {
		t.Fatalf("decodeMetrics number-shape timestamp: %v", err)
	}
	if got := m.Series[0].Samples[0].TimestampMs; got != 1747000000 {
		t.Fatalf("TimestampMs = %d, want 1747000000", got)
	}
}

// TestMetricsSample_TimestampMs_StringWire pins the recent Tempo wire
// shape: `timestampMs` is a JSON string holding a base-10 int64. The
// upstream protobuf-to-JSON projection emits int64 fields as strings
// to dodge JS-number precision loss. Before FlexInt64 this body failed
// to decode with `json: cannot unmarshal string into Go struct field
// MetricsSample.series.samples.timestampMs of type int64`, which then
// surfaced in the compatibility report as ERROR for the three metrics
// cases (count_over_time, quantile_over_time, rate_no_groupby).
func TestMetricsSample_TimestampMs_StringWire(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[{"labels":[{"key":"k","value":"v"}],"samples":[{"timestampMs":"1747000000","value":1.5}]}]}`)
	m, err := decodeMetrics(body)
	if err != nil {
		t.Fatalf("decodeMetrics string-shape timestamp: %v", err)
	}
	if got := m.Series[0].Samples[0].TimestampMs; got != 1747000000 {
		t.Fatalf("TimestampMs = %d, want 1747000000", got)
	}
}

// TestMetricsExemplar_TimestampMs_StringWire covers the exemplar path
// because exemplars use the same JSON projection as samples — Tempo
// string-encodes both.
func TestMetricsExemplar_TimestampMs_StringWire(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[{
		"labels":[{"key":"k","value":"v"}],
		"samples":[{"timestampMs":"1000","value":1}],
		"exemplars":[{"timestampMs":"1500","value":1.2,"labels":[{"key":"k","value":"v"}]}]
	}]}`)
	m, err := decodeMetrics(body)
	if err != nil {
		t.Fatalf("decodeMetrics exemplar string-shape timestamp: %v", err)
	}
	if got := m.Series[0].Exemplars[0].TimestampMs; got != 1500 {
		t.Fatalf("Exemplar TimestampMs = %d, want 1500", got)
	}
}

// TestMetricsSample_TimestampMs_Malformed pins the loud-fail behaviour
// on a malformed body so a bad wire shape still produces a decode
// error (rather than silently zero-filling the timestamp).
func TestMetricsSample_TimestampMs_Malformed(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[{"labels":[],"samples":[{"timestampMs":"not-a-number","value":1}]}]}`)
	if _, err := decodeMetrics(body); err == nil {
		t.Fatal("expected decode error for non-integer string timestamp, got nil")
	}
}

// TestCompareMetrics_MixedTimestampWire is the cross-shape parity case
// that motivated the fix: tempo-side string-encoded `timestampMs`,
// cerberus-side number-encoded `timestampMs`. Same numeric value, same
// label set, same float value — the differ must report Equal=true.
func TestCompareMetrics_MixedTimestampWire(t *testing.T) {
	t.Parallel()
	tempoSide := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],
		 "samples":[{"timestampMs":"1747000000","value":1.5}]}
	]}`)
	cerberusSide := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],
		 "samples":[{"timestampMs":1747000000,"value":1.5}]}
	]}`)
	d, err := CompareMetrics(tempoSide, cerberusSide, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected Equal across mixed timestampMs wire shapes, got reasons=%v", d.Reasons)
	}
}

func TestCompareMetrics_Identical(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[
		{"labels":[{"key":"resource.service.name","value":"checkout"}],
		 "samples":[{"timestampMs":1000,"value":1.5},{"timestampMs":2000,"value":2.5}]}
	]}`)
	d, err := CompareMetrics(body, body, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected Equal, got %+v", d.Reasons)
	}
	if d.MatchedCount != 1 {
		t.Fatalf("MatchedCount = %d, want 1", d.MatchedCount)
	}
}

func TestCompareMetrics_CrossShapeEqual(t *testing.T) {
	t.Parallel()
	// Tempo's wire shape vs cerberus's wire shape, otherwise identical
	// label set + samples. The differ should treat them as equal.
	tempoSide := []byte(`{"series":[
		{"labels":[{"key":"resource.service.name","value":{"stringValue":"checkout"}}],
		 "samples":[{"timestampMs":1000,"value":1.5}]}
	]}`)
	cerberusSide := []byte(`{"series":[
		{"labels":[{"key":"resource.service.name","value":"checkout"}],
		 "samples":[{"timestampMs":1000,"value":1.5}]}
	]}`)
	d, err := CompareMetrics(tempoSide, cerberusSide, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected cross-shape match, got reasons=%v", d.Reasons)
	}
}

func TestCompareMetrics_CardinalityMismatch(t *testing.T) {
	t.Parallel()
	a := []byte(`{"series":[
		{"labels":[{"key":"k","value":"a"}],"samples":[]},
		{"labels":[{"key":"k","value":"b"}],"samples":[]}
	]}`)
	b := []byte(`{"series":[
		{"labels":[{"key":"k","value":"a"}],"samples":[]}
	]}`)
	d, err := CompareMetrics(a, b, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if d.Equal {
		t.Fatal("expected Equal=false")
	}
	foundCard, foundMissing := false, false
	for _, r := range d.Reasons {
		if r.Kind == "cardinality" {
			foundCard = true
		}
		if r.Kind == "missing_in_b" {
			foundMissing = true
		}
	}
	if !foundCard || !foundMissing {
		t.Fatalf("expected cardinality + missing_in_b reasons, got %+v", d.Reasons)
	}
}

func TestCompareMetrics_SampleEpsilon(t *testing.T) {
	t.Parallel()
	a := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"samples":[{"timestampMs":1000,"value":1.5}]}
	]}`)
	b := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"samples":[{"timestampMs":1000,"value":1.5000000000001}]}
	]}`)
	d, err := CompareMetrics(a, b, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected epsilon-absorbed match, got reasons=%v", d.Reasons)
	}
}

func TestCompareMetrics_SampleValueDiff(t *testing.T) {
	t.Parallel()
	a := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"samples":[{"timestampMs":1000,"value":1.5}]}
	]}`)
	b := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"samples":[{"timestampMs":1000,"value":2.5}]}
	]}`)
	d, err := CompareMetrics(a, b, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if d.Equal {
		t.Fatal("expected Equal=false")
	}
	found := false
	for _, r := range d.Reasons {
		if r.Kind == "field_mismatch" && strings.Contains(r.Detail, "sample[0]") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected sample mismatch reason, got %+v", d.Reasons)
	}
}

func TestCompareMetrics_InstantValue(t *testing.T) {
	t.Parallel()
	a := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"value":3.14}
	]}`)
	b := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"value":3.14}
	]}`)
	d, err := CompareMetrics(a, b, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected equal instant series, got reasons=%v", d.Reasons)
	}
}

func TestCompareMetrics_InstantValueOneSideMissing(t *testing.T) {
	t.Parallel()
	a := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"value":3.14}
	]}`)
	b := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}]}
	]}`)
	d, err := CompareMetrics(a, b, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if d.Equal {
		t.Fatal("expected Equal=false (one side missing instant value)")
	}
}

func TestCompareMetrics_LabelOrderInvariant(t *testing.T) {
	t.Parallel()
	// Different label orderings of the same set must canonicalise to
	// the same key — Tempo and cerberus may emit labels in different
	// stable orders.
	a := []byte(`{"series":[
		{"labels":[{"key":"a","value":"1"},{"key":"b","value":"2"}],"samples":[]}
	]}`)
	b := []byte(`{"series":[
		{"labels":[{"key":"b","value":"2"},{"key":"a","value":"1"}],"samples":[]}
	]}`)
	d, err := CompareMetrics(a, b, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected label-order invariance, got reasons=%v", d.Reasons)
	}
}

func TestCompareMetrics_ExemplarCountInformational(t *testing.T) {
	t.Parallel()
	// Exemplar count divergence is reported but does NOT drive
	// Equal=false (see CompareMetrics doc-comment).
	a := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"samples":[{"timestampMs":1000,"value":1}],
		 "exemplars":[{"timestampMs":1500,"value":1.2,"labels":[{"key":"k","value":"v"}]}]}
	]}`)
	b := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"samples":[{"timestampMs":1000,"value":1}]}
	]}`)
	d, err := CompareMetrics(a, b, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	// Equal must remain true — exemplar count divergence is informational.
	if !d.Equal {
		t.Fatalf("exemplar count divergence should not drive Equal=false, got %+v", d.Reasons)
	}
	foundExemplar := false
	for _, r := range d.Reasons {
		if r.Kind == "exemplar_count" {
			foundExemplar = true
		}
	}
	if !foundExemplar {
		t.Fatalf("expected exemplar_count reason as informational signal, got %+v", d.Reasons)
	}
}

func TestAssertMetricsCase_MinSeries(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "m", Endpoint: "metrics_range", ExpectedMinSeries: 3}
	body := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"samples":[]}
	]}`)
	reasons, err := AssertMetricsCase(tc, body, "tempo")
	if err != nil {
		t.Fatalf("AssertMetricsCase: %v", err)
	}
	if len(reasons) == 0 || !strings.Contains(reasons[0].Detail, "want >= 3") {
		t.Fatalf("expected min-series reason, got %+v", reasons)
	}
}

func TestAssertMetricsCase_SamplesPerSeries(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "m", Endpoint: "metrics_range", ExpectedSamplesPerSeries: 3}
	body := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"samples":[{"timestampMs":1000,"value":1}]}
	]}`)
	reasons, err := AssertMetricsCase(tc, body, "tempo")
	if err != nil {
		t.Fatalf("AssertMetricsCase: %v", err)
	}
	if len(reasons) == 0 || !strings.Contains(reasons[0].Detail, "want >= 3") {
		t.Fatalf("expected samples-per-series reason, got %+v", reasons)
	}
}

func TestSemanticChecks_SamplesNonNegative_OK(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"samples":[{"timestampMs":1000,"value":1},{"timestampMs":2000,"value":2}]}
	]}`)
	tc := CorpusCase{Name: "n", Endpoint: "metrics_range", SemanticChecks: []string{"samples_non_negative"}}
	reasons, err := RunSemanticChecks(tc, body, "tempo")
	if err != nil {
		t.Fatalf("RunSemanticChecks: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %+v", reasons)
	}
}

func TestSemanticChecks_SamplesNonNegative_Violation(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[
		{"labels":[{"key":"k","value":"v"}],"samples":[{"timestampMs":1000,"value":-1.5}]}
	]}`)
	tc := CorpusCase{Name: "n", Endpoint: "metrics_range", SemanticChecks: []string{"samples_non_negative"}}
	reasons, err := RunSemanticChecks(tc, body, "tempo")
	if err != nil {
		t.Fatalf("RunSemanticChecks: %v", err)
	}
	if len(reasons) == 0 {
		t.Fatal("expected violation reason for negative sample")
	}
	if reasons[0].Kind != "semantic" {
		t.Fatalf("expected Kind=semantic, got %q", reasons[0].Kind)
	}
}

func TestSemanticChecks_GroupByLabelsPresent_OK(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[
		{"labels":[{"key":"resource.service.name","value":"checkout"}],"samples":[]}
	]}`)
	tc := CorpusCase{
		Name:           "n",
		Endpoint:       "metrics_range",
		SemanticChecks: []string{"groupby_labels_present:resource.service.name"},
	}
	reasons, err := RunSemanticChecks(tc, body, "tempo")
	if err != nil {
		t.Fatalf("RunSemanticChecks: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %+v", reasons)
	}
}

func TestSemanticChecks_GroupByLabelsPresent_Missing(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[
		{"labels":[{"key":"other","value":"x"}],"samples":[]}
	]}`)
	tc := CorpusCase{
		Name:           "n",
		Endpoint:       "metrics_range",
		SemanticChecks: []string{"groupby_labels_present:resource.service.name"},
	}
	reasons, err := RunSemanticChecks(tc, body, "tempo")
	if err != nil {
		t.Fatalf("RunSemanticChecks: %v", err)
	}
	if len(reasons) == 0 {
		t.Fatal("expected missing group-by label reason")
	}
}

func TestSemanticChecks_UnknownCheckSurfaces(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[{"labels":[{"key":"k","value":"v"}],"samples":[]}]}`)
	tc := CorpusCase{Name: "n", Endpoint: "metrics_range", SemanticChecks: []string{"nope_not_a_real_check"}}
	reasons, err := RunSemanticChecks(tc, body, "tempo")
	if err != nil {
		t.Fatalf("RunSemanticChecks: %v", err)
	}
	if len(reasons) == 0 || !strings.Contains(reasons[0].Detail, "unknown semantic check") {
		t.Fatalf("expected unknown-check reason, got %+v", reasons)
	}
}

func TestSemanticChecks_LabelsMatchQuery(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[
		{"labels":[],"samples":[]}
	]}`)
	tc := CorpusCase{Name: "n", Endpoint: "metrics_range", SemanticChecks: []string{"labels_match_query"}}
	reasons, err := RunSemanticChecks(tc, body, "tempo")
	if err != nil {
		t.Fatalf("RunSemanticChecks: %v", err)
	}
	if len(reasons) == 0 {
		t.Fatal("expected empty-label-set reason")
	}
}

func TestSemanticChecks_ExemplarInSeries_OK(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[
		{"labels":[{"key":"resource.service.name","value":"checkout"}],
		 "samples":[{"timestampMs":1000,"value":1}],
		 "exemplars":[{"timestampMs":1500,"value":1.2,"labels":[{"key":"resource.service.name","value":"checkout"}]}]}
	]}`)
	tc := CorpusCase{Name: "n", Endpoint: "metrics_range", SemanticChecks: []string{"exemplar_in_series"}}
	reasons, err := RunSemanticChecks(tc, body, "tempo")
	if err != nil {
		t.Fatalf("RunSemanticChecks: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons (exemplar agrees with series), got %+v", reasons)
	}
}

func TestSemanticChecks_ExemplarInSeries_Mismatch(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[
		{"labels":[{"key":"resource.service.name","value":"checkout"}],
		 "samples":[{"timestampMs":1000,"value":1}],
		 "exemplars":[{"timestampMs":1500,"value":1.2,"labels":[{"key":"resource.service.name","value":"payments"}]}]}
	]}`)
	tc := CorpusCase{Name: "n", Endpoint: "metrics_range", SemanticChecks: []string{"exemplar_in_series"}}
	reasons, err := RunSemanticChecks(tc, body, "tempo")
	if err != nil {
		t.Fatalf("RunSemanticChecks: %v", err)
	}
	if len(reasons) == 0 {
		t.Fatal("expected exemplar-in-series mismatch reason")
	}
}

func TestSemanticChecks_NoChecksReturnsNothing(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[]}`)
	tc := CorpusCase{Name: "n", Endpoint: "metrics_range"}
	reasons, err := RunSemanticChecks(tc, body, "tempo")
	if err != nil {
		t.Fatalf("RunSemanticChecks: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons when SemanticChecks empty, got %+v", reasons)
	}
}

func TestSemanticChecks_SumEqCountTimesAvgPlaceholder(t *testing.T) {
	t.Parallel()
	body := []byte(`{"series":[]}`)
	tc := CorpusCase{Name: "n", Endpoint: "metrics_range", SemanticChecks: []string{"sum_eq_count_times_avg"}}
	reasons, err := RunSemanticChecks(tc, body, "tempo")
	if err != nil {
		t.Fatalf("RunSemanticChecks: %v", err)
	}
	// Placeholder must register as known + return zero reasons today.
	if len(reasons) != 0 {
		t.Fatalf("expected placeholder to no-op today, got %+v", reasons)
	}
}

func TestMetricsCanonicalKey_LabelOrderInvariant(t *testing.T) {
	t.Parallel()
	s1 := MetricsSeriesEntry{Labels: []MetricsLabel{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}}}
	s2 := MetricsSeriesEntry{Labels: []MetricsLabel{{Key: "b", Value: "2"}, {Key: "a", Value: "1"}}}
	if metricsCanonicalKey(s1) != metricsCanonicalKey(s2) {
		t.Fatal("label order should not affect canonical key")
	}
}

func TestMetricsCanonicalKey_DistinguishesValues(t *testing.T) {
	t.Parallel()
	s1 := MetricsSeriesEntry{Labels: []MetricsLabel{{Key: "k", Value: "v1"}}}
	s2 := MetricsSeriesEntry{Labels: []MetricsLabel{{Key: "k", Value: "v2"}}}
	if metricsCanonicalKey(s1) == metricsCanonicalKey(s2) {
		t.Fatal("different label values must produce different canonical keys")
	}
}
