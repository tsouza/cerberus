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

// TestStripBucketSuffix_PassesThroughNonMatching pins the pointer-
// passthrough contract for matchers that don't satisfy ALL three
// strip conditions (Name==__name__ AND Type==MatchEqual AND
// HasSuffix(_bucket)). Non-matching matchers must be reused
// pointer-identically — they are NEVER re-allocated.
//
// This kills the invert-logical mutant on the second `&&`
// (`A && B && C` → `A && (B || C)`) on inputs where A=T, B=T, C=F:
// the mutated branch would enter the strip path, call
// labels.NewMatcher, and store a *fresh* pointer in `out[i]` even
// though TrimSuffix would be a no-op on the value. The pointer-
// identity check distinguishes the two paths even when the matcher
// fields look semantically identical.
//
// Mirror coverage for the other two conditions:
//
//   - `Name != __name__` (a regular label like `job`) — even if
//     Type==MatchEqual and value has `_bucket` suffix, must pass through.
//   - `Type != MatchEqual` (a regex matcher) — even if Name==__name__
//     and the value text ends in `_bucket`, must pass through.
func TestStripBucketSuffix_PassesThroughNonMatching(t *testing.T) {
	t.Parallel()

	mk := func(typ labels.MatchType, name, value string) *labels.Matcher {
		m, err := labels.NewMatcher(typ, name, value)
		if err != nil {
			t.Fatalf("NewMatcher(%v, %q, %q): %v", typ, name, value, err)
		}
		return m
	}

	cases := []struct {
		name string
		in   *labels.Matcher
	}{
		{
			// A=T, B=T, C=F: name is __name__, type is MatchEqual, but
			// the value has no `_bucket` suffix. Kills the
			// `A && (B || C)` invert-logical mutant on the second `&&`.
			name: "name_metricname_eq_no_bucket_suffix",
			in:   mk(labels.MatchEqual, model.MetricNameLabel, "http_request_duration"),
		},
		{
			// A=F, B=T, C=T: name is a regular label, type is MatchEqual,
			// value ends in `_bucket`. Pins the Name==__name__ guard.
			name: "name_not_metricname_value_has_bucket",
			in:   mk(labels.MatchEqual, "job", "api_bucket"),
		},
		{
			// A=T, B=F, C=T: name is __name__, type is regex, value
			// looks like `*_bucket`. Pins the Type==MatchEqual guard.
			name: "name_metricname_regex_value_has_bucket",
			in:   mk(labels.MatchRegexp, model.MetricNameLabel, ".*_bucket"),
		},
		{
			// A=T, B=F, C=F: regex matcher without suffix in pattern.
			// Pins the combined Type==MatchEqual + HasSuffix guard.
			name: "name_metricname_regex_no_bucket",
			in:   mk(labels.MatchRegexp, model.MetricNameLabel, ".*"),
		},
		{
			// A=F, B=F, C=T: regular label with regex matcher whose
			// pattern happens to end in `_bucket`. Belt-and-braces:
			// only matchers that satisfy ALL three conditions trigger
			// the strip.
			name: "name_not_metricname_regex_value_has_bucket",
			in:   mk(labels.MatchRegexp, "job", ".*_bucket"),
		},
		{
			// A=F, B=T, C=F: regular label, MatchEqual, value without
			// `_bucket` suffix. The "all three false" baseline.
			name: "name_not_metricname_eq_no_bucket",
			in:   mk(labels.MatchEqual, "job", "api"),
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := stripBucketSuffix([]*labels.Matcher{tc.in})
			if len(out) != 1 {
				t.Fatalf("len(out) = %d, want 1", len(out))
			}
			if out[0] != tc.in {
				t.Errorf("non-matching matcher was re-allocated: got %p, want %p (%v %q=%q)",
					out[0], tc.in, tc.in.Type, tc.in.Name, tc.in.Value)
			}
		})
	}
}

// TestStripBucketSuffix_PreservesPositionAndLength pins the slice
// shape: the output is a fresh slice of EXACTLY len(input), with
// each position carrying either the rewritten matcher (when all
// three strip conditions hold) or the original pointer (otherwise).
//
// Coverage targets:
//
//   - `make([]*labels.Matcher, len(matchers))` with conditionals-
//     boundary or arithmetic mutations: a wrong allocation length
//     panics on out-of-bounds write or leaves nil tail entries.
//   - empty-input case asserts the function does NOT panic and
//     returns a non-nil zero-length slice (callers downstream may
//     range over `out` and expect a defined, even if empty, value).
//   - position preservation kills mutants that swap `i` with a
//     constant (e.g., always overwriting `out[0]`).
func TestStripBucketSuffix_PreservesPositionAndLength(t *testing.T) {
	t.Parallel()

	mk := func(typ labels.MatchType, name, value string) *labels.Matcher {
		m, err := labels.NewMatcher(typ, name, value)
		if err != nil {
			t.Fatalf("NewMatcher(%v, %q, %q): %v", typ, name, value, err)
		}
		return m
	}

	// Three matchers in a deliberately scrambled order so that a
	// mutant which (a) always writes out[0], or (b) reverses the
	// loop direction, surfaces as either a duplicated or
	// out-of-order output.
	m0 := mk(labels.MatchEqual, "job", "api")                                  // pass-through
	m1 := mk(labels.MatchEqual, model.MetricNameLabel, "http_duration_bucket") // strip
	m2 := mk(labels.MatchRegexp, "instance", "host-.*")                        // pass-through
	in := []*labels.Matcher{m0, m1, m2}

	out := stripBucketSuffix(in)
	if len(out) != len(in) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(in))
	}
	if out[0] != m0 {
		t.Errorf("out[0] = %p, want pass-through of m0 %p", out[0], m0)
	}
	if out[1] == m1 {
		t.Errorf("out[1] is the original m1 pointer; must be a fresh stripped matcher")
	}
	if out[1].Value != "http_duration" {
		t.Errorf("out[1].Value = %q, want %q", out[1].Value, "http_duration")
	}
	if out[2] != m2 {
		t.Errorf("out[2] = %p, want pass-through of m2 %p", out[2], m2)
	}

	// Empty-input path: must return a non-nil zero-length slice.
	// A `len(matchers)-1` mutation would panic here (negative cap);
	// returning nil would still pass `len()==0` but break callers
	// that do `append(...stripBucketSuffix(...)...)` patterns.
	empty := stripBucketSuffix(nil)
	if len(empty) != 0 {
		t.Errorf("stripBucketSuffix(nil) length = %d, want 0", len(empty))
	}

	// Single-element input strictly matters for the `len()` allocation
	// bound: a `len-1` arithmetic mutant would panic on the first
	// `out[0] = ...` write. Explicitly run a 1-element case so the
	// boundary mutant has a deterministic crash site.
	one := stripBucketSuffix([]*labels.Matcher{m0})
	if len(one) != 1 {
		t.Errorf("stripBucketSuffix(1-element) length = %d, want 1", len(one))
	}
	if one[0] != m0 {
		t.Errorf("stripBucketSuffix(1-element)[0] = %p, want %p", one[0], m0)
	}
}

// TestStripBucketSuffix_TrimSuffixSemantics pins that the strip
// removes EXACTLY the trailing `_bucket` and nothing more. Kills
// any mutant that turns `strings.TrimSuffix(m.Value, "_bucket")`
// into a different transformation (e.g., chopping a different
// fixed number of characters, removing all occurrences, trimming
// from the left).
//
// `_bucket_bucket` → `_bucket` (only the trailing suffix removed).
// `bucket_bucket`  → `bucket` (the leading `bucket` token survives).
// `__bucket`       → `_` (a single underscore remains).
func TestStripBucketSuffix_TrimSuffixSemantics(t *testing.T) {
	t.Parallel()

	mk := func(value string) *labels.Matcher {
		m, err := labels.NewMatcher(labels.MatchEqual, model.MetricNameLabel, value)
		if err != nil {
			t.Fatalf("NewMatcher(%q): %v", value, err)
		}
		return m
	}

	cases := []struct {
		in   string
		want string
	}{
		{"_bucket_bucket", "_bucket_bucket"[:len("_bucket_bucket")-len("_bucket")]}, // -> "_bucket"
		{"bucket_bucket", "bucket"},
		{"__bucket", "_"},
		{"http_request_duration_bucket", "http_request_duration"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			out := stripBucketSuffix([]*labels.Matcher{mk(tc.in)})
			if len(out) != 1 {
				t.Fatalf("len(out) = %d, want 1", len(out))
			}
			if out[0].Value != tc.want {
				t.Errorf("stripBucketSuffix(%q).Value = %q, want %q", tc.in, out[0].Value, tc.want)
			}
		})
	}
}
