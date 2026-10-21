package chsql

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// secondsStrategies / nanosStrategies render a statement for metrics
// timestamp columns coarser than nanoseconds / of DateTime64(9).
var (
	secondsStrategies = SchemaStrategies{MetricsTimestamp: schema.MetricsTimestampSeconds}
	nanosStrategies   = SchemaStrategies{MetricsTimestamp: schema.MetricsTimestampNanos}
)

// TestUnixTimestamp64_RendersPerPrecision pins both renderings of the
// DateTime64-only timestamp functions: the column directly under the zero
// value (the SQL every DateTime64(9) deployment has always run), and the
// column widened through toDateTime64(…, 9) under seconds.
func TestUnixTimestamp64_RendersPerPrecision(t *testing.T) {
	cases := []struct {
		name    string
		frag    Frag
		nanos   string
		seconds string
	}{
		{"nanos", ToUnixNanos(Col("TimeUnix")), "toUnixTimestamp64Nano(`TimeUnix`)", "toUnixTimestamp64Nano(toDateTime64(`TimeUnix`, 9))"},
		{"millis", ToUnixMillis(Col("TimeUnix")), "toUnixTimestamp64Milli(`TimeUnix`)", "toUnixTimestamp64Milli(toDateTime64(`TimeUnix`, 9))"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderFrag(c.frag); got != c.nanos {
				t.Errorf("zero strategies = %q, want %q", got, c.nanos)
			}
			if got := renderFrag(UnderSchemaStrategies(nanosStrategies, c.frag)); got != c.nanos {
				t.Errorf("nanos = %q, want %q", got, c.nanos)
			}
			if got := renderFrag(UnderSchemaStrategies(secondsStrategies, c.frag)); got != c.seconds {
				t.Errorf("seconds = %q, want %q", got, c.seconds)
			}
		})
	}
}

// TestUnixTimestamp64_NameResolvesThroughFnResolutions pins that the
// function name every rendering spells comes from the fnResolutions table,
// the one resolution of chplan.Fn to ClickHouse: the Frag constructors and
// the chplan path both read it.
func TestUnixTimestamp64_NameResolvesThroughFnResolutions(t *testing.T) {
	const renamed = "resolvedThroughTheTable"
	for _, fn := range []chplan.Fn{chplan.FnToUnixNanos, chplan.FnToUnixMillis} {
		orig := fnResolutions[fn]
		fnResolutions[fn] = fnResolution{Name: renamed}
		frag := ToUnixNanos(Col("TimeUnix"))
		if fn == chplan.FnToUnixMillis {
			frag = ToUnixMillis(Col("TimeUnix"))
		}
		expr := &chplan.FuncCall{Fn: fn, Args: []chplan.Expr{&chplan.ColumnRef{Name: "TimeUnix"}}}
		viaPlan := renderFrag(func(b *Builder) { _ = b.Expr(expr) })
		viaFrag := renderFrag(frag)
		fnResolutions[fn] = orig
		want := renamed + "(`TimeUnix`)"
		if viaFrag != want || viaPlan != want {
			t.Errorf("%s: Frag rendered %q, plan rendered %q; want both %q", fn, viaFrag, viaPlan, want)
		}
	}
}

// TestUnixTimestamp64_SubqueryInheritsStrategies pins that an inner
// QueryBuilder an emitter composes by hand renders under the enclosing
// statement's strategies: they belong to the statement, so a timestamp
// function one subquery deep must widen too.
func TestUnixTimestamp64_SubqueryInheritsStrategies(t *testing.T) {
	inner := NewQuery().SelectAs(ToUnixNanos(Col("TimeUnix")), "ts").From(Col("otel_metrics_gauge"))
	const widened = "toUnixTimestamp64Nano(toDateTime64(`TimeUnix`, 9))"
	for name, wrap := range map[string]func(Subqueryable) Frag{"Subquery": Subquery, "Spliced": Spliced} {
		if got := renderFrag(UnderSchemaStrategies(secondsStrategies, wrap(inner))); !strings.Contains(got, widened) {
			t.Errorf("%s = %q, want it to contain %q", name, got, widened)
		}
	}
	if got := renderFrag(UnderSchemaStrategies(secondsStrategies, inner.Frag())); !strings.Contains(got, widened) {
		t.Errorf("Frag = %q, want it to contain %q", got, widened)
	}
	// Rendering it on its own afterwards uses the zero value: an embedding
	// leaves nothing behind on the shared QueryBuilder.
	if got := renderFrag(Subquery(inner)); strings.Contains(got, "toDateTime64") {
		t.Errorf("inner builder kept the enclosing strategies: %q", got)
	}
}

// TestUnixTimestamp64_ExplicitStrategiesWinInsideAnyStatement pins that a
// QueryBuilder carrying its own strategies renders under them wherever it
// is embedded — including the nanos zero value inside a seconds statement,
// which no "is the inner value the zero value" test can tell apart from
// carrying none.
func TestUnixTimestamp64_ExplicitStrategiesWinInsideAnyStatement(t *testing.T) {
	inner := NewQuery().SelectAs(ToUnixNanos(Col("TimeUnix")), "ts").From(Col("otel_metrics_gauge")).
		WithSchemaStrategies(nanosStrategies)
	for name, frag := range map[string]Frag{"Subquery": Subquery(inner), "Spliced": Spliced(inner), "Frag": inner.Frag()} {
		if got := renderFrag(UnderSchemaStrategies(secondsStrategies, frag)); strings.Contains(got, "toDateTime64") {
			t.Errorf("%s: a nanos statement embedded in a seconds one widened: %q", name, got)
		}
	}
}

// TestUnixTimestamp64_OneBuilderEmbeddedUnderTwoStrategies pins that a
// QueryBuilder shared by two statements rendering under different
// strategies renders each under its own even when one render runs inside
// the other: rendering reads the QueryBuilder and never writes the
// strategies onto it, so a render in progress leaves nothing for the
// nested one to pick up.
func TestUnixTimestamp64_OneBuilderEmbeddedUnderTwoStrategies(t *testing.T) {
	inner := NewQuery().SelectAs(ToUnixNanos(Col("TimeUnix")), "ts").From(Col("otel_metrics_gauge"))
	var nanosRender string
	nested := false
	// While the seconds statement is rendering inner, inner's own WHERE
	// renders inner again inside a nanos statement.
	inner.Where(func(b *Builder) {
		if !nested {
			nested = true
			nanosRender = renderFrag(UnderSchemaStrategies(nanosStrategies, Subquery(inner)))
		}
		InlineLit(int64(1))(b)
	})
	seconds := renderFrag(UnderSchemaStrategies(secondsStrategies, Subquery(inner)))
	if !nested {
		t.Fatal("the nested render never ran; the test proves nothing")
	}
	if !strings.Contains(seconds, "toDateTime64(`TimeUnix`, 9)") {
		t.Errorf("seconds statement = %q, want the widened read", seconds)
	}
	if strings.Contains(nanosRender, "toDateTime64") {
		t.Errorf("nanos statement rendered inside the seconds one = %q, want the direct read", nanosRender)
	}
}

// TestExprUnixTimestamp64_PlanFunctionsFollowContext pins the chplan path:
// a FnToUnixNanos / FnToUnixMillis call in a plan renders under the
// strategies chsql.Emit reads off the context.
func TestExprUnixTimestamp64_PlanFunctionsFollowContext(t *testing.T) {
	for _, fn := range []chplan.Fn{chplan.FnToUnixNanos, chplan.FnToUnixMillis} {
		expr := &chplan.FuncCall{Fn: fn, Args: []chplan.Expr{&chplan.ColumnRef{Name: "TimeUnix"}}}
		frag := func(b *Builder) { _ = b.Expr(expr) }
		want := string(fn) + "(`TimeUnix`)"
		if got := renderFrag(frag); got != want {
			t.Errorf("%s zero strategies = %q, want %q", fn, got, want)
		}
		want = string(fn) + "(toDateTime64(`TimeUnix`, 9))"
		if got := renderFrag(UnderSchemaStrategies(secondsStrategies, frag)); got != want {
			t.Errorf("%s seconds = %q, want %q", fn, got, want)
		}
	}
	ctx := WithSchemaStrategies(context.Background(), secondsStrategies)
	if got := schemaStrategiesFromCtx(ctx); got.MetricsTimestamp != schema.MetricsTimestampSeconds {
		t.Errorf("schemaStrategiesFromCtx = %v, want seconds", got)
	}
	if got := schemaStrategiesFromCtx(context.Background()); got.MetricsTimestamp != schema.MetricsTimestampNanos || got.Attrs != nil {
		t.Errorf("schemaStrategiesFromCtx(no value) = %v, want the zero value", got)
	}
}

// TestSchemaStrategiesCell pins the cell every head reads per request: a
// nil cell and a cell never stored load the zero value, and a Store is what
// every later Load returns.
func TestSchemaStrategiesCell(t *testing.T) {
	var nilCell *SchemaStrategiesCell
	if got := nilCell.Load(); got.Metrics.MetricsTimestamp != schema.MetricsTimestampNanos || got.Logs.Attrs != nil {
		t.Errorf("nil cell = %v, want the zero value", got)
	}
	if got := (&SchemaStrategiesCell{}).Load(); got.Traces.Attrs != nil {
		t.Errorf("unstored cell = %v, want the zero value", got)
	}
	logs := AttrStrategies{"LogAttributes": AttrStrategyJSON}
	c := NewSchemaStrategiesCell(ResolvedSchemaStrategies{Metrics: secondsStrategies})
	if got := c.Load(); got.Metrics.MetricsTimestamp != schema.MetricsTimestampSeconds {
		t.Errorf("Load = %v, want seconds metrics", got)
	}
	c.Store(ResolvedSchemaStrategies{Logs: SchemaStrategies{Attrs: logs}})
	got := c.Load()
	if got.Metrics.MetricsTimestamp != schema.MetricsTimestampNanos || got.Logs.Attrs.Lookup("LogAttributes") != AttrStrategyJSON {
		t.Errorf("Load after Store = %v, want the stored value", got)
	}
}

// TestTimestampFunctions_NoDirectCalls guards the reach of the metrics
// timestamp rendering and its one spelling of each function: every emitter
// call site renders the DateTime64-only timestamp functions through
// ToUnixNanos / ToUnixMillis (or a chplan call exprUnixTimestamp64
// renders), never through a hand-written Call, which would render the bare
// function over a DateTime column and fail every query on that schema; and
// every `toDateTime64(<x>, 9)` through ToDateTime64Nanos. A function name
// reaches a Call either as a string literal or through the chplan.Fn
// constant whose value it is, so both spellings are owned: fnresolution.go
// names the timestamp functions in the resolution table and
// metrics_timestamp.go renders them from their chplan.Fn, and
// metrics_timestamp.go alone names toDateTime64.
func TestTimestampFunctions_NoDirectCalls(t *testing.T) {
	const resolution, rendering = "fnresolution.go", "metrics_timestamp.go"
	spellings := map[string][]string{
		`"toUnixTimestamp64`:    {resolution},
		`chplan.FnToUnixNanos`:  {resolution, rendering},
		`chplan.FnToUnixMillis`: {resolution, rendering},
		`"toDateTime64"`:        {rendering},
		`toDateTime64Fn`:        {rendering},
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			code, _, _ := strings.Cut(line, "//")
			for spelling, owners := range spellings {
				if !slices.Contains(owners, f) && strings.Contains(code, spelling) {
					t.Errorf("%s:%d spells %s directly; render it through the helpers %v define: %s",
						f, i+1, spelling, owners, strings.TrimSpace(line))
				}
			}
		}
	}
}
