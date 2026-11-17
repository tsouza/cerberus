package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMayCarryBucketBound pins both directions of the label-algebra proof.
// A false answer drops the argument unevaluated, so every shape whose
// output can hold an `le` label must answer true; the false rows are the
// shapes whose output labels exclude `le` by construction.
func TestMayCarryBucketBound(t *testing.T) {
	t.Parallel()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	for q, want := range map[string]bool{
		// Selectors and anything carrying their label sets through.
		`m_bucket`: true,
		`up`:       true,
		`up * 2`:   true,
		`2 * up`:   true,
		`up + on(job) group_left(le) sum by (job, le) (m_bucket)`: true,
		`sum(up) + m_bucket`:                        true,
		`abs(sum(up))`:                              true,
		`irate(m_bucket[5m])`:                       true,
		`label_replace(sum(up), "le", "1", "", "")`: true,
		`absent(up{le="1"})`:                        true,
		`(m_bucket)`:                                true,
		`sum by (le) (rate(m_bucket[5m]))`:          true,
		`sum without (job) (m_bucket)`:              true,
		`topk(2, m_bucket)`:                         true,
		`topk(2, sum by (le) (m_bucket))`:           true,
		`limitk(2, m_bucket)`:                       true,
		`count_values("le", up)`:                    true,
		`count_values by (le) ("v", m_bucket)`:      true,
		`quantile by (le) (0.5, m_bucket)`:          true,
		// Output labels exclude `le` by construction.
		`sum(m_bucket)`:                         false,
		`sum by (job) (m_bucket)`:               false,
		`sum without (le) (m_bucket)`:           false,
		`max by (job) (rate(m_bucket[5m]))`:     false,
		`count_values("v", m_bucket)`:           false,
		`count_values by (job) ("v", m_bucket)`: false,
		`topk(2, sum by (job) (m_bucket))`:      false,
		`sum(up) * 2`:                           false,
		`sum(up) + on() count(up)`:              false,
		`vector(1)`:                             false,
		`(vector(1))`:                           false,
		`quantile(0.5, m_bucket)`:               false,
	} {
		expr, err := p.ParseExpr(q)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", q, err)
		}
		if got := mayCarryBucketBound(expr); got != want {
			t.Errorf("mayCarryBucketBound(%q) = %v, want %v", q, got, want)
		}
	}
}

// TestPlanProvablyEmpty pins the plan-level emptiness proof: a
// constant-false Filter reached through row-mapping nodes proves the plan
// empty, while an Aggregate (one row over an empty input when it has no
// grouping keys) or a satisfiable predicate does not.
func TestPlanProvablyEmpty(t *testing.T) {
	t.Parallel()
	empty := &chplan.Filter{Input: &chplan.Scan{}, Predicate: &chplan.LitBool{V: false}}
	for name, tc := range map[string]struct {
		plan chplan.Node
		want bool
	}{
		"false filter":         {empty, true},
		"project over false":   {&chplan.Project{Input: empty}, true},
		"order by over false":  {&chplan.OrderBy{Input: &chplan.Project{Input: empty}}, true},
		"filter over false":    {&chplan.Filter{Input: empty, Predicate: &chplan.LitBool{V: true}}, true},
		"true filter":          {&chplan.Filter{Input: &chplan.Scan{}, Predicate: &chplan.LitBool{V: true}}, false},
		"aggregate over false": {&chplan.Project{Input: &chplan.Aggregate{Input: empty}}, false},
		"scan":                 {&chplan.Scan{}, false},
	} {
		if got := planProvablyEmpty(tc.plan); got != tc.want {
			t.Errorf("%s: planProvablyEmpty = %v, want %v", name, got, tc.want)
		}
	}
}
