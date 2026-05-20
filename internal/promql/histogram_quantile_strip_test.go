package promql

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
)

// TestStripBucketSuffix pins the Grafana classic-histogram name
// translation: when a query references `<X>_bucket`, the classic
// lowering must read from the OTel-CH histogram row keyed by `<X>`
// (no suffix). The pre-fix behaviour silently routed the
// `MetricName='<X>_bucket'` predicate to the histogram table where it
// matched zero rows — every dashboard p95 panel rendered "No data".
//
// Coverage:
//   - `__name__="<X>_bucket"` MatchEqual → rewritten to `<X>`.
//   - `__name__="<X>"` (no suffix) → unchanged.
//   - `__name__=~"<X>_bucket"` MatchRegexp → unchanged (regex matchers
//     are user-authored; we do not edit them — stripping a regex
//     anchor would change the semantics).
//   - non-`__name__` matchers → unchanged.
//   - empty input → empty output (no panic on nil).
//   - boundary: `__name__="_bucket"` → stripped to `""` (acceptable —
//     no real histogram is named `_bucket`, so the resulting empty
//     `__name__` filter is harmless and matches no rows).
func TestStripBucketSuffix(t *testing.T) {
	t.Parallel()

	mk := func(typ labels.MatchType, name, value string) *labels.Matcher {
		m, err := labels.NewMatcher(typ, name, value)
		if err != nil {
			t.Fatalf("NewMatcher(%v, %q, %q): %v", typ, name, value, err)
		}
		return m
	}

	type expect struct {
		name, value string
		matchType   labels.MatchType
	}
	cases := []struct {
		name string
		in   []*labels.Matcher
		want []expect
	}{
		{
			name: "name_bucket_stripped",
			in: []*labels.Matcher{
				mk(labels.MatchEqual, model.MetricNameLabel, "http_request_duration_bucket"),
			},
			want: []expect{
				{name: model.MetricNameLabel, value: "http_request_duration", matchType: labels.MatchEqual},
			},
		},
		{
			name: "name_without_bucket_unchanged",
			in: []*labels.Matcher{
				mk(labels.MatchEqual, model.MetricNameLabel, "http_request_duration"),
			},
			want: []expect{
				{name: model.MetricNameLabel, value: "http_request_duration", matchType: labels.MatchEqual},
			},
		},
		{
			name: "name_regex_unchanged",
			in: []*labels.Matcher{
				mk(labels.MatchRegexp, model.MetricNameLabel, ".*_bucket"),
			},
			want: []expect{
				{name: model.MetricNameLabel, value: ".*_bucket", matchType: labels.MatchRegexp},
			},
		},
		{
			name: "other_labels_unchanged",
			in: []*labels.Matcher{
				mk(labels.MatchEqual, model.MetricNameLabel, "http_request_duration_bucket"),
				mk(labels.MatchEqual, "job", "api_bucket"),
				mk(labels.MatchRegexp, "instance", "host-.*"),
			},
			want: []expect{
				{name: model.MetricNameLabel, value: "http_request_duration", matchType: labels.MatchEqual},
				{name: "job", value: "api_bucket", matchType: labels.MatchEqual},
				{name: "instance", value: "host-.*", matchType: labels.MatchRegexp},
			},
		},
		{
			name: "empty_input",
			in:   nil,
			want: nil,
		},
		{
			name: "boundary_bare_underscore_bucket",
			in: []*labels.Matcher{
				mk(labels.MatchEqual, model.MetricNameLabel, "_bucket"),
			},
			want: []expect{
				{name: model.MetricNameLabel, value: "", matchType: labels.MatchEqual},
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := stripBucketSuffix(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len: got %d, want %d", len(got), len(tc.want))
			}
			for i, g := range got {
				w := tc.want[i]
				if g.Name != w.name || g.Value != w.value || g.Type != w.matchType {
					t.Errorf("matcher[%d]: got (%v, %q, %q), want (%v, %q, %q)",
						i, g.Type, g.Name, g.Value, w.matchType, w.name, w.value)
				}
			}
		})
	}
}

// TestStripBucketSuffix_DoesNotMutateInput pins the copy-on-write
// invariant: the input slice + matcher pointers must not be mutated
// by the strip. PromQL's parser may reuse matcher slices across
// lowering passes; an in-place rewrite would leak the bare name back
// into subsequent passes that expected the `_bucket`-suffixed form.
func TestStripBucketSuffix_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	origin, err := labels.NewMatcher(labels.MatchEqual, model.MetricNameLabel, "http_request_duration_bucket")
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	in := []*labels.Matcher{origin}

	out := stripBucketSuffix(in)
	if out[0] == origin {
		t.Fatalf("stripBucketSuffix reused the input pointer; must allocate a fresh matcher")
	}
	if origin.Value != "http_request_duration_bucket" {
		t.Errorf("input matcher Value mutated: %q", origin.Value)
	}
	if out[0].Value != "http_request_duration" {
		t.Errorf("output matcher Value: got %q, want %q", out[0].Value, "http_request_duration")
	}
}
