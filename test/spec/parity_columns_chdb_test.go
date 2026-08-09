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
