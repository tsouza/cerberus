package nightly

import (
	"fmt"
	"strings"
	"testing"
)

func TestMapArraysExpr_PlainKeys(t *testing.T) {
	got := mapArraysExpr("Attributes", []string{"app_id", "method"})
	want := "mapFromArrays(['app_id', 'method'], [tupleElement(Attributes, 'app_id'), tupleElement(Attributes, 'method')])"
	if got != want {
		t.Errorf("mapArraysExpr =\n%s\nwant\n%s", got, want)
	}
}

func TestMapArraysExpr_DottedKeys(t *testing.T) {
	got := mapArraysExpr("ResourceAttributes", []string{"service.name", "k8s.namespace.name"})
	want := "mapFromArrays(['service.name', 'k8s.namespace.name'], " +
		"[tupleElement(ResourceAttributes, 'service.name'), tupleElement(ResourceAttributes, 'k8s.namespace.name')])"
	if got != want {
		t.Errorf("mapArraysExpr =\n%s\nwant\n%s", got, want)
	}
}

func TestMapArraysExpr_EmptyKeys(t *testing.T) {
	got := mapArraysExpr("Attributes", nil)
	want := "mapFromArrays([], [])"
	if got != want {
		t.Errorf("mapArraysExpr(no keys) = %q, want %q", got, want)
	}
}

// TestMapArraysExpr_NeverUsesDotAccess pins the SHAPE the #2875 fix turns
// on, over the loader's real key sets rather than a hand-picked pair: no
// rendered field access may be dot access. Dot access is what ClickHouse
// 26.x's file()-reader column pushdown resolves by longest matching column
// prefix, and so what collapses "deployment.environment.name" onto
// "deployment.environment" — see mapArraysExpr's own comment. Reverting to
// `Col.` + "`key`" reddens this without needing an engine.
func TestMapArraysExpr_NeverUsesDotAccess(t *testing.T) {
	sets := map[string][]string{"ResourceAttributes": sampleResourceAttributeKeys}
	for metric, keys := range sampleAttributeKeys {
		sets["Attributes/"+metric] = keys
	}
	for name, keys := range sets {
		column := "ResourceAttributes"
		if strings.HasPrefix(name, "Attributes/") {
			column = "Attributes"
		}
		got := mapArraysExpr(column, keys)
		if strings.Contains(got, column+".") {
			t.Errorf("%s: mapArraysExpr must not render dot access on %s, got: %s", name, column, got)
		}
		for _, k := range keys {
			if !strings.Contains(got, fmt.Sprintf("tupleElement(%s, '%s')", column, k)) {
				t.Errorf("%s: mapArraysExpr must read %q via tupleElement, got: %s", name, k, got)
			}
		}
	}
}

// TestSampleResourceAttributeKeys_KeepPrefixCollision keeps
// TestMapArraysExpr_NeverUsesDotAccess and the chDB pin honest. The whole
// hazard exists only because the captured sample really does carry a key
// that is a dotted prefix of another key; deleting one of the pair would
// make the nightly lane pass while measuring strictly less real production
// shape than it did before — the hollow green invariant 6 forbids. This
// fails the moment such a pair stops being present.
func TestSampleResourceAttributeKeys_KeepPrefixCollision(t *testing.T) {
	var pairs []string
	for _, a := range sampleResourceAttributeKeys {
		for _, b := range sampleResourceAttributeKeys {
			if a != b && strings.HasPrefix(b, a+".") {
				pairs = append(pairs, fmt.Sprintf("%q is a dotted prefix of %q", a, b))
			}
		}
	}
	if len(pairs) == 0 {
		t.Fatalf("sampleResourceAttributeKeys no longer carries a dotted-prefix key pair, so the #2875 "+
			"regression pins cannot fail; the captured sample's real key set is %v", sampleResourceAttributeKeys)
	}
	t.Logf("prefix-colliding key pairs still loaded: %s", strings.Join(pairs, "; "))
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
