package promql

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/schema"
)

// The classic-histogram-companion fan-out decides how a `<x>_sum` / `<x>_count`
// selector is READ: as the single bare-name arm inside the histogram table, or
// as a UnionAll that also scans the Sum / Gauge tables under the literal
// suffixed name. Choosing the single arm when a distinct value table exists
// drops whichever rows live there — a silently short answer, not an error — and
// choosing the union when there is no such table adds an arm that scans the
// histogram table twice under two different names.
//
// [needCompanionUnion] and [literalCompanionValueTables] are the two pure
// functions that decision funnels through, and both were carrying unkilled
// mutants on `phase4-promql-lower` (issue #2883): the lowering fixtures pin the
// SQL of a fully-configured default schema, which satisfies every clause at
// once and so distinguishes none of them.

// companionSuffixedName / companionBareName are a matched `<x>_sum` pair: the
// user-visible suffixed name and the histogram-companion bare name it reduces
// to.
const (
	companionSuffixedName = "http_request_duration_seconds_sum"
	companionBareName     = "http_request_duration_seconds"
)

// TestNeedCompanionUnion_RejectsEachMissingNameIndependently pins that all
// three name inputs are required, one at a time.
//
// Each is load-bearing for a DIFFERENT part of the union: the value column is
// what the histogram arm reduces, the suffixed name is the literal the
// Sum/Gauge arm filters on, and the bare name is the literal the histogram arm
// filters on. A union built with any of them empty emits an arm matching the
// empty string, which returns no rows rather than failing.
//
// The schema is the fully-configured default, so [literalCompanionValueTables]
// is non-empty for every case — the union would be requested if and only if the
// name guard let it through. That isolates the guard.
func TestNeedCompanionUnion_RejectsEachMissingNameIndependently(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	if !needCompanionUnion(s, s.SumColumn, companionSuffixedName, companionBareName) {
		t.Fatal("the fully-named default-schema companion was refused; the rejection cases below would be vacuous")
	}

	for _, tc := range []struct {
		name        string
		valueColumn string
		suffixed    string
		bare        string
	}{
		{"no companion value column", "", companionSuffixedName, companionBareName},
		{"no suffixed name", s.SumColumn, "", companionBareName},
		{"no bare name", s.SumColumn, companionSuffixedName, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if needCompanionUnion(s, tc.valueColumn, tc.suffixed, tc.bare) {
				t.Errorf("needCompanionUnion requested a union with %s; want the single-arm histogram emit", tc.name)
			}
		})
	}
}

// TestNeedCompanionUnion_RequiresADistinctLiteralValueTable pins the other half
// of the decision: with all three names present, the union is requested if and
// only if there is somewhere OTHER than the histogram table for the literal
// suffixed name to live.
//
// The `> 0` is a strict comparison for a reason: a zero-length table list means
// every configured value table collapsed onto the histogram table, so the
// second arm would scan the same physical layout the first arm already reads,
// under a name that is not in it.
func TestNeedCompanionUnion_RequiresADistinctLiteralValueTable(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	s.SumTable = s.HistogramTable
	s.GaugeTable = s.HistogramTable

	if got := literalCompanionValueTables(s); len(got) != 0 {
		t.Fatalf("literalCompanionValueTables = %v; want empty once Sum and Gauge collapse onto Histogram", got)
	}
	if needCompanionUnion(s, s.SumColumn, companionSuffixedName, companionBareName) {
		t.Error("needCompanionUnion requested a union with no distinct literal value table; want the single-arm histogram emit")
	}
}

// TestLiteralCompanionValueTables_SkipsEmptyAndHistogramTables pins the two
// independent reasons a configured table is dropped from the literal-name arm
// list, and that both are checked for every candidate.
//
// An unconfigured (empty) table name must not become an arm scanning ""; the
// histogram table must not become one either, because it is already the bare-
// name arm and re-scanning it under the suffixed name matches nothing. The two
// cases below make exactly one of those reasons apply per candidate, so
// collapsing the pair into a conjunction admits whichever one is not shared.
func TestLiteralCompanionValueTables_SkipsEmptyAndHistogramTables(t *testing.T) {
	t.Parallel()

	t.Run("unconfigured sum table with a distinct histogram table", func(t *testing.T) {
		t.Parallel()
		s := schema.DefaultOTelMetrics()
		s.SumTable = ""

		got := literalCompanionValueTables(s)
		if len(got) != 1 || got[0] != s.GaugeTable {
			t.Errorf("literalCompanionValueTables = %v; want only the Gauge table %q — an empty name is not an arm",
				got, s.GaugeTable)
		}
	})

	t.Run("sum table configured as the histogram table", func(t *testing.T) {
		t.Parallel()
		s := schema.DefaultOTelMetrics()
		s.SumTable = s.HistogramTable

		got := literalCompanionValueTables(s)
		if len(got) != 1 || got[0] != s.GaugeTable {
			t.Errorf("literalCompanionValueTables = %v; want only the Gauge table %q — the histogram table is the bare-name arm",
				got, s.GaugeTable)
		}
	})
}

// TestResolveSelectorRouting_RewritesToTheBareNameOnlyForTheSingleArmFallback
// pins the consequence of the decision above on the matchers the selector
// actually scans with.
//
// The bare-name rewrite exists so the single-arm histogram fallback filters on
// the name the histogram row is stored under. It is correct ONLY on that
// fallback: when a distinct literal value table exists the union path owns the
// rewrite per-arm, and rewriting here as well would make the Sum/Gauge arm
// filter on the bare name — which is not in those tables — and return nothing.
//
// So the two configurations must disagree, and this asserts both. Testing only
// the default schema would leave the negation of the guard indistinguishable
// from the guard.
func TestResolveSelectorRouting_RewritesToTheBareNameOnlyForTheSingleArmFallback(t *testing.T) {
	t.Parallel()

	matchers := func(t *testing.T) []*labels.Matcher {
		t.Helper()
		return []*labels.Matcher{
			mustMatcher(t, labels.MatchEqual, model.MetricNameLabel, companionSuffixedName),
		}
	}

	t.Run("union path leaves the suffixed name alone", func(t *testing.T) {
		t.Parallel()
		s := schema.DefaultOTelMetrics()
		if got := literalCompanionValueTables(s); len(got) == 0 {
			t.Fatalf("default schema has no distinct literal value table (%v); this case would test the fallback instead", got)
		}

		route, err := resolveSelectorRouting(
			companionSuffixedName, s, lowerCtx{}, s.TablesFor(companionSuffixedName), matchers(t),
		)
		if err != nil {
			t.Fatalf("resolveSelectorRouting: %v", err)
		}
		if got := route.matchers[0].Value; got != companionSuffixedName {
			t.Errorf("routed matcher = %q; want the suffixed name %q left intact — the union path rewrites per-arm",
				got, companionSuffixedName)
		}
	})

	t.Run("single-arm fallback rewrites to the bare name", func(t *testing.T) {
		t.Parallel()
		s := schema.DefaultOTelMetrics()
		s.SumTable = s.HistogramTable
		s.GaugeTable = s.HistogramTable

		route, err := resolveSelectorRouting(
			companionSuffixedName, s, lowerCtx{}, s.TablesFor(companionSuffixedName), matchers(t),
		)
		if err != nil {
			t.Fatalf("resolveSelectorRouting: %v", err)
		}
		if got := route.matchers[0].Value; got != companionBareName {
			t.Errorf("routed matcher = %q; want the bare name %q — the histogram row is stored under it",
				got, companionBareName)
		}
	})
}

// TestRewriteMetricName_TouchesOnlyThePinnedNameMatcher pins both halves of
// rewriteMetricName's per-matcher predicate and its copy-on-write contract.
//
// The rewrite substitutes the caller's target name into a pinned `__name__=`
// matcher so the single-arm histogram fallback reads the bare companion name.
// Two failure modes it must not have: rewriting a matcher that is not the
// pinned name (which would silently retarget a label filter — `job="api"`
// becoming `job="<metric>"` matches nothing), and stopping the sweep at the
// first rewritten matcher (which would leave the rest of the slice nil and
// panic or drop filters downstream).
//
// The pinned matcher is deliberately NOT last, so a sweep that terminates early
// instead of continuing leaves an observable hole behind it.
func TestRewriteMetricName_TouchesOnlyThePinnedNameMatcher(t *testing.T) {
	t.Parallel()

	name := mustMatcher(t, labels.MatchEqual, model.MetricNameLabel, companionSuffixedName)
	job := mustMatcher(t, labels.MatchEqual, "job", "api")
	nameRegex := mustMatcher(t, labels.MatchRegexp, model.MetricNameLabel, "http_.*")
	in := []*labels.Matcher{name, job, nameRegex}

	out := rewriteMetricName(in, companionBareName)

	if len(out) != len(in) {
		t.Fatalf("rewriteMetricName returned %d matchers; want %d", len(out), len(in))
	}
	if out[0] == nil || out[0].Value != companionBareName {
		t.Errorf("out[0] = %v; want the pinned __name__ matcher rewritten to %q", out[0], companionBareName)
	}
	// Every matcher after the rewritten one must still be there: a sweep
	// that broke out of the loop leaves these nil.
	if out[1] != job {
		t.Errorf("out[1] = %v; want the untouched job matcher %v — only the pinned __name__ matcher is rewritten", out[1], job)
	}
	if out[2] != nameRegex {
		t.Errorf("out[2] = %v; want the untouched __name__ regex matcher %v — the rewrite is equality-only", out[2], nameRegex)
	}
	// Copy-on-write: the caller's slice and its matchers are never mutated,
	// because the parser reuses them across lowering passes.
	if in[0].Value != companionSuffixedName {
		t.Errorf("input matcher was mutated to %q; want the original %q", in[0].Value, companionSuffixedName)
	}
}
