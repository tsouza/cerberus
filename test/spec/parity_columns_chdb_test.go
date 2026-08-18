//go:build chdb

package spec

import (
	"strings"
	"testing"
	"time"

	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

// TestLocateSampleColumns pins the projection layouts the promql corpus
// actually emits, and the ones the parity layer must refuse.
//
// The accepted rows are not invented: they are every distinct column list
// the corpus's own `sql:` sections produce, read off the driver. The
// refused row is the metadata catalog projection, which carries a single
// `value` column that is a LABEL VALUE rather than a sample — the case
// that makes name-resolution necessary, since no positional rule
// distinguishes it from a one-column sample projection.
func TestLocateSampleColumns(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cols []string
		want sampleColumns
	}{
		{
			name: "canonical Sample projection",
			cols: []string{"MetricName", "Attributes", "TimeUnix", "Value"},
			want: sampleColumns{name: 0, attrs: 1, ts: 2, value: 3, mixedIsHistogram: -1},
		},
		{
			// An instant aggregation drops __name__ and has no sample
			// time to report, so it projects only what its answer has.
			name: "instant aggregation projection",
			cols: []string{"Attributes", "Value"},
			want: sampleColumns{name: -1, attrs: 0, ts: -1, value: 1, mixedIsHistogram: -1},
		},
		{
			// anchor_ts is scaffolding of the emitted subquery, not a
			// field of the answer, and must not be mistaken for the
			// sample time sitting behind it.
			name: "subquery projection interposing anchor_ts",
			cols: []string{"Attributes", "anchor_ts", "TimeUnix", "Value"},
			want: sampleColumns{name: -1, attrs: 0, ts: 2, value: 3, mixedIsHistogram: -1},
		},
		{
			// A raw range projection is wrapped into the public Sample
			// shape by renaming anchor_ts to TimeUnix in the HTTP layer.
			name: "range projection whose sample time is anchor_ts",
			cols: []string{"Attributes", "anchor_ts", "Value"},
			want: sampleColumns{name: -1, attrs: 0, ts: 1, value: 2, mixedIsHistogram: -1},
		},
		{
			name: "named subquery projection interposing anchor_ts",
			cols: []string{"MetricName", "Attributes", "anchor_ts", "TimeUnix", "Value"},
			want: sampleColumns{name: 0, attrs: 1, ts: 3, value: 4, mixedIsHistogram: -1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := locateSampleColumns(tc.cols)
			if err != nil {
				t.Fatalf("locateSampleColumns(%v): %v", tc.cols, err)
			}
			if got != tc.want {
				t.Errorf("locateSampleColumns(%v) = %+v, want %+v", tc.cols, got, tc.want)
			}
			if arity, min := got.arity(), len(tc.cols); arity > min {
				t.Errorf("arity() = %d, past the end of a %d-column row", arity, min)
			}
		})
	}
}

// TestLocateSampleColumnsRejectsNonSampleProjection asserts the refusal is
// LOUD. A projection that carries no label set or no value cannot be
// parity-checked, and returning a zero-valued location instead of an error
// would compare a fabricated empty answer and manufacture a green.
func TestLocateSampleColumnsRejectsNonSampleProjection(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cols []string
	}{
		{name: "metadata catalog label values", cols: []string{"value"}},
		{name: "no value column", cols: []string{"MetricName", "Attributes", "TimeUnix"}},
		{name: "no attributes column", cols: []string{"MetricName", "TimeUnix", "Value"}},
		{name: "empty projection", cols: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := locateSampleColumns(tc.cols)
			if err == nil {
				t.Fatalf("locateSampleColumns(%v) accepted a non-Sample projection", tc.cols)
			}
			// The message must name the projection it refused, so a
			// fixture author reads which columns were actually seen
			// rather than only that something was wrong.
			for _, col := range tc.cols {
				if !strings.Contains(err.Error(), col) {
					t.Errorf("error %q does not name the refused column %q", err, col)
				}
			}
		})
	}
}

// TestLabelsFromSeededRow pins the label set the translator reconstructs
// from one seeded row, because that reconstruction is what decides which
// series the two engines' answers are aligned on. A row whose labels are
// built differently on the two sides produces a "different series"
// failure that says nothing about the query.
func TestLabelsFromSeededRow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		attrs      string
		resAttrs   string
		service    string
		metric     string
		allow      resourceAllowlist
		want       map[string]string
		wantAbsent []string
	}{
		{
			name:   "attributes and metric name only",
			attrs:  `{"job":"api"}`,
			metric: "up",
			want:   map[string]string{"job": "api", "__name__": "up"},
		},
		{
			name:   "metric name is Prometheus normalized",
			attrs:  `{"job":"api"}`,
			metric: "container.cpu.usage",
			want:   map[string]string{"job": "api", "__name__": "container_cpu_usage"},
		},
		{
			// The ServiceName column is where the exporter puts
			// service.name, and it is the label cerberus exposes it as.
			name:    "service name comes from its dedicated column",
			attrs:   `{"job":"api"}`,
			service: "checkout",
			metric:  "up",
			want: map[string]string{
				"job": "api", "service_name": "checkout", "__name__": "up",
			},
		},
		{
			// Dotted OTel keys are addressable underscored.
			name:     "resource keys are sanitized",
			attrs:    `{}`,
			resAttrs: `{"k8s.namespace.name":"prod","deployment/env":"eu"}`,
			metric:   "up",
			want: map[string]string{
				"k8s_namespace_name": "prod", "deployment_env": "eu", "__name__": "up",
			},
		},
		{
			// A metric attribute wins over a resource attribute of the
			// same sanitized name.
			name:     "metric attributes win a collision with resource attributes",
			attrs:    `{"region":"metric"}`,
			resAttrs: `{"region":"resource"}`,
			metric:   "up",
			want:     map[string]string{"region": "metric", "__name__": "up"},
		},
		{
			// service.name is carried by ServiceName, never duplicated
			// out of the resource map.
			name:     "service name in the resource map does not shadow the column",
			attrs:    `{}`,
			resAttrs: `{"service.name":"from-map"}`,
			service:  "from-column",
			metric:   "up",
			want:     map[string]string{"service_name": "from-column", "__name__": "up"},
		},
		{
			name:       "an allowlist narrows which resource keys are promoted",
			attrs:      `{}`,
			resAttrs:   `{"kept":"yes","dropped":"no"}`,
			metric:     "up",
			allow:      resourceAllowlist{"kept": true},
			want:       map[string]string{"kept": "yes", "__name__": "up"},
			wantAbsent: []string{"dropped"},
		},
		{
			// A derived sample carries no metric name, and Prometheus
			// represents that as __name__ being ABSENT.
			name:       "an empty metric name is absent rather than empty",
			attrs:      `{"job":"api"}`,
			want:       map[string]string{"job": "api"},
			wantAbsent: []string{"__name__"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := labelsFromSeededRow(tc.metric, tc.attrs, tc.resAttrs, tc.service, tc.allow)
			if err != nil {
				t.Fatalf("labelsFromSeededRow: %v", err)
			}
			if labelKey(got) != labelKey(tc.want) {
				t.Errorf("labels = %v, want %v", got, tc.want)
			}
			for _, k := range tc.wantAbsent {
				if _, present := got[k]; present {
					t.Errorf("label %q is present as %q, want absent", k, got[k])
				}
			}
		})
	}
}

func TestNormalizeMetricName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "already Prometheus", in: "up:total_1", want: "up:total_1"},
		{name: "dotted OTel", in: "container.cpu.usage", want: "container_cpu_usage"},
		{name: "leading digit", in: "9.cpu", want: "_9_cpu"},
		{name: "multibyte bytes", in: "服务名", want: "_________"},
		{name: "empty", in: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeMetricName(tc.in); got != tc.want {
				t.Errorf("normalizeMetricName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestResourceAttributesProjectionFallsBackWhenUnseeded asserts the reader
// tolerates the corpus's minimal seeds. Most fixtures declare only the
// columns their own query needs, so demanding ResourceAttributes and
// ServiceName would fail to read the majority of the corpus.
func TestResourceAttributesProjectionFallsBackWhenUnseeded(t *testing.T) {
	t.Parallel()

	declared := []string{"MetricName", "Attributes", "ResourceAttributes", "ServiceName", "Value"}
	if got := resourceAttributesProjection(declared); got != "toJSONString(ResourceAttributes)" {
		t.Errorf("declared ResourceAttributes projected as %q", got)
	}
	if got := serviceNameProjection(declared); got != "toString(ServiceName)" {
		t.Errorf("declared ServiceName projected as %q", got)
	}

	minimal := []string{"MetricName", "Attributes", "TimeUnix", "Value"}
	if got := resourceAttributesProjection(minimal); got != "'{}'" {
		t.Errorf("unseeded ResourceAttributes projected as %q, want the empty-map literal", got)
	}
	if got := serviceNameProjection(minimal); got != "''" {
		t.Errorf("unseeded ServiceName projected as %q, want the empty-string literal", got)
	}
}

// TestReadSeededClassicHistograms drives the classic-histogram reader
// against a real seeded session, because the translation it performs is
// the whole reason those fixtures can be parity-checked: OTel stores
// per-bucket counts, Prometheus's `le` series are cumulative, and the
// overflow bucket becomes `le="+Inf"`. Getting any of that wrong produces
// a reference answer that disagrees with cerberus for a reason that is
// the translator's fault rather than the lowering's.
func TestReadSeededClassicHistograms(t *testing.T) {
	const declaredCountSeed = `CREATE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_histogram VALUES
	    ('lat', map('service', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 19, 10.0, [1, 2, 3, 4, 5, 6], [1.0, 2.0, 3.0, 4.0, 5.0]);`
	const omittedCountSeed = `CREATE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_histogram VALUES
    ('lat', map('service', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 10.0, [1, 2, 3, 4, 5, 6], [1.0, 2.0, 3.0, 4.0, 5.0]);`

	for _, tc := range []struct {
		name  string
		seed  string
		count float64
	}{
		{name: "declared count is preserved", seed: declaredCountSeed, count: 19},
		{name: "omitted count is derived from buckets", seed: omittedCountSeed, count: 21},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdbEngineMu.Lock()
			defer chdbEngineMu.Unlock()

			db := OpenChDB(t)
			ApplySeed(t, db, tc.seed)

			byKey := map[string]*oracle.Series{}
			nameRestorer := newMetricNameRestorer()
			if err := readSeededClassicHistograms(
				db, &RoundTripSections{Seed: tc.seed}, nil, byKey, &nameRestorer, map[string]bool{},
			); err != nil {
				t.Fatalf("readSeededClassicHistograms: %v", err)
			}

			// Per-bucket counts [1 2 3 4 5 6] accumulate to [1 3 6 10 15].
			want := map[string]float64{
				`__name__=lat_bucket,le=1,service=api`:    1,
				`__name__=lat_bucket,le=2,service=api`:    3,
				`__name__=lat_bucket,le=3,service=api`:    6,
				`__name__=lat_bucket,le=4,service=api`:    10,
				`__name__=lat_bucket,le=5,service=api`:    15,
				`__name__=lat_bucket,le=+Inf,service=api`: tc.count,
				`__name__=lat_count,service=api`:          tc.count,
				`__name__=lat_sum,service=api`:            10,
			}
			if len(byKey) != len(want) {
				t.Fatalf("read %d series, want %d: %v", len(byKey), len(want), byKey)
			}
			for key, wantValue := range want {
				s, ok := byKey[key]
				if !ok {
					t.Errorf("series %s was not reconstructed", key)
					continue
				}
				if len(s.Points) != 1 {
					t.Errorf("series %s has %d points, want 1", key, len(s.Points))
					continue
				}
				if !oracle.EqualValues(s.Points[0].Value, wantValue) {
					t.Errorf("series %s = %v, want %v", key, s.Points[0].Value, wantValue)
				}
			}
		})
	}
}

func TestMetricNameRestorer(t *testing.T) {
	t.Parallel()

	t.Run("restores a unique name after labels transform", func(t *testing.T) {
		restorer := newMetricNameRestorer()
		input := map[string]string{promNameLabel: "container_cpu_usage", "job": "api"}
		if err := restorer.record(input, "container.cpu.usage"); err != nil {
			t.Fatalf("record: %v", err)
		}
		results := []oracle.Result{{Labels: map[string]string{promNameLabel: "container_cpu_usage", "service": "api"}}}
		restorer.restore(results)
		if got := results[0].Labels[promNameLabel]; got != "container.cpu.usage" {
			t.Errorf("restored name = %q, want raw stored spelling", got)
		}
	})

	t.Run("does not guess an ambiguous normalized name", func(t *testing.T) {
		restorer := newMetricNameRestorer()
		for labels, name := range map[string]string{
			"job=api": "container.cpu.usage",
			"job=web": "container/cpu/usage",
		} {
			labelSet := map[string]string{promNameLabel: "container_cpu_usage"}
			for _, pair := range strings.Split(labels, ",") {
				key, value, _ := strings.Cut(pair, "=")
				labelSet[key] = value
			}
			if err := restorer.record(labelSet, name); err != nil {
				t.Fatalf("record %q: %v", name, err)
			}
		}
		results := []oracle.Result{{Labels: map[string]string{promNameLabel: "container_cpu_usage", "service": "api"}}}
		restorer.restore(results)
		if got := results[0].Labels[promNameLabel]; got != "container_cpu_usage" {
			t.Errorf("ambiguous name restored as %q, want normalized spelling left intact", got)
		}
	})

	t.Run("rejects a collision on one Prometheus series", func(t *testing.T) {
		restorer := newMetricNameRestorer()
		labels := map[string]string{promNameLabel: "container_cpu_usage", "job": "api"}
		if err := restorer.record(labels, "container.cpu.usage"); err != nil {
			t.Fatalf("record first spelling: %v", err)
		}
		if err := restorer.record(labels, "container/cpu/usage"); err == nil {
			t.Error("record accepted two stored spellings for one Prometheus series")
		}
	})
}

func TestReadSeededNativeHistogramCompanions(t *testing.T) {
	const (
		firstTimestamp  = "2026-01-01 00:00:00"
		secondTimestamp = "2026-01-01 00:01:00"
	)
	firstMillis := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	secondMillis := time.Date(2026, time.January, 1, 0, 1, 0, 0, time.UTC).UnixMilli()

	for _, tc := range []struct {
		name string
		seed string
		want map[string][]oracle.Point
	}{
		{
			name: "declared columns group companion points by normalized name",
			seed: `CREATE TABLE otel_metrics_exponential_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    Scale Int32,
    ZeroCount UInt64,
    PositiveOffset Int32,
    PositiveBucketCounts Array(UInt64),
    NegativeOffset Int32,
    NegativeBucketCounts Array(UInt64)
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_exponential_histogram VALUES
				('latency.exp', map('service', 'api'), toDateTime64('` + firstTimestamp + `', 9), 7, 21.0, 0, 0, 0, [7], 0, []),
				('latency.exp', map('service', 'api'), toDateTime64('` + secondTimestamp + `', 9), 9, 34.0, 0, 0, 0, [9], 0, []);`,
			want: map[string][]oracle.Point{
				"__name__=latency_exp_count,service=api": {
					{TMillis: firstMillis, Value: 7}, {TMillis: secondMillis, Value: 9},
				},
				"__name__=latency_exp_sum,service=api": {
					{TMillis: firstMillis, Value: 21}, {TMillis: secondMillis, Value: 34},
				},
			},
		},
		{
			name: "absent sum produces only count companion",
			seed: `CREATE TABLE otel_metrics_exponential_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Scale Int32,
    ZeroCount UInt64,
    PositiveOffset Int32,
    PositiveBucketCounts Array(UInt64),
    NegativeOffset Int32,
    NegativeBucketCounts Array(UInt64)
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('latency.exp', map('service', 'api'), toDateTime64('` + firstTimestamp + `', 9), 7, 0, 0, 0, [7], 0, []);`,
			want: map[string][]oracle.Point{
				"__name__=latency_exp_count,service=api": {{TMillis: firstMillis, Value: 7}},
			},
		},
		{
			name: "absent count produces only sum companion",
			seed: `CREATE TABLE otel_metrics_exponential_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Sum Float64,
    Scale Int32,
    ZeroCount UInt64,
    PositiveOffset Int32,
    PositiveBucketCounts Array(UInt64),
    NegativeOffset Int32,
    NegativeBucketCounts Array(UInt64)
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('latency.exp', map('service', 'api'), toDateTime64('` + firstTimestamp + `', 9), 21.0, 0, 0, 0, [7], 0, []);`,
			want: map[string][]oracle.Point{
				"__name__=latency_exp_sum,service=api": {{TMillis: firstMillis, Value: 21}},
			},
		},
		{
			name: "absent count and sum produce no companions",
			seed: `CREATE TABLE otel_metrics_exponential_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Scale Int32,
    ZeroCount UInt64,
    PositiveOffset Int32,
    PositiveBucketCounts Array(UInt64),
    NegativeOffset Int32,
    NegativeBucketCounts Array(UInt64)
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('latency.exp', map('service', 'api'), toDateTime64('` + firstTimestamp + `', 9), 0, 0, 0, [7], 0, []);`,
			want: map[string][]oracle.Point{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdbEngineMu.Lock()
			defer chdbEngineMu.Unlock()

			db := OpenChDB(t)
			ApplySeed(t, db, tc.seed)

			byKey := map[string]*oracle.Series{}
			nameRestorer := newMetricNameRestorer()
			if err := readSeededNativeHistograms(
				db, &RoundTripSections{Seed: tc.seed}, nil, byKey, &nameRestorer, nil, map[string]bool{},
			); err != nil {
				t.Fatalf("readSeededNativeHistograms: %v", err)
			}

			if _, ok := byKey["__name__=latency_exp,service=api"]; !ok {
				t.Error("native histogram series was not reconstructed with its normalized metric name")
			}
			for key, want := range tc.want {
				s, ok := byKey[key]
				if !ok {
					t.Errorf("companion series %s was not reconstructed", key)
					continue
				}
				if len(s.Points) != len(want) {
					t.Errorf("companion series %s has %d points, want %d", key, len(s.Points), len(want))
					continue
				}
				for i, point := range s.Points {
					if point.TMillis != want[i].TMillis || !oracle.EqualValues(point.Value, want[i].Value) {
						t.Errorf("companion series %s point %d = %+v, want %+v", key, i, point, want[i])
					}
				}
			}
			if got, want := len(byKey), len(tc.want)+1; got != want {
				t.Errorf("read %d series, want %d: %v", got, want, byKey)
			}
		})
	}
}

// TestFormatBucketBound pins the `le` spelling against ClickHouse's
// toString(Float64), the expression cerberus's own emitter uses. A
// disagreement here does not produce a wrong value — it produces two
// bucket series with different label sets that never align at all.
func TestFormatBucketBound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		bound float64
		want  string
	}{
		{bound: 1, want: "1"},
		{bound: 2.5, want: "2.5"},
		{bound: 0.05, want: "0.05"},
		{bound: 0, want: "0"},
		{bound: -10, want: "-10"},
		{bound: 300, want: "300"},
	} {
		if got := formatBucketBound(tc.bound); got != tc.want {
			t.Errorf("formatBucketBound(%v) = %q, want %q", tc.bound, got, tc.want)
		}
	}
}

func TestCumulativeDeltaSeries(t *testing.T) {
	t.Parallel()

	const (
		scalarKey  = "__name__=requests_total"
		classicKey = "__name__=latency_bucket,le=+Inf"
		nativeKey  = "__name__=latency_exp"
	)
	series := map[string]*oracle.Series{
		scalarKey: {
			Points: []oracle.Point{{TMillis: 1, Value: 10}, {TMillis: 2, Value: 40}},
		},
		classicKey: {
			Points: []oracle.Point{{TMillis: 1, Value: 2}, {TMillis: 2, Value: 4}},
		},
		nativeKey: {
			Points: []oracle.Point{
				{TMillis: 1, Histogram: &oracle.Histogram{
					Count: 6, Sum: 3, Scale: 0, ZeroCount: 1,
					PositiveOffset: 0, PositiveBuckets: []float64{1}, NegativeOffset: -1, NegativeBuckets: []float64{4},
				}},
				{TMillis: 2, Histogram: &oracle.Histogram{
					Count: 12, Sum: 6, Scale: 1, ZeroCount: 2,
					PositiveOffset: 1, PositiveBuckets: []float64{2}, NegativeOffset: -1, NegativeBuckets: []float64{8},
				}},
			},
		},
		"__name__=cumulative_total": {
			Points: []oracle.Point{{TMillis: 1, Value: 10}, {TMillis: 2, Value: 40}},
		},
	}
	if err := cumulativeDeltaSeries(series, map[string]bool{
		scalarKey: true, classicKey: true, nativeKey: true,
	}); err != nil {
		t.Fatalf("cumulativeDeltaSeries: %v", err)
	}

	for _, key := range []string{scalarKey, classicKey} {
		if got := series[key].Points[1].Value; got != map[string]float64{scalarKey: 50, classicKey: 6}[key] {
			t.Errorf("%s cumulative value = %v", key, got)
		}
	}
	native := series[nativeKey].Points[1].Histogram
	if native.Count != 18 || native.Sum != 9 || native.Scale != 0 || native.ZeroCount != 3 ||
		native.PositiveOffset != 0 || len(native.PositiveBuckets) != 1 || native.PositiveBuckets[0] != 3 ||
		native.NegativeOffset != -1 || len(native.NegativeBuckets) != 1 || native.NegativeBuckets[0] != 12 {
		t.Errorf("native cumulative histogram = %+v", native)
	}
	if got := series["__name__=cumulative_total"].Points[1].Value; got != 40 {
		t.Errorf("cumulative control changed to %v", got)
	}
}
