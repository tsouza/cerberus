package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMutation_AbsentGridAnchorFrag_OffsetDispatch kills the
// CONDITIONALS_NEGATION mutant on absent_over_time.go:absentGridAnchorFrag:
// `if offsetNS == 0` rewritten to `if offsetNS != 0`. Offset zero must
// render the bare anchor column, and a non-zero offset must render the
// offset-unshift expression.
func TestMutation_AbsentGridAnchorFrag_OffsetDispatch(t *testing.T) {
	t.Parallel()

	zero := renderFragToSQL(absentGridAnchorFrag(0))
	if strings.Contains(zero, "toIntervalNanosecond") {
		t.Fatalf("offset zero must render the bare anchor, got %q", zero)
	}

	shifted := renderFragToSQL(absentGridAnchorFrag(int64(time.Second)))
	if !strings.Contains(shifted, "toIntervalNanosecond") {
		t.Fatalf("non-zero offset must render the shifted anchor, got %q", shifted)
	}
}

// TestMutation_AbsentOverTimeBookendFrag_OffsetDispatch kills the
// CONDITIONALS_NEGATION mutant on absent_over_time.go:absentOverTimeBookendFrag:
// `if offsetNS == 0` rewritten to `if offsetNS != 0`. Offset zero must keep
// the bare bookend; a non-zero offset must wrap it in the subtraction.
func TestMutation_AbsentOverTimeBookendFrag_OffsetDispatch(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	zero := renderFragToSQL(absentOverTimeBookendFrag(ts, 0))
	if strings.Contains(zero, "toIntervalNanosecond") {
		t.Fatalf("offset zero must render the bare bookend, got %q", zero)
	}

	shifted := renderFragToSQL(absentOverTimeBookendFrag(ts, int64(time.Second)))
	if !strings.Contains(shifted, "toIntervalNanosecond") {
		t.Fatalf("non-zero offset must render the offset bookend, got %q", shifted)
	}
}

// TestMutation_SynthAttrsMapFrag_EmptyLabelsCastEmptyMap kills the
// CONDITIONALS_NEGATION mutant on absent_over_time.go:synthAttrsMapFrag:
// `if len(labels) == 0` rewritten to `if len(labels) != 0`. An empty label
// set must render CAST(map(), 'Map(String,String)'), while a non-empty set
// must render the parameterised map of key/value pairs.
func TestMutation_SynthAttrsMapFrag_EmptyLabelsCastEmptyMap(t *testing.T) {
	t.Parallel()

	emptyBuilder := NewBuilder()
	synthAttrsMapFrag(nil)(emptyBuilder)
	empty, emptyArgs, _ := emptyBuilder.Build()
	if !strings.Contains(empty, "CAST(map(),") {
		t.Fatalf("empty labels must render the empty-map cast, got %q", empty)
	}
	if len(emptyArgs) != 1 || emptyArgs[0] != "Map(String,String)" {
		t.Fatalf("empty labels args = %#v, want [Map(String,String)]", emptyArgs)
	}

	nonEmptyBuilder := NewBuilder()
	synthAttrsMapFrag([]chplan.SynthLabel{{Key: "k", Value: "v"}})(nonEmptyBuilder)
	nonEmpty, nonEmptyArgs, _ := nonEmptyBuilder.Build()
	if !strings.Contains(nonEmpty, "map(") {
		t.Fatalf("non-empty labels must render the label pair, got %q", nonEmpty)
	}
	if len(nonEmptyArgs) != 2 || nonEmptyArgs[0] != "k" || nonEmptyArgs[1] != "v" {
		t.Fatalf("non-empty labels args = %#v, want [k v]", nonEmptyArgs)
	}
}

// TestMutation_RateWindowFanoutRowBound_ZeroFallsBackToDefault kills the
// CONDITIONALS_BOUNDARY mutant on rate_window_fanout_bound.go:
// `if e.rateWindowFanoutMaxRows > 0` rewritten to `>= 0`. A zero override is
// the sentinel meaning "no override": it must fall back to the compiled-in
// default, not be returned as a zero row bound.
func TestMutation_RateWindowFanoutRowBound_ZeroFallsBackToDefault(t *testing.T) {
	t.Parallel()

	e := &emitter{}
	if got := e.rateWindowFanoutRowBound(); got != maxRateWindowFanoutRows {
		t.Fatalf("rateWindowFanoutRowBound() = %d, want default %d", got, maxRateWindowFanoutRows)
	}

	e.rateWindowFanoutMaxRows = 7
	if got := e.rateWindowFanoutRowBound(); got != 7 {
		t.Fatalf("rateWindowFanoutRowBound() = %d, want override 7", got)
	}
}

// TestResolveRangeLWRInputColumns_RequiresEveryRoleThroughTheResolver pins
// that resolveRangeLWRInputColumns routes each of its four identities through
// chplan.Schema.UniqueNamedRole: a child missing exactly one of them is
// rejected by the resolver's own ErrRoleMissing, not by a later emitter step.
func TestResolveRangeLWRInputColumns_RequiresEveryRoleThroughTheResolver(t *testing.T) {
	t.Parallel()

	all := []chplan.Column{
		{Name: "metric", Role: chplan.RoleMetricName},
		{Name: "attrs", Role: chplan.RoleAttributes},
		{Name: "ts", Role: chplan.RoleTimestamp},
		{Name: "value", Role: chplan.RoleValue},
	}
	for _, dropped := range all {
		t.Run("missing "+dropped.Role.String(), func(t *testing.T) {
			t.Parallel()
			roles := make([]chplan.Column, 0, len(all)-1)
			for _, column := range all {
				if column != dropped {
					roles = append(roles, column)
				}
			}
			_, err := resolveRangeLWRInputColumns(rangeLWRRoleProject(roles...))
			if !errors.Is(err, ErrUnsupported) || !errors.Is(err, chplan.ErrRoleMissing) {
				t.Fatalf("a child missing %s must be rejected as ErrUnsupported+ErrRoleMissing, got %v", dropped.Role, err)
			}
		})
	}
}

// TestMutation_RangeBucketFanoutFoldCostAliases_RequiresGroupArray kills the
// INVERT_LOGICAL mutant on range_bucket_fanout.go:rangeBucketFanoutFoldCostAliases:
// `if samples == "" && af.Fn == chplan.FnGroupArray` rewritten to `||`.
//
// With no groupArray accumulator in the list at all, the original leaves
// `samples` empty and returns the "requires a groupArray AggFunc" error.
// The mutant sets `samples` from the first non-groupArray alias instead,
// and the malformed accumulator set is accepted.
func TestMutation_RangeBucketFanoutFoldCostAliases_RequiresGroupArray(t *testing.T) {
	t.Parallel()

	_, _, err := rangeBucketFanoutFoldCostAliases([]chplan.AggFunc{
		{Fn: chplan.FnSum, Alias: "sum"},
	})
	if err == nil {
		t.Fatal("rangeBucketFanoutFoldCostAliases accepted an accumulator set with no groupArray")
	}
}

// TestAbsentOverTime_PrefilterLowerBookendFollowsQueryMode pins which bookend
// bounds the global matcher prefilter from below. A range query's earliest
// anchor is Start, so the prefilter must reach back to Start - Range; an
// instant query has its single anchor at End and ignores Start, so the
// prefilter starts at End - Range.
func TestAbsentOverTime_PrefilterLowerBookendFollowsQueryMode(t *testing.T) {
	t.Parallel()

	const inputTimestamp = "physical_sample_time"
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	start := end.Add(-time.Hour)
	lowerBound := func(bookend time.Time) string {
		return renderFragToSQL(Gt(Col(inputTimestamp),
			Sub(absentOverTimeBookendFrag(bookend, 0), Call("toIntervalNanosecond", InlineLit(time.Minute.Nanoseconds())))))
	}
	for name, tc := range map[string]struct {
		step        time.Duration
		want, avoid time.Time
	}{
		"range":   {step: time.Minute, want: start, avoid: end},
		"instant": {step: 0, want: end, avoid: start},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sql, _, err := Emit(context.Background(), &chplan.AbsentOverTime{
				Input:            closedRoleProject(chplan.Column{Name: inputTimestamp, Role: chplan.RoleTimestamp}),
				Range:            time.Minute,
				Start:            start,
				End:              end,
				Step:             tc.step,
				TimestampColumn:  "TimeUnix",
				MetricNameColumn: "MetricName",
				AttributesColumn: "Attributes",
				ValueColumn:      "Value",
			})
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if want := lowerBound(tc.want); !strings.Contains(sql, want) {
				t.Errorf("prefilter lower bound: want %s\nSQL: %s", want, sql)
			}
			if avoid := lowerBound(tc.avoid); strings.Contains(sql, avoid) {
				t.Errorf("prefilter lower bound must not start from %s\nSQL: %s", avoid, sql)
			}
		})
	}
}
