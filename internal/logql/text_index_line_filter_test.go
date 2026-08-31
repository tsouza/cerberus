package logql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// findLineContents recursively collects every *chplan.LineContent reachable
// from expr, walking the chplan.Binary AND/OR tree lowerLineFilterChain
// builds. Sufficient for the flat line-filter chains this test constructs;
// not a general chplan.Expr walker.
func findLineContents(expr chplan.Expr) []*chplan.LineContent {
	switch e := expr.(type) {
	case *chplan.LineContent:
		return []*chplan.LineContent{e}
	case *chplan.Binary:
		out := findLineContents(e.Left)
		out = append(out, findLineContents(e.Right)...)
		return out
	default:
		return nil
	}
}

// onlyLineContent lowers query and returns the single *chplan.LineContent
// its Filter predicate carries, failing the test if the plan doesn't have
// exactly one Filter with exactly one LineContent.
func onlyLineContent(t *testing.T, opts logql.LowerOpts) *chplan.LineContent {
	t.Helper()
	s := schema.DefaultOTelLogs()
	expr, err := logql.ParseExprPermissive(`{app="x"} |= "connection reset by peer"`)
	if err != nil {
		t.Fatalf("ParseExprPermissive: %v", err)
	}
	plan, err := logql.LowerAtRangeOpts(context.Background(), expr, s, time.Time{}, time.Time{}, 0, opts)
	if err != nil {
		t.Fatalf("LowerAtRangeOpts: %v", err)
	}
	f, ok := plan.(*chplan.Filter)
	if !ok {
		t.Fatalf("plan root = %T, want *chplan.Filter", plan)
	}
	lcs := findLineContents(f.Predicate)
	if len(lcs) != 1 {
		t.Fatalf("found %d LineContent node(s) in predicate, want 1: %#v", len(lcs), f.Predicate)
	}
	return lcs[0]
}

// TestLowerAtRangeOpts_TextIndexLineFilter_Threading pins that
// LowerOpts.TextIndexLineFilter (cerberus issue #2773, chopt
// text_index_line_filter) reaches the lowered chplan.LineContent node's
// TextIndexPrefilter field for a plain, non-negated |= filter, and that it
// stays false when the option is unset — the byte-identical-by-default
// contract every chopt-gated feature in this repo carries.
func TestLowerAtRangeOpts_TextIndexLineFilter_Threading(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts logql.LowerOpts
		want bool
	}{
		{"disabled_zero_value", logql.LowerOpts{}, false},
		{"enabled", logql.LowerOpts{TextIndexLineFilter: true}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lc := onlyLineContent(t, tt.opts)
			if lc.TextIndexPrefilter != tt.want {
				t.Errorf("TextIndexPrefilter = %v, want %v", lc.TextIndexPrefilter, tt.want)
			}
			if lc.Pattern != "connection reset by peer" || lc.IsRegex || lc.Negated {
				t.Errorf("unexpected LineContent shape: %#v", lc)
			}
		})
	}
}

// TestLower_TextIndexLineFilter_DefaultEntrypointsInert pins that the
// zero-options entry points ([Lower], [LowerAt], [LowerAtRange]) — every
// caller except internal/logql.Lang — never set TextIndexPrefilter, since
// they construct a zero-value lowerCtx with no way to opt in.
func TestLower_TextIndexLineFilter_DefaultEntrypointsInert(t *testing.T) {
	s := schema.DefaultOTelLogs()
	expr, err := logql.ParseExprPermissive(`{app="x"} |= "connection reset by peer"`)
	if err != nil {
		t.Fatalf("ParseExprPermissive: %v", err)
	}
	plan, err := logql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	f, ok := plan.(*chplan.Filter)
	if !ok {
		t.Fatalf("plan root = %T, want *chplan.Filter", plan)
	}
	lcs := findLineContents(f.Predicate)
	if len(lcs) != 1 {
		t.Fatalf("found %d LineContent node(s), want 1", len(lcs))
	}
	if lcs[0].TextIndexPrefilter {
		t.Errorf("Lower() (zero-options entrypoint) set TextIndexPrefilter=true unexpectedly")
	}
}

// TestLowerAtRangeOpts_TextIndexLineFilter_NegatedInert pins the
// LineContent.TextIndexPrefilter eligibility contract's negation carve-out:
// a negated filter (`!=`) NEVER gets TextIndexPrefilter=true, even with
// the chopt feature enabled — a superset prefilter has no sound dual for
// "must not contain".
func TestLowerAtRangeOpts_TextIndexLineFilter_NegatedInert(t *testing.T) {
	s := schema.DefaultOTelLogs()
	expr, err := logql.ParseExprPermissive(`{app="x"} != "connection reset by peer"`)
	if err != nil {
		t.Fatalf("ParseExprPermissive: %v", err)
	}
	plan, err := logql.LowerAtRangeOpts(context.Background(), expr, s, time.Time{}, time.Time{}, 0,
		logql.LowerOpts{TextIndexLineFilter: true})
	if err != nil {
		t.Fatalf("LowerAtRangeOpts: %v", err)
	}
	f, ok := plan.(*chplan.Filter)
	if !ok {
		t.Fatalf("plan root = %T, want *chplan.Filter", plan)
	}
	lcs := findLineContents(f.Predicate)
	if len(lcs) != 1 {
		t.Fatalf("found %d LineContent node(s), want 1", len(lcs))
	}
	if !lcs[0].Negated {
		t.Fatalf("expected Negated=true for `!=`, got false")
	}
	if lcs[0].TextIndexPrefilter {
		t.Errorf("negated filter got TextIndexPrefilter=true, want false")
	}
}
