package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestHistogramSyntheticNames pins the enumeration against the invariant that
// makes it safe to publish: every name it returns must be one the selector
// lowering routes back to the same classic-histogram base row. The assertion
// below therefore checks the names AND re-derives the routing, so a lowering
// change that stops serving one of these names fails here rather than silently
// leaving a name advertised that no query can answer.
func TestHistogramSyntheticNames(t *testing.T) {
	t.Parallel()

	metrics := schema.DefaultOTelMetrics()
	const base = "synth_latency_seconds"

	got := HistogramSyntheticNames(base, metrics)
	want := []string{base + "_bucket", base + "_count", base + "_sum"}
	if len(got) != len(want) {
		t.Fatalf("HistogramSyntheticNames(%q) = %v, want %v", base, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("HistogramSyntheticNames(%q) = %v, want %v", base, got, want)
		}
	}
	for _, name := range got {
		if !resolvesToHistogramRow(name, base, metrics) {
			t.Errorf("advertised name %q does not route back to base %q", name, base)
		}
		if name == base {
			t.Errorf("bare base name %q must not be advertised as a series name", base)
		}
	}
}

// TestHistogramSyntheticNames_Empty pins the two cases with nothing to
// enumerate: an empty base, and a deployment with no classic-histogram table
// (where no row could serve these names).
func TestHistogramSyntheticNames_Empty(t *testing.T) {
	t.Parallel()

	metrics := schema.DefaultOTelMetrics()
	if got := HistogramSyntheticNames("", metrics); got != nil {
		t.Errorf("empty base: got %v, want nil", got)
	}
	metrics.HistogramTable = ""
	if got := HistogramSyntheticNames("synth_latency_seconds", metrics); got != nil {
		t.Errorf("no histogram table: got %v, want nil", got)
	}
}

// TestResolvesToHistogramRow pins the negative side of the shared routing
// decision: names that belong to other physical layouts (or to a different
// base) must not be claimed by the histogram row.
func TestResolvesToHistogramRow(t *testing.T) {
	t.Parallel()

	metrics := schema.DefaultOTelMetrics()
	const base = "synth_latency_seconds"

	cases := []struct {
		name string
		want bool
	}{
		{name: base + "_bucket", want: true},
		{name: base + "_count", want: true},
		{name: base + "_sum", want: true},
		// The stored row's own name is not a wire series name.
		{name: base},
		// The counter convention routes to the sum table.
		{name: base + "_total"},
		// A companion of a DIFFERENT family.
		{name: "synth_size_bytes_sum"},
	}
	for _, tc := range cases {
		if got := resolvesToHistogramRow(tc.name, base, metrics); got != tc.want {
			t.Errorf("resolvesToHistogramRow(%q, %q) = %v, want %v", tc.name, base, got, tc.want)
		}
	}
}
