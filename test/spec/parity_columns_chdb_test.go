//go:build chdb

package spec

import (
	"strings"
	"testing"
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
			want: sampleColumns{name: 0, attrs: 1, ts: 2, value: 3},
		},
		{
			// An instant aggregation drops __name__ and has no sample
			// time to report, so it projects only what its answer has.
			name: "instant aggregation projection",
			cols: []string{"Attributes", "Value"},
			want: sampleColumns{name: -1, attrs: 0, ts: -1, value: 1},
		},
		{
			// anchor_ts is scaffolding of the emitted subquery, not a
			// field of the answer, and must not be mistaken for the
			// sample time sitting behind it.
			name: "subquery projection interposing anchor_ts",
			cols: []string{"Attributes", "anchor_ts", "TimeUnix", "Value"},
			want: sampleColumns{name: -1, attrs: 0, ts: 2, value: 3},
		},
		{
			name: "subquery projection with anchor_ts and no sample time",
			cols: []string{"Attributes", "anchor_ts", "Value"},
			want: sampleColumns{name: -1, attrs: 0, ts: -1, value: 2},
		},
		{
			name: "named subquery projection interposing anchor_ts",
			cols: []string{"MetricName", "Attributes", "anchor_ts", "TimeUnix", "Value"},
			want: sampleColumns{name: 0, attrs: 1, ts: 3, value: 4},
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
