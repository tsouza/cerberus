package nightly

import (
	"fmt"
	"strings"
	"testing"
)

func TestMapArraysExpr_PlainKeys(t *testing.T) {
	got := mapArraysExpr("Attributes", []string{"app_id", "method"})
	want := "mapFromArrays(['app_id', 'method'], [Attributes.`app_id`, Attributes.`method`])"
	if got != want {
		t.Errorf("mapArraysExpr =\n%s\nwant\n%s", got, want)
	}
}

func TestMapArraysExpr_DottedKeys(t *testing.T) {
	got := mapArraysExpr("ResourceAttributes", []string{"service.name", "k8s.namespace.name"})
	// Dotted field names need backtick-quoted dot access — ClickHouse would
	// otherwise try to parse the dot as nested member access on an
	// intermediate identifier that doesn't exist.
	if !strings.Contains(got, "ResourceAttributes.`service.name`") {
		t.Errorf("mapArraysExpr must backtick-quote dotted keys, got: %s", got)
	}
	if !strings.Contains(got, "ResourceAttributes.`k8s.namespace.name`") {
		t.Errorf("mapArraysExpr must backtick-quote dotted keys, got: %s", got)
	}
}

func TestMapArraysExpr_EmptyKeys(t *testing.T) {
	got := mapArraysExpr("Attributes", nil)
	want := "mapFromArrays([], [])"
	if got != want {
		t.Errorf("mapArraysExpr(no keys) = %q, want %q", got, want)
	}
}

func TestSampleAttributeKeys_CoverEveryMetric(t *testing.T) {
	for _, metric := range []string{histogramMetric, sumMetric, gaugeMetric} {
		keys, ok := sampleAttributeKeys[metric]
		if !ok || len(keys) == 0 {
			t.Errorf("sampleAttributeKeys has no entry for metric %q", metric)
		}
	}
}

func TestSampleResourceAttributeKeys_NonEmpty(t *testing.T) {
	if len(sampleResourceAttributeKeys) == 0 {
		t.Fatal("sampleResourceAttributeKeys is empty")
	}
}

// TestNativeHistogramDerivationSQL_CarriesExpectedLiterals pins the
// generated INSERT's load-bearing literals (see nativeHistogramDerivationSQL's
// own doc comment for the per-alias walkthrough this string encodes): the
// derived rows land under nativeHistogramMetric, are sourced only from
// histogramMetric's real classic rows, and re-bucket at
// nativeHistogramDerivedScale via nativeHistogramBucketsPerOctave's index
// multiplier. A future edit that silently drops one of these — e.g. reusing
// the classic MetricName instead of renaming it, which would make the
// derived rows invisible to nativeHistogramMetric's own sentinel query and
// instead corrupt the classic sentinel's own data — fails here rather than
// only showing up as an empty result in the real-ClickHouse integration
// lane.
func TestNativeHistogramDerivationSQL_CarriesExpectedLiterals(t *testing.T) {
	sql := nativeHistogramDerivationSQL()

	mustContain := []string{
		"INSERT INTO otel_metrics_exponential_histogram",
		"'" + nativeHistogramMetric + "' AS MetricName",
		"WHERE MetricName = '" + histogramMetric + "'",
	}
	for _, want := range mustContain {
		if !strings.Contains(sql, want) {
			t.Errorf("nativeHistogramDerivationSQL() missing %q:\n%s", want, sql)
		}
	}

	// The index-multiplier literal (nativeHistogramBucketsPerOctave) must be
	// the SAME direction internal/chsql/histogram_quantile_native.go's own
	// reader inverts (base = pow(2, pow(2, -Scale)); this derivation applies
	// index = ceil(log2(value) * 2^Scale)) — a drifted constant here would
	// silently re-bucket at the wrong resolution without ever failing an
	// assertion downstream, since histogram_quantile just answers a
	// different (still plausible-looking) number.
	wantMultiplier := fmt.Sprintf("log2(r) * %d", nativeHistogramBucketsPerOctave)
	if !strings.Contains(sql, wantMultiplier) {
		t.Errorf("nativeHistogramDerivationSQL() missing index multiplier %q:\n%s", wantMultiplier, sql)
	}

	wantScale := fmt.Sprintf("%d AS Scale", nativeHistogramDerivedScale)
	if !strings.Contains(sql, wantScale) {
		t.Errorf("nativeHistogramDerivationSQL() missing %q:\n%s", wantScale, sql)
	}
}

// TestNativeHistogramMetric_HasRoutingSuffix pins the "_exp_hist" suffix
// schema.Metrics.RouteHistogramToNative (default ExpHistogramSuffix) keys
// its classic-vs-native routing decision on — without it, this sentinel's
// histogram_quantile query would silently fall through to the classic-bucket
// lowering path instead of exercising the native one it exists to stress.
func TestNativeHistogramMetric_HasRoutingSuffix(t *testing.T) {
	const suffix = "_exp_hist"
	if !strings.HasSuffix(nativeHistogramMetric, suffix) {
		t.Fatalf("nativeHistogramMetric %q does not end in %q, the native-histogram routing suffix", nativeHistogramMetric, suffix)
	}
}
