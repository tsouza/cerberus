package optimizer

import (
	"reflect"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// traceLookupPlan is Project(TraceLookupFilter(Filter(Scan))) as the TraceQL
// lowering builds it for `{ rootName = name && duration > 1 }`: the span input
// filtered by the conjunct that reads no per-trace value, joined to a rootName
// lookup the predicate compares with the span's own name.
func traceLookupPlan(lookup chplan.Node) *chplan.Project {
	spans := &chplan.Scan{Table: "otel_traces", Roles: []chplan.Column{{Name: "TraceId", Role: chplan.RoleTraceID}}}
	return &chplan.Project{
		Input: &chplan.TraceLookupFilter{
			Input: &chplan.Filter{
				Input:     spans,
				Predicate: &chplan.Binary{Op: chplan.OpGt, Left: &chplan.ColumnRef{Name: "Duration"}, Right: &chplan.LitInt{V: 1}},
			},
			Lookups: []chplan.Node{lookup},
			Predicate: &chplan.Binary{
				Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "_cerb_root_name"}, Right: &chplan.ColumnRef{Name: "SpanName"},
			},
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "SpanName"}, Alias: "MetricName"},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Alias: "TimeUnix"},
		},
	}
}

func rootNameLookup() chplan.Node {
	return &chplan.Aggregate{
		Input:          &chplan.Scan{Table: "otel_traces"},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
		GroupByAliases: []string{chplan.TraceLookupKeyColumn},
		AggFuncs: []chplan.AggFunc{{
			Fn:    chplan.FnArgMin,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: "SpanName"}, &chplan.ColumnRef{Name: "Timestamp"}},
			Alias: "_cerb_root_name",
		}},
	}
}

// TestProjectionPushdown_TraceLookupFilterNarrowsSpanInput pins that the span
// input of a TraceLookupFilter reads only what reaches the Project, what the
// joined predicate reads off the span row, what its own Filter reads, and the
// trace id the lookups join on — and never a column a lookup provides.
func TestProjectionPushdown_TraceLookupFilterNarrowsSpanInput(t *testing.T) {
	t.Parallel()
	out, changed := ProjectionPushdown{}.Apply(traceLookupPlan(rootNameLookup()))
	if !changed {
		t.Fatal("ProjectionPushdown did not narrow Project(TraceLookupFilter(Filter(Scan)))")
	}
	scan := out.(*chplan.Project).Input.(*chplan.TraceLookupFilter).Input.(*chplan.Filter).Input.(*chplan.Scan)
	want := []string{"Duration", "SpanName", "Timestamp", "TraceId"}
	if !reflect.DeepEqual(scan.Columns, want) {
		t.Fatalf("narrowed span input columns = %v, want %v", scan.Columns, want)
	}
	if _, again := (ProjectionPushdown{}).Apply(out); again {
		t.Fatal("ProjectionPushdown fired again on an already-narrowed span input")
	}
}

// TestProjectionPushdown_TraceLookupFilterOpenLookupDeclines pins that a
// lookup whose row type cannot name the columns it provides leaves the span
// input unnarrowed rather than guessing which predicate columns are its own.
func TestProjectionPushdown_TraceLookupFilterOpenLookupDeclines(t *testing.T) {
	t.Parallel()
	if _, changed := (ProjectionPushdown{}).Apply(traceLookupPlan(&chplan.Scan{Table: "otel_traces"})); changed {
		t.Fatal("ProjectionPushdown narrowed a span input beside an open-row-type lookup")
	}
}

// TestProjectionPushdown_AggregateOverExposingLookupNarrowsSpanInput pins the
// `{ } | by(rootName)` shape: an Aggregate whose group key reads an exposed
// lookup column narrows the span input under the TraceLookupFilter to the
// columns the Aggregate reads off the span row plus the trace id, never the
// exposed lookup column.
func TestProjectionPushdown_AggregateOverExposingLookupNarrowsSpanInput(t *testing.T) {
	t.Parallel()
	spans := &chplan.Scan{Table: "otel_traces", Roles: []chplan.Column{{Name: "TraceId", Role: chplan.RoleTraceID}}}
	plan := &chplan.Aggregate{
		Input: &chplan.TraceLookupFilter{
			Input:         &chplan.Filter{Input: spans, Predicate: &chplan.LitBool{V: true}},
			Lookups:       []chplan.Node{rootNameLookup()},
			ExposeLookups: true,
		},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "_cerb_root_name"}},
		GroupByAliases: []string{"GroupKey"},
		AggFuncs: []chplan.AggFunc{
			{Fn: chplan.FnAny, Args: []chplan.Expr{&chplan.ColumnRef{Name: "SpanId"}}, Alias: "SpanId"},
		},
	}
	out, changed := ProjectionPushdown{}.Apply(plan)
	if !changed {
		t.Fatal("ProjectionPushdown did not narrow Aggregate(TraceLookupFilter(Filter(Scan)))")
	}
	scan := out.(*chplan.Aggregate).Input.(*chplan.TraceLookupFilter).Input.(*chplan.Filter).Input.(*chplan.Scan)
	if want := []string{"SpanId", "TraceId"}; !reflect.DeepEqual(scan.Columns, want) {
		t.Fatalf("narrowed span input columns = %v, want %v", scan.Columns, want)
	}
	if _, again := (ProjectionPushdown{}).Apply(out); again {
		t.Fatal("ProjectionPushdown fired again on an already-narrowed span input")
	}
}

// TestProjectionPushdown_NestedTraceLookupFiltersNarrowInnermostSpanInput pins
// `{ rootName = name } | avg(traceDuration)`: an exposing TraceLookupFilter
// over the spanset filter's own TraceLookupFilter narrows through both to the
// span input, keeping what the inner predicate reads off the span row.
func TestProjectionPushdown_NestedTraceLookupFiltersNarrowInnermostSpanInput(t *testing.T) {
	t.Parallel()
	inner := traceLookupPlan(rootNameLookup()).Input.(*chplan.TraceLookupFilter)
	durationLookup := &chplan.Aggregate{
		Input:          &chplan.Scan{Table: "otel_traces"},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
		GroupByAliases: []string{chplan.TraceLookupKeyColumn},
		AggFuncs:       []chplan.AggFunc{{Fn: chplan.FnMax, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Duration"}}, Alias: "_cerb_trace_duration"}},
	}
	plan := &chplan.Aggregate{
		Input:          &chplan.TraceLookupFilter{Input: inner, Lookups: []chplan.Node{durationLookup}, ExposeLookups: true},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
		GroupByAliases: []string{"TraceId"},
		AggFuncs: []chplan.AggFunc{
			{Fn: chplan.FnAvg, Args: []chplan.Expr{&chplan.ColumnRef{Name: "_cerb_trace_duration"}}, Alias: "Value"},
		},
	}
	out, changed := ProjectionPushdown{}.Apply(plan)
	if !changed {
		t.Fatal("ProjectionPushdown did not narrow nested TraceLookupFilters")
	}
	scan := out.(*chplan.Aggregate).Input.(*chplan.TraceLookupFilter).Input.(*chplan.TraceLookupFilter).Input.(*chplan.Filter).Input.(*chplan.Scan)
	if want := []string{"Duration", "SpanName", "TraceId"}; !reflect.DeepEqual(scan.Columns, want) {
		t.Fatalf("narrowed span input columns = %v, want %v", scan.Columns, want)
	}
}
