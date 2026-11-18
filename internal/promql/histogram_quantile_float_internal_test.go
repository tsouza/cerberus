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
		// Selectors that can carry `le`, and anything carrying their
		// label sets through.
		`m_bucket`:                             true,
		`{__name__="m_bucket"}`:                true,
		`{job="a"}`:                            true,
		`{__name__=~"up.*"}`:                   true,
		`up{le="1"}`:                           true,
		`up{le!=""}`:                           true,
		`up{le="1"} * 2`:                       true,
		`-m_bucket`:                            true,
		`m_bucket * 2`:                         true,
		`irate(m_bucket[5m])`:                  true,
		`abs(m_bucket)`:                        true,
		`clamp_max(m_bucket, 10)`:              true,
		`max_over_time((m_bucket * 2)[5m:1m])`: true,
		`up + on(job) group_left(le) sum by (job, le) (m_bucket)`: true,
		`sum(up) + m_bucket`:                   true,
		`(m_bucket)`:                           true,
		`sum by (le) (rate(m_bucket[5m]))`:     true,
		`sum without (job) (m_bucket)`:         true,
		`topk(2, m_bucket)`:                    true,
		`topk(2, sum by (le) (m_bucket))`:      true,
		`limitk(2, m_bucket)`:                  true,
		`count_values by (le) ("v", m_bucket)`: true,
		`quantile by (le) (0.5, m_bucket)`:     true,
		`sort_by_label(m_bucket, "job")`:       true,
		// Labels minted or copied in, whatever the operand carries.
		`label_replace(sum(up), "le", "1", "", "")`:           true,
		`label_replace(up, "le", "$1", "job", "(.*)")`:        true,
		`label_join(up, "le", ",", "job")`:                    true,
		`histogram_quantiles(up, "le", 0.5)`:                  true,
		`count_values("le", up)`:                              true,
		`absent(up{le="1"})`:                                  true,
		`absent_over_time(up{le="1"}[5m])`:                    true,
		`info(m_bucket)`:                                      true,
		`info(up, {le=~".+"})`:                                true,
		`info(up, {__name__="build_bucket"})`:                 true,
		`info(up, {__name__=~"m.*"})`:                         true,
		`info(up, {__name__!="target_info"})`:                 true,
		`label_replace(m_bucket, "dst", "$1", "job", "(.*)")`: true,
		// A selector pinning a name without the `_bucket` suffix and
		// with no `le` matcher, and anything carrying its labels.
		`up`:                             false,
		`{__name__="up"}`:                false,
		`up @ 100`:                       false,
		`up * 2`:                         false,
		`2 * up`:                         false,
		`up + up`:                        false,
		`-up`:                            false,
		`abs(up)`:                        false,
		`abs(sum(up))`:                   false,
		`clamp_max(up, 1)`:               false,
		`round(up, 0.1)`:                 false,
		`rate(up[5m])`:                   false,
		`rate(requests_total[5m]) * 2`:   false,
		`max_over_time((up * 2)[5m:1m])`: false,
		`sum by (le) (up)`:               false,
		`topk(2, up)`:                    false,
		`up + on(job) group_left(le) queue_depth`:      false,
		`label_replace(up, "dst", "$1", "le", "(.*)")`: false,
		`histogram_quantiles(up, "q", 0.5)`:            false,
		`absent(up)`:                                   false,
		`absent(up{job="a"})`:                          false,
		`absent(m_bucket)`:                             false,
		`info(up)`:                                     false,
		`info(up, {k8s_cluster_name=~".+"})`:           false,
		`info(up, {__name__="build_info"})`:            false,
		`info(up, {__name__="m_bucket", job="a"})`:     false,
		`info(up, {})`:                                 false,
		// Scalars and label-free outputs.
		`up * scalar(m_bucket)`: false,
		`hour()`:                false,
		`vector(time())`:        false,
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
