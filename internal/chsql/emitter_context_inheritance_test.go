package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The four production sites that used to build a bare `&emitter{}` — the two
// Expr-embedded subquery renderers here, plus EmitMetricsExemplars and
// EmitCompareRootLeg — each discarded every ctx-carried bound and strategy
// chsql.Emit seeds. These tests pin that they inherit them now (cerberus
// issue #3186). Each one is written against OBSERVABLE SQL rather than
// against the emitter's fields, so it fails if the plumbing is removed even
// if the fields survive.

const inheritanceAttrColumn = "ResourceAttributes"

func jsonAttrStrategies() AttrStrategies {
	return AttrStrategies{inheritanceAttrColumn: AttrStrategyJSON}
}

// attrReadScan is a Scan projecting one attribute-map key read, the smallest
// plan whose SQL differs between AttrStrategyMap and AttrStrategyJSON.
func attrReadScan(table string) chplan.Node {
	return &chplan.Project{
		Input: &chplan.Scan{Table: table},
		Projections: []chplan.Projection{{
			Expr: &chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: inheritanceAttrColumn},
				Key: &chplan.LitString{V: "k"},
			},
			Alias: "attr",
		}},
	}
}

// renderedAsJSON reports whether sql carries the JSON dynamic-subcolumn read
// shape rather than the Map subscript. The two are mutually exclusive for the
// same access, so this is a faithful "which strategy rendered it" question.
func renderedAsJSON(sql string) bool {
	return strings.Contains(sql, "coalesce(`"+inheritanceAttrColumn+"`.")
}

// TestScalarSubqueryInheritsAttrStrategies pins that a chplan.ScalarSubquery
// renders its embedded plan inside the OUTER emission. Before this, the site
// built a bare emitter, so the subtree rendered Map syntax against a
// JSON-typed column and failed at query time.
func TestScalarSubqueryInheritsAttrStrategies(t *testing.T) {
	t.Parallel()

	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
		Projections: []chplan.Projection{{
			Expr:  &chplan.ScalarSubquery{Input: attrReadScan("otel_metrics_sum")},
			Alias: "scalar",
		}},
	}

	ctx := WithAttrStrategies(context.Background(), jsonAttrStrategies())
	sql, _, err := Emit(ctx, plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !renderedAsJSON(sql) {
		t.Fatalf("the scalar subquery rendered Map syntax against a JSON-typed column:\n%s", sql)
	}
}

// TestInSubqueryInheritsAttrStrategies is the same pin for chplan.InSubquery,
// the other Expr slot that embeds a whole plan subtree.
func TestInSubqueryInheritsAttrStrategies(t *testing.T) {
	t.Parallel()

	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
		Predicate: &chplan.InSubquery{
			Left:     &chplan.ColumnRef{Name: "MetricName"},
			Subquery: attrReadScan("otel_metrics_sum"),
		},
	}

	ctx := WithAttrStrategies(context.Background(), jsonAttrStrategies())
	sql, _, err := Emit(ctx, plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !renderedAsJSON(sql) {
		t.Fatalf("the IN subquery rendered Map syntax against a JSON-typed column:\n%s", sql)
	}
}

// TestEmbeddedSubqueryStrategiesAreScopedToTheEmission is the negative half:
// with no strategies on the ctx, the same plans render the Map subscript. It
// is what proves the two tests above assert a real switch rather than a shape
// the emitter produces unconditionally.
func TestEmbeddedSubqueryStrategiesAreScopedToTheEmission(t *testing.T) {
	t.Parallel()

	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
		Projections: []chplan.Projection{{
			Expr:  &chplan.ScalarSubquery{Input: attrReadScan("otel_metrics_sum")},
			Alias: "scalar",
		}},
	}

	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if renderedAsJSON(sql) {
		t.Fatalf("a plain emission rendered the JSON shape with no strategies on the ctx:\n%s", sql)
	}
}

// TestSubEmitterSharesTheCTECounter pins the second half of the same bug: a
// sub-emitter renders into its own buffer but its SQL is spliced into the
// SAME statement, so a restarted CTE counter re-emits a name the outer
// statement already bound — ClickHouse error 49, the exact collision the
// counter exists to prevent.
func TestSubEmitterSharesTheCTECounter(t *testing.T) {
	t.Parallel()

	e := newEmitter(context.Background())
	if got := e.nextCTESeq(); got != 1 {
		t.Fatalf("first CTE sequence = %d, want 1", got)
	}
	sub := e.sub()
	if got := sub.nextCTESeq(); got != 2 {
		t.Fatalf("sub-emitter restarted the CTE counter at %d, want 2", got)
	}
	if got := e.nextCTESeq(); got != 3 {
		t.Fatalf("the outer emitter did not see the sub-emitter's advance: got %d, want 3", got)
	}
}

// TestSubEmitterCarriesEveryContextSeededBound pins that sub() forwards every
// ctx-seeded field, so adding a bound to newEmitter and forgetting it here
// cannot silently leave subqueries unbounded.
func TestSubEmitterCarriesEveryContextSeededBound(t *testing.T) {
	t.Parallel()

	ctx := WithAttrStrategies(context.Background(), jsonAttrStrategies())
	ctx = WithSpansTable(ctx, "custom_spans")
	e := newEmitter(ctx)
	sub := e.sub()

	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"spansTable", sub.spansTable, e.spansTable},
		{"ctxSpansTable", sub.ctxSpansTable, e.ctxSpansTable},
		{"deltaPrefixLookbackNS", sub.deltaPrefixLookbackNS, e.deltaPrefixLookbackNS},
		{"deltaPrefixReadEnabled", sub.deltaPrefixReadEnabled, e.deltaPrefixReadEnabled},
		{"rangeBucketFanoutMaxRows", sub.rangeBucketFanoutMaxRows, e.rangeBucketFanoutMaxRows},
		{"rangeLWRFanoutMaxRows", sub.rangeLWRFanoutMaxRows, e.rangeLWRFanoutMaxRows},
		{"rateWindowFanoutMaxRows", sub.rateWindowFanoutMaxRows, e.rateWindowFanoutMaxRows},
		{"rangeBucketGridNativeMaxRows", sub.rangeBucketGridNativeMaxRows, e.rangeBucketGridNativeMaxRows},
		{"rangeBucketGridNativeMaxDensityUnits", sub.rangeBucketGridNativeMaxDensityUnits, e.rangeBucketGridNativeMaxDensityUnits},
		{"emittedSQLMaxBytes", sub.emittedSQLMaxBytes, e.emittedSQLMaxBytes},
	} {
		if c.got != c.want {
			t.Errorf("sub-emitter %s = %v, outer emitter has %v", c.name, c.got, c.want)
		}
	}
	if sub.deltaPrefixLookbackNS == 0 {
		t.Error("deltaPrefixLookbackNS is 0, the explicit \"no lower bound\" value — the default was not seeded")
	}
	if len(sub.attrStrategies) != len(e.attrStrategies) {
		t.Errorf("sub-emitter attrStrategies = %v, outer emitter has %v", sub.attrStrategies, e.attrStrategies)
	}
}

// TestEmitMetricsExemplarsInheritsAttrStrategies pins the third of the four
// sites. The exemplars statement is built here and executed directly by the
// Tempo handlers, so it never flows through Emit — which is exactly why its
// emitter has to be seeded from the ctx itself rather than left bare.
func TestEmitMetricsExemplarsInheritsAttrStrategies(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	attrRead := &chplan.MapAccess{
		Map: &chplan.ColumnRef{Name: inheritanceAttrColumn},
		Key: &chplan.LitString{V: "k"},
	}
	m := &chplan.MetricsAggregate{
		Op:             chplan.MetricsOpRate,
		GroupBy:        []chplan.Expr{attrRead},
		GroupByAliases: []string{"attr"},
		ValueAlias:     "Value",
		Inner:          &chplan.Scan{Table: "otel_traces"},
	}
	rw := &chplan.RangeWindow{
		Input:           m,
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}

	ctx := WithAttrStrategies(context.Background(), jsonAttrStrategies())
	sql, _, _, err := EmitMetricsExemplars(ctx, rw, m, "TraceId", "SpanId", 0, "")
	if err != nil {
		t.Fatalf("EmitMetricsExemplars: %v", err)
	}
	if !renderedAsJSON(sql) {
		t.Fatalf("the exemplars statement rendered Map syntax against a JSON-typed column:\n%s", sql)
	}

	plain, _, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 0, "")
	if err != nil {
		t.Fatalf("EmitMetricsExemplars (no strategies): %v", err)
	}
	if renderedAsJSON(plain) {
		t.Fatalf("the exemplars statement rendered the JSON shape with no strategies on the ctx:\n%s", plain)
	}
}

// TestEmitCompareRootLegInheritsAttrStrategies pins the fourth site. The
// compare root leg is rendered outside Emit so a caller can run it on its
// own, and it too built a bare emitter.
func TestEmitCompareRootLegInheritsAttrStrategies(t *testing.T) {
	t.Parallel()

	m := &chplan.MetricsCompare{
		Selection: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "StatusCode"},
			Right: &chplan.LitString{V: "Error"},
		},
		TopN: 10,
		Pairs: &chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{
				&chplan.LitString{V: "name"},
				&chplan.ColumnRef{Name: "SpanName"},
			}},
		}},
		SelAlias:   "is_selection",
		AttrAlias:  "attr",
		ValAlias:   "val",
		ValueAlias: "Value",
		Inner: &chplan.Scan{Table: "otel_traces", Roles: []chplan.Column{
			{Name: "Timestamp", Role: chplan.RoleTimestamp},
		}},
		TraceIDColumn: "TraceId",
		RootLookup: &chplan.Aggregate{
			Input: &chplan.Filter{
				Input: &chplan.Scan{Table: "otel_traces", Roles: []chplan.Column{
					{Name: "Timestamp", Role: chplan.RoleTimestamp},
				}},
				Predicate: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: "ParentSpanId"},
					Right: &chplan.LitString{V: ""},
				},
			},
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
			AggFuncs: []chplan.AggFunc{{
				Fn: chplan.FnAny,
				Args: []chplan.Expr{&chplan.MapAccess{
					Map: &chplan.ColumnRef{Name: inheritanceAttrColumn},
					Key: &chplan.LitString{V: "k"},
				}},
				Alias: "__root_attr",
			}},
		},
	}
	rw := &chplan.RangeWindow{
		Input:           m,
		Range:           time.Minute,
		Step:            time.Minute,
		Start:           time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC),
		TimestampColumn: "Timestamp",
	}

	ctx := WithAttrStrategies(context.Background(), jsonAttrStrategies())
	sql, _, _, err := EmitCompareRootLeg(ctx, rw)
	if err != nil {
		t.Fatalf("EmitCompareRootLeg: %v", err)
	}
	if !renderedAsJSON(sql) {
		t.Fatalf("the compare root leg rendered Map syntax against a JSON-typed column:\n%s", sql)
	}

	plain, _, _, err := EmitCompareRootLeg(context.Background(), rw)
	if err != nil {
		t.Fatalf("EmitCompareRootLeg (no strategies): %v", err)
	}
	if renderedAsJSON(plain) {
		t.Fatalf("the compare root leg rendered the JSON shape with no strategies on the ctx:\n%s", plain)
	}
}
