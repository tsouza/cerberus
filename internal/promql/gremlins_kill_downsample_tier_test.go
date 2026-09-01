package promql

import (
	"testing"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// [buildDownsampleTierArm] builds the pre-aggregated tier relation a range
// window reads INSTEAD OF the raw samples when the tier covers the request. Its
// doc is explicit that the arm filters by "the SAME full label-matcher set the
// primary selector arm applies", because the tier table carries the whole
// series identity and there is no later join step to defer the rest of the
// matchers to.
//
// That makes the Filter load-bearing for correctness, not for cost: an
// unfiltered tier arm reads every series in the tier table and answers a
// window over the wrong rows. The `pred != nil` test guarding it survived as a
// mutant on `phase4-promql-lower` (issue #2883) — the fixtures that reach this
// arm all pass a non-empty matcher set and pin the emitted SQL, so nothing
// distinguished "wrap when there is a predicate" from its negation.
//
// Both directions are asserted, since the negation flips both: a matcher set
// must produce a Filter, and an empty one must NOT produce a Filter carrying a
// nil predicate.

// downsampleTierMetric is the series the tier arm below selects.
const downsampleTierMetric = "http_requests_total"

// tierArmProject unwraps the attributes Project every tier arm is wrapped in
// and returns the relation underneath it.
func tierArmProject(t *testing.T, n chplan.Node) chplan.Node {
	t.Helper()
	p, ok := n.(*chplan.Project)
	if !ok {
		t.Fatalf("buildDownsampleTierArm returned %#v; want the attributes *chplan.Project", n)
	}
	return p.Input
}

func TestBuildDownsampleTierArm_FiltersByTheFullMatcherSet(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	matchers := []*labels.Matcher{
		mustMatcher(t, labels.MatchEqual, labels.MetricName, downsampleTierMetric),
		mustMatcher(t, labels.MatchEqual, "job", "api"),
	}

	under := tierArmProject(t, buildDownsampleTierArm(matchers, s, lowerCtx{}))

	filter, ok := under.(*chplan.Filter)
	if !ok {
		t.Fatalf("tier arm relation = %#v; want a *chplan.Filter — an unfiltered tier scan reads every series in the tier table", under)
	}
	if want := buildPredicate(matchers, s); !filter.Predicate.Equal(want) {
		t.Errorf("tier arm predicate = %#v; want the same predicate the primary selector arm applies (%#v)", filter.Predicate, want)
	}
	scan, ok := filter.Input.(*chplan.Scan)
	if !ok || scan.Table != schema.DownsampleTierTable {
		t.Errorf("tier arm scans %#v; want a Scan of %q", filter.Input, schema.DownsampleTierTable)
	}
}

func TestBuildDownsampleTierArm_SkipsTheFilterWithNoMatchers(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	under := tierArmProject(t, buildDownsampleTierArm(nil, s, lowerCtx{}))

	scan, ok := under.(*chplan.Scan)
	if !ok {
		t.Fatalf("tier arm relation = %#v; want the bare *chplan.Scan — there is no predicate to filter on", under)
	}
	if scan.Table != schema.DownsampleTierTable {
		t.Errorf("tier arm scans %q; want %q", scan.Table, schema.DownsampleTierTable)
	}
}
