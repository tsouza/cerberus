package engine

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// A PromQL scalar parameter that is computed rather than written as a literal
// binds its vector as a chplan.ScalarSubquery, and per-step binding puts a
// ts-grid node INSIDE that scalar interior. The interior hangs off an Expr
// slot, which chplan.Node.Children does not report, so the plan's only
// experimental aggregate is invisible to a Children()-only sweep. When the
// gate misses it the setting is not stamped and ClickHouse answers code 63
// ("Aggregate function timeSeriesResampleToGridWithStaleness is experimental
// and disabled by default") — a 502 on `vector(scalar(m))` and on
// `topk(scalar(m) * 2, m)`.
//
// The two corpus shapes fail on DIFFERENT dispatches: `vector(scalar(m))` on
// the main statement, `topk(scalar(m) * 2, m)` on the ScalarGuard that runs
// the K parameter's own query. Both seams are pinned here.

// scalarInteriorTSGridPlan builds a plan whose ONLY experimental ts-grid node
// is buried inside a ScalarSubquery hanging off a Project projection — the
// shape the per-step scalar binding produces. Nothing on the Children() spine
// is a ts-grid node, so a Walk-based gate answers false for it.
func scalarInteriorTSGridPlan(interior chplan.Node) chplan.Node {
	return &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{{
			Expr: &chplan.Binary{
				Op:    chplan.OpMul,
				Left:  &chplan.ScalarSubquery{Input: interior},
				Right: &chplan.LitInt{V: 2},
			},
			Alias: "Value",
		}},
	}
}

// resampleInterior builds an emittable RangeWindowResample — the node a
// per-step scalar binding puts inside the ScalarSubquery. The column names and
// the pinned grid are what the emitter requires, so the guard test can drive it
// all the way through emit.
func resampleInterior() *chplan.RangeWindowResample {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeWindowResample{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         start,
		End:           start.Add(5 * time.Minute),
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
}

// tsGridInteriors is the pair of experimental node kinds the setting gates:
// the resample family (timeSeriesResampleToGridWithStaleness, what a per-step
// scalar binding emits) and the native family (timeSeriesRateToGrid and
// siblings). Both must be seen through a scalar interior.
func tsGridInteriors() map[string]chplan.Node {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return map[string]chplan.Node{
		"RangeWindowResample": resampleInterior(),
		"RangeWindowNative": &chplan.RangeWindowNative{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "rate",
			Range:           5 * time.Minute,
			Step:            30 * time.Second,
			Start:           start,
			End:             start.Add(5 * time.Minute),
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		},
	}
}

// TestExecContext_StampsTSGridSettingForScalarInterior pins the MAIN dispatch
// seam: execContext must put the experimental setting on the ctx it hands the
// ClickHouse client for a plan whose only ts-grid node sits inside a
// ScalarSubquery.
func TestExecContext_StampsTSGridSettingForScalarInterior(t *testing.T) {
	t.Parallel()

	for name, interior := range tsGridInteriors() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := &Engine{Optimizer: optimizer.Default()}
			ctx, _ := e.execContext(context.Background(), scalarInteriorTSGridPlan(interior), "promql", nil)
			got := chclient.QuerySettingsFromContext(ctx)[chclient.SettingExperimentalTSGridAggregate]
			if got != 1 {
				t.Errorf("%s = %v on the dispatch ctx, want 1 — the only %s in the plan is inside a "+
					"ScalarSubquery, so ClickHouse rejects the query with code 63",
					chclient.SettingExperimentalTSGridAggregate, got, name)
			}
		})
	}
}

// TestExecContext_LeavesTSGridSettingOffPlainPlans is the negative control:
// widening the sweep must not start stamping an experimental setting on every
// query. An unknown or unwanted setting can itself error on an older server,
// so the gate has to stay shape-specific.
func TestExecContext_LeavesTSGridSettingOffPlainPlans(t *testing.T) {
	t.Parallel()

	e := &Engine{Optimizer: optimizer.Default()}
	plain := scalarInteriorTSGridPlan(&chplan.Aggregate{
		Input:    &chplan.Scan{Table: "otel_metrics_gauge"},
		AggFuncs: []chplan.AggFunc{{Name: "max", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "Value"}},
	})
	ctx, _ := e.execContext(context.Background(), plain, "promql", nil)
	if got, ok := chclient.QuerySettingsFromContext(ctx)[chclient.SettingExperimentalTSGridAggregate]; ok {
		t.Errorf("%s = %v on a plan with no ts-grid node anywhere, want absent",
			chclient.SettingExperimentalTSGridAggregate, got)
	}
}

// ctxCapturingQuerier records the ctx of every dispatch so a test can read the
// per-query ClickHouse settings that rode with it.
type ctxCapturingQuerier struct {
	ctxs []context.Context
}

func (q *ctxCapturingQuerier) Query(ctx context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	q.ctxs = append(q.ctxs, ctx)
	return nil, nil
}

// TestRunGuards_StampsTSGridSettingForScalarInterior pins the GUARD dispatch
// seam. `topk(scalar(m) * 2, m)` fails on the guard query, not on the main
// statement: the guard runs the K parameter's own plan, which is where the
// ts-grid node lives. A fix that only deepens the main seam leaves this half
// of the regression un-stamped.
func TestRunGuards_StampsTSGridSettingForScalarInterior(t *testing.T) {
	t.Parallel()

	q := &ctxCapturingQuerier{}
	e := &Engine{Optimizer: optimizer.Default(), Client: q}
	meta := Meta{Guards: []Guard{{
		Name:  "topk K",
		Plan:  scalarInteriorTSGridPlan(resampleInterior()),
		Check: func([]float64) error { return nil },
	}}}

	if err := e.runGuards(context.Background(), &tsGridStubLang{}, meta); err != nil {
		t.Fatalf("runGuards: %v", err)
	}
	if len(q.ctxs) != 1 {
		t.Fatalf("guard dispatches = %d, want 1", len(q.ctxs))
	}
	got := chclient.QuerySettingsFromContext(q.ctxs[0])[chclient.SettingExperimentalTSGridAggregate]
	if got != 1 {
		t.Errorf("%s = %v on the guard dispatch ctx, want 1 — the guard plan's only ts-grid node is "+
			"inside a ScalarSubquery, so ClickHouse rejects the guard query with code 63",
			chclient.SettingExperimentalTSGridAggregate, got)
	}
}

// tsGridStubLang is the minimal Lang runGuards needs: it never parses (the
// guard plan is supplied directly) and passes its plan through unchanged.
type tsGridStubLang struct{}

func (*tsGridStubLang) Name() string { return "promql" }

func (*tsGridStubLang) Parse(context.Context, string) (chplan.Node, Meta, error) {
	return nil, Meta{}, nil
}

func (*tsGridStubLang) ProjectSamples(plan chplan.Node, _ Meta) chplan.Node { return plan }
