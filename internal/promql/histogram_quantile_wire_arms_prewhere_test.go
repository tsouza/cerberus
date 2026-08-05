package promql

import (
	"testing"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// containsFuncCall reports whether e's tree contains a *chplan.FuncCall
// anywhere. internal/chsql/prewhere.go's isCheapPredicate treats FuncCall
// (and a handful of other node kinds not reachable from a matcher
// predicate) as NOT cheap, so any FuncCall in a predicate's tree keeps it
// out of PREWHERE and off ClickHouse's granule-pruning path.
func containsFuncCall(e chplan.Expr) bool {
	switch v := e.(type) {
	case *chplan.FuncCall:
		return true
	case *chplan.Binary:
		return containsFuncCall(v.Left) || containsFuncCall(v.Right)
	case *chplan.InList:
		if containsFuncCall(v.Left) {
			return true
		}
		for _, item := range v.List {
			if containsFuncCall(item) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// TestHistogramQuantileMatcherPredicate_PinnedNameStaysBareColumn pins the
// #1756 PERFORMANCE INVARIANT: a PINNED (`=`) wire-name matcher must
// resolve, at plan time, to a bare-column MetricName comparison — never a
// *chplan.FuncCall (e.g. concat(...)) — because
// internal/chsql/prewhere.go's isCheapPredicate treats *chplan.FuncCall as
// not-cheap and keeps it out of PREWHERE. A regression here silently
// converts a granule-pruned point lookup into a full table scan.
//
// The unpinned/regex case is asserted too (wantFuncCall: true) so the
// FuncCall-freedom check above is not vacuous: WireArmWireNameUnpinned
// legitimately DOES need the synthesized concat(MetricName, suffix)
// expression, since no stored column holds the Prometheus wire name for
// that case.
func TestHistogramQuantileMatcherPredicate_PinnedNameStaysBareColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()

	cases := []struct {
		name          string
		matcherType   labels.MatchType
		matcherValue  string
		wantFuncCall  bool
		wantColumnRef bool
	}{
		{
			name:          "pinned_equal_correct_suffix_stays_bare_column",
			matcherType:   labels.MatchEqual,
			matcherValue:  "demo_api_request_duration_seconds_bucket",
			wantFuncCall:  false,
			wantColumnRef: true,
		},
		{
			name:         "unpinned_regexp_needs_synthetic_concat",
			matcherType:  labels.MatchRegexp,
			matcherValue: "demo_api_request_duration_seconds_.+",
			wantFuncCall: true,
		},
		{
			name:         "unpinned_not_equal_needs_synthetic_concat",
			matcherType:  labels.MatchNotEqual,
			matcherValue: "demo_api_request_duration_seconds_bucket",
			wantFuncCall: true,
		},
		{
			name:         "unpinned_not_regexp_needs_synthetic_concat",
			matcherType:  labels.MatchNotRegexp,
			matcherValue: "demo_api_request_duration_seconds_.+",
			wantFuncCall: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := labels.NewMatcher(tc.matcherType, "__name__", tc.matcherValue)
			if err != nil {
				t.Fatalf("NewMatcher: %v", err)
			}
			pred, leMatchers := histogramQuantileMatcherPredicate([]*labels.Matcher{m}, s)
			if len(leMatchers) != 0 {
				t.Fatalf("leMatchers = %v, want none — this matcher targets __name__, not le", leMatchers)
			}
			if got := containsFuncCall(pred); got != tc.wantFuncCall {
				t.Fatalf("containsFuncCall(pred) = %v, want %v — predicate: %#v", got, tc.wantFuncCall, pred)
			}
			if !tc.wantColumnRef {
				return
			}
			// A pinned `__name__=` matcher fans out across OTel's
			// dotted-vs-underscored candidate spellings (see
			// metricNamePredicateOn / lower.go), so the bare-column shape
			// is either a plain equality (single-candidate value, e.g.
			// values with no rewritable separator) or an InList over that
			// same column — both are the "bare-column MetricName =/IN"
			// shape internal/chsql/prewhere.go's isCheapPredicate accepts,
			// as opposed to a *chplan.FuncCall-rooted comparison.
			var col *chplan.ColumnRef
			switch v := pred.(type) {
			case *chplan.Binary:
				if v.Op != chplan.OpEq {
					t.Fatalf("pred = %#v, want Op: OpEq", pred)
				}
				col, _ = v.Left.(*chplan.ColumnRef)
			case *chplan.InList:
				col, _ = v.Left.(*chplan.ColumnRef)
			default:
				t.Fatalf("pred = %#v, want *chplan.Binary{Op: OpEq, ...} or *chplan.InList — the "+
					"bare-column/IN shape internal/chsql/prewhere.go treats as cheap and PREWHERE-eligible", pred)
			}
			if col == nil || col.Name != s.MetricNameColumn {
				t.Fatalf("pred's left-hand column = %#v, want *chplan.ColumnRef{Name: %q}", col, s.MetricNameColumn)
			}
		})
	}
}

// TestSplitBucketMatchers_PinnedNameStaysBareColumn pins the same
// performance invariant for splitBucketMatchers's rewritten scanMatchers:
// a pinned `__name__=<bareName>_bucket`-style match rewrites to a plain
// labels.MatchEqual against the bare storage name — matcherToExpr then
// resolves it to a bare-column comparison — never a matcher that forces
// synthetic-name resolution.
func TestSplitBucketMatchers_PinnedNameStaysBareColumn(t *testing.T) {
	t.Parallel()

	const bareName = "demo_api_request_duration_seconds"

	m, err := labels.NewMatcher(labels.MatchEqual, "__name__", bareName+bucketSuffix)
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	scanMatchers, leMatchers := splitBucketMatchers([]*labels.Matcher{m}, bareName)
	if len(leMatchers) != 0 {
		t.Fatalf("leMatchers = %v, want none", leMatchers)
	}
	if len(scanMatchers) != 1 {
		t.Fatalf("scanMatchers = %v, want exactly one rewritten matcher", scanMatchers)
	}
	got := scanMatchers[0]
	if got.Type != labels.MatchEqual || got.Name != "__name__" || got.Value != bareName {
		t.Fatalf("scanMatchers[0] = %+v, want {Type: MatchEqual, Name: __name__, Value: %q} — "+
			"a pinned wire name must rewrite to the bare storage name so matcherToExpr keeps it a "+
			"bare-column comparison", got, bareName)
	}
}
