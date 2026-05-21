package logql

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestHasTimeWindowAsymmetric pins the asymmetric guards in
// [lowerCtx.hasTimeWindow]: a non-degenerate window requires BOTH bounds
// to be non-zero. The helper reads `!Start.IsZero() && !End.IsZero()`;
// flipping the connective to `||` would treat a half-zero pair as a
// valid window and emit a spurious BETWEEN predicate. Test each of the
// four corners explicitly.
func TestHasTimeWindowAsymmetric(t *testing.T) {
	t.Parallel()

	someTS := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		start time.Time
		end   time.Time
		want  bool
	}{
		{name: "both zero -> no window", start: time.Time{}, end: time.Time{}, want: false},
		{name: "only start set -> no window", start: someTS, end: time.Time{}, want: false},
		{name: "only end set -> no window", start: time.Time{}, end: someTS, want: false},
		{name: "both set -> window", start: someTS, end: someTS.Add(time.Hour), want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lc := lowerCtx{Start: tc.start, End: tc.end}
			if got := lc.hasTimeWindow(); got != tc.want {
				t.Fatalf("hasTimeWindow() = %v, want %v (start=%v end=%v)", got, tc.want, tc.start, tc.end)
			}
		})
	}
}

// TestRegexpMergeLabelsSkipsUnnamedSubexps pins the
// `i == 0 || n == ""` skip in [regexpMergeLabels]: index 0 is the
// whole-match group and any positional (unnamed) subexp at i > 0 has no
// name in `re.SubexpNames()`. Both shapes must be dropped before the
// duplicate-detection map (`seen`) ingests them — otherwise multiple
// unnamed subexps in the same pattern would all hash under the same
// empty-string key, tripping the duplicate-capture guard and erroring
// out on patterns LogQL accepts. Flipping the connective to `&&` keeps
// the i==0 skip but lets every positional subexp leak through.
//
// The returned expression is a `mapConcat(prev, map(<key>, <val>, ...))`
// FuncCall — the inner `map(...)` must carry exactly `2*len(named)` args.
// We pin both directions: (a) the inner map has 2 args (one named
// capture, key+value), (b) every other arg of that map is a non-empty
// string literal (so unnamed subexps with `n == ""` did not leak in
// as keys).
func TestRegexpMergeLabelsSkipsUnnamedSubexps(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	// Pattern with two unnamed positional groups plus one named group.
	// SubexpNames() returns ["", "", "", "name"]. The original guard
	// processes only index 3; the mutant `&&` form would walk all four,
	// build three namedGroup entries (two with empty names), and trip
	// the "duplicate named capture" error on the second empty name.
	pattern := `(\d+)-(\d+) (?P<name>\w+)`

	expr, err := regexpMergeLabels(nil, s, pattern)
	if err != nil {
		t.Fatalf("regexpMergeLabels(%q) returned error: %v — unnamed subexps leaked into the duplicate-check map", pattern, err)
	}
	if expr == nil {
		t.Fatalf("regexpMergeLabels(%q) returned nil expression", pattern)
	}

	outer, ok := expr.(*chplan.FuncCall)
	if !ok {
		t.Fatalf("regexpMergeLabels(%q) returned %T, want *chplan.FuncCall (mapConcat)", pattern, expr)
	}
	if outer.Name != "mapConcat" {
		t.Fatalf("regexpMergeLabels(%q) outer FuncCall.Name = %q, want %q", pattern, outer.Name, "mapConcat")
	}
	if len(outer.Args) != 2 {
		t.Fatalf("regexpMergeLabels(%q) mapConcat has %d args, want 2", pattern, len(outer.Args))
	}

	inner, ok := outer.Args[1].(*chplan.FuncCall)
	if !ok {
		t.Fatalf("regexpMergeLabels(%q) inner Args[1] is %T, want *chplan.FuncCall (map)", pattern, outer.Args[1])
	}
	if inner.Name != "map" {
		t.Fatalf("regexpMergeLabels(%q) inner FuncCall.Name = %q, want %q", pattern, inner.Name, "map")
	}

	// One named capture -> exactly 2 (key, value) args. With the `&&`
	// mutant, this would be 6 (three captures: two unnamed + the named
	// "name") — assuming the duplicate-check didn't error first.
	if len(inner.Args) != 2 {
		t.Fatalf("regexpMergeLabels(%q) inner map has %d args, want 2 — unnamed subexps leaked through the skip", pattern, len(inner.Args))
	}

	// First arg is the key literal. Confirm it's the named capture and
	// not the empty positional-subexp name.
	key, ok := inner.Args[0].(*chplan.LitString)
	if !ok {
		t.Fatalf("regexpMergeLabels(%q) inner map key is %T, want *chplan.LitString", pattern, inner.Args[0])
	}
	if key.V != "name" {
		t.Fatalf("regexpMergeLabels(%q) inner map key = %q, want %q", pattern, key.V, "name")
	}
}

// TestRangeModeAsymmetric pins the two guards that gate the matrix
// RangeWindow shape: `lowerCtx.rangeMode` reads
// `c.Step > 0 && c.hasTimeWindow()`. Two mutations target this line:
//
//   - INVERT_LOGICAL flips `&&` to `||`, which makes the Step > 0 leg
//     alone (or hasTimeWindow alone) trip range-mode and emit a matrix
//     shape against an instant query — the engine would then look for a
//     per-row `anchor_ts` column that the inner instant lowering does
//     not produce.
//   - CONDITIONALS_BOUNDARY flips `> 0` to `>= 0`, which makes a Step of
//     exactly zero satisfy the predicate. A range-mode flag without a
//     real Step would divide-by-zero in the matrix emitter's anchor
//     grid.
//
// Pin the four corners so both mutations diverge from the original.
func TestRangeModeAsymmetric(t *testing.T) {
	t.Parallel()

	someTS := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		start time.Time
		end   time.Time
		step  time.Duration
		want  bool
	}{
		// Step == 0 with a real window → rangeMode is false. The
		// CONDITIONALS_BOUNDARY mutant (`Step >= 0`) would flip this to
		// true because `0 >= 0`.
		{name: "step zero with window -> instant", start: someTS, end: someTS.Add(time.Hour), step: 0, want: false},
		// Step > 0 without a window → rangeMode is false. The
		// INVERT_LOGICAL mutant (`||`) would flip this to true because
		// the Step > 0 leg alone satisfies the disjunction.
		{name: "step set without window -> instant", start: time.Time{}, end: time.Time{}, step: time.Minute, want: false},
		// Both legs satisfied → rangeMode is true. The original requires
		// both; either mutant would also return true here, so this case
		// guards the positive side of the conjunction.
		{name: "both step and window -> range", start: someTS, end: someTS.Add(time.Hour), step: time.Minute, want: true},
		// Neither leg satisfied → rangeMode is false. Both mutants also
		// return false; this case anchors the baseline.
		{name: "neither step nor window -> instant", start: time.Time{}, end: time.Time{}, step: 0, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lc := lowerCtx{Start: tc.start, End: tc.end, Step: tc.step}
			if got := lc.rangeMode(); got != tc.want {
				t.Fatalf("rangeMode() = %v, want %v (start=%v end=%v step=%v)", got, tc.want, tc.start, tc.end, tc.step)
			}
		})
	}
}

// TestWithMatcherWindowExtension pins the start-backshift behaviour of
// [lowerCtx.withMatcherWindowExtension]. The helper is the entry point
// the range-aggregation lowering uses to widen the pre-scan
// `Timestamp >= start AND Timestamp <= end` clamp's left bound by the
// `[range]` selector's interval so the leftmost per-anchor windows in a
// matrix evaluation still see the full `(anchor_ts - range, anchor_ts]`
// slice. Three behaviours are pinned:
//
//   - A positive extension on a context with a window moves Start back
//     by exactly the requested duration. End and Step stay untouched.
//   - A non-positive extension (including zero) is a no-op. Callers
//     compute `interval + offset` and can pass a negative result when
//     offset is forward-shifting; the helper must absorb that without
//     touching the bounds.
//   - A positive extension against a context with NO window is a no-op
//     — the pre-scan clamp would not be emitted anyway, so widening the
//     hypothetical Start could only confuse downstream telemetry.
func TestWithMatcherWindowExtension(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	step := time.Minute

	cases := []struct {
		name      string
		lc        lowerCtx
		extension time.Duration
		wantStart time.Time
		wantEnd   time.Time
		wantStep  time.Duration
	}{
		{
			name:      "positive extension on windowed range ctx -> Start moves back",
			lc:        lowerCtx{Start: start, End: end, Step: step},
			extension: 5 * time.Minute,
			wantStart: start.Add(-5 * time.Minute),
			wantEnd:   end,
			wantStep:  step,
		},
		{
			name:      "zero extension is a no-op",
			lc:        lowerCtx{Start: start, End: end, Step: step},
			extension: 0,
			wantStart: start,
			wantEnd:   end,
			wantStep:  step,
		},
		{
			name:      "negative extension is a no-op (forward-shift offset)",
			lc:        lowerCtx{Start: start, End: end, Step: step},
			extension: -time.Minute,
			wantStart: start,
			wantEnd:   end,
			wantStep:  step,
		},
		{
			name:      "no window -> positive extension is a no-op",
			lc:        lowerCtx{},
			extension: 5 * time.Minute,
			wantStart: time.Time{},
			wantEnd:   time.Time{},
			wantStep:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.lc.withMatcherWindowExtension(tc.extension)
			if !got.Start.Equal(tc.wantStart) {
				t.Fatalf("withMatcherWindowExtension(%v).Start = %v, want %v", tc.extension, got.Start, tc.wantStart)
			}
			if !got.End.Equal(tc.wantEnd) {
				t.Fatalf("withMatcherWindowExtension(%v).End = %v, want %v", tc.extension, got.End, tc.wantEnd)
			}
			if got.Step != tc.wantStep {
				t.Fatalf("withMatcherWindowExtension(%v).Step = %v, want %v", tc.extension, got.Step, tc.wantStep)
			}
		})
	}
}

// TestIsMatrixRangeWindowBoundary pins the `v.OuterRange > 0` boundary
// in [isMatrixRangeWindow]. A CONDITIONALS_BOUNDARY mutant flips `> 0`
// to `>= 0`, which would classify a zero-OuterRange instant RangeWindow
// as a matrix node. The caller — vector-aggregation lowering — would
// then add a non-existent `anchor_ts` column to the GROUP BY and the
// emitter would fail at SQL build time.
//
// Pin the boundary explicitly: OuterRange == 0 must report false,
// any positive duration must report true.
func TestIsMatrixRangeWindowBoundary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		outerRange  time.Duration
		want        bool
		description string
	}{
		{name: "OuterRange zero -> instant", outerRange: 0, want: false, description: "boundary: must be strictly positive"},
		{name: "OuterRange positive -> matrix", outerRange: time.Hour, want: true},
		// 1 nanosecond is the smallest positive duration that still
		// satisfies `> 0`; pin it as a separate case so a downgrade to
		// `>= 1ms` (or any other threshold) also surfaces.
		{name: "OuterRange one ns -> matrix", outerRange: time.Nanosecond, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rw := &chplan.RangeWindow{OuterRange: tc.outerRange}
			if got := isMatrixRangeWindow(rw); got != tc.want {
				t.Fatalf("isMatrixRangeWindow(OuterRange=%v) = %v, want %v (%s)", tc.outerRange, got, tc.want, tc.description)
			}
		})
	}
}

// TestIsMatrixRangeWindowWalksWrappers pins the recursive walk through
// the value-rewrite Projects / Filters that preserve the inner RangeWindow
// shape. Without the recursion, a vector-aggregation over a Project-wrapped
// matrix RangeWindow would never add `anchor_ts` to its GROUP BY.
func TestIsMatrixRangeWindowWalksWrappers(t *testing.T) {
	t.Parallel()

	matrix := &chplan.RangeWindow{OuterRange: time.Hour}
	instant := &chplan.RangeWindow{OuterRange: 0}

	cases := []struct {
		name string
		node chplan.Node
		want bool
	}{
		{name: "bare matrix", node: matrix, want: true},
		{name: "bare instant", node: instant, want: false},
		{name: "Project over matrix", node: &chplan.Project{Input: matrix}, want: true},
		{name: "Filter over matrix", node: &chplan.Filter{Input: matrix}, want: true},
		{name: "Project over instant", node: &chplan.Project{Input: instant}, want: false},
		{name: "nested wrappers over matrix", node: &chplan.Project{Input: &chplan.Filter{Input: matrix}}, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isMatrixRangeWindow(tc.node); got != tc.want {
				t.Fatalf("isMatrixRangeWindow(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestResourceFallbackColumn pins the table of Prom-label-name → CH
// top-level-column entries that anchor the OTel-CH resource-attribute
// fallback. The matcher lowering reads this helper to decide whether
// to wrap the ResourceAttributes lookup in a coalesce(nullIf(...), ...)
// against a dedicated column. Adding a new entry requires both a
// schema.Logs field AND an upstream OTel-CH exporter that promotes the
// label out of the map — the helper deliberately stays narrow so a
// regression that silently expands the table is loud in this test.
func TestResourceFallbackColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	cases := []struct {
		name  string
		label string
		want  string
	}{
		{name: "service_name resolves to ServiceName", label: "service_name", want: "ServiceName"},
		{name: "job has no top-level column", label: "job", want: ""},
		{name: "k8s_pod_name has no top-level column", label: "k8s_pod_name", want: ""},
		{name: "detected_level has no top-level column", label: "detected_level", want: ""},
		{name: "unknown label has no top-level column", label: "totally_unknown_label", want: ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resourceFallbackColumn(s, tc.label); got != tc.want {
				t.Errorf("resourceFallbackColumn(%q) = %q, want %q", tc.label, got, tc.want)
			}
		})
	}
}

// TestResourceFallbackColumn_RespectsSchemaOverride pins the
// custom-schema opt-out: a user whose CH layout has no dedicated
// ServiceName column clears `schema.Logs.ServiceNameColumn` and the
// helper returns "" so the matcher lowering stays on the map-only
// path. Without this, the lowering would emit a coalesce against a
// non-existent column and every query against that schema would 500.
func TestResourceFallbackColumn_RespectsSchemaOverride(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	s.ServiceNameColumn = ""
	if got := resourceFallbackColumn(s, "service_name"); got != "" {
		t.Errorf("resourceFallbackColumn with cleared ServiceNameColumn = %q, want \"\"", got)
	}
}
