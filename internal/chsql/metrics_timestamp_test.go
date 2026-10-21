package chsql

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestUnixTimestamp64_RendersPerStrategy pins both renderings of the
// DateTime64-only timestamp functions: the column directly under the zero
// strategy (the SQL every DateTime64(9) deployment has always run), and the
// column widened through toDateTime64(…, 9) under MetricsTimestampDateTime.
func TestUnixTimestamp64_RendersPerStrategy(t *testing.T) {
	cases := []struct {
		name     string
		frag     Frag
		dt64     string
		dateTime string
	}{
		{"nanos", ToUnixNanos(Col("TimeUnix")), "toUnixTimestamp64Nano(`TimeUnix`)", "toUnixTimestamp64Nano(toDateTime64(`TimeUnix`, 9))"},
		{"millis", ToUnixMillis(Col("TimeUnix")), "toUnixTimestamp64Milli(`TimeUnix`)", "toUnixTimestamp64Milli(toDateTime64(`TimeUnix`, 9))"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderFrag(c.frag); got != c.dt64 {
				t.Errorf("zero strategy = %q, want %q", got, c.dt64)
			}
			if got := renderFrag(UnderMetricsTimestamp(MetricsTimestampDateTime64, c.frag)); got != c.dt64 {
				t.Errorf("DateTime64 strategy = %q, want %q", got, c.dt64)
			}
			if got := renderFrag(UnderMetricsTimestamp(MetricsTimestampDateTime, c.frag)); got != c.dateTime {
				t.Errorf("DateTime strategy = %q, want %q", got, c.dateTime)
			}
		})
	}
}

// TestUnixTimestamp64_SubqueryInheritsStrategy pins that an inner
// QueryBuilder an emitter composes by hand renders under the enclosing
// statement's strategy: the strategy belongs to the statement, so a
// timestamp function one subquery deep must widen too.
func TestUnixTimestamp64_SubqueryInheritsStrategy(t *testing.T) {
	inner := NewQuery().SelectAs(ToUnixNanos(Col("TimeUnix")), "ts").From(Col("otel_metrics_gauge"))
	got := renderFrag(UnderMetricsTimestamp(MetricsTimestampDateTime, Subquery(inner)))
	if want := "toUnixTimestamp64Nano(toDateTime64(`TimeUnix`, 9))"; !strings.Contains(got, want) {
		t.Errorf("subquery = %q, want it to contain %q", got, want)
	}
	// The inner builder is left as it was: rendering it again on its own
	// uses the zero strategy.
	if got := renderFrag(Subquery(inner)); strings.Contains(got, "toDateTime64") {
		t.Errorf("inner builder kept the borrowed strategy: %q", got)
	}
}

// TestExprUnixTimestamp64_PlanFunctionsFollowContext pins the chplan path:
// a FnToUnixNanos / FnToUnixMillis call in a plan renders through the
// strategy chsql.Emit reads off the context.
func TestExprUnixTimestamp64_PlanFunctionsFollowContext(t *testing.T) {
	for _, fn := range []chplan.Fn{chplan.FnToUnixNanos, chplan.FnToUnixMillis} {
		expr := &chplan.FuncCall{Fn: fn, Args: []chplan.Expr{&chplan.ColumnRef{Name: "TimeUnix"}}}
		frag := func(b *Builder) { _ = b.Expr(expr) }
		want := string(fn) + "(`TimeUnix`)"
		if got := renderFrag(frag); got != want {
			t.Errorf("%s zero strategy = %q, want %q", fn, got, want)
		}
		want = string(fn) + "(toDateTime64(`TimeUnix`, 9))"
		if got := renderFrag(UnderMetricsTimestamp(MetricsTimestampDateTime, frag)); got != want {
			t.Errorf("%s DateTime strategy = %q, want %q", fn, got, want)
		}
	}
	ctx := WithMetricsTimestamp(context.Background(), MetricsTimestampDateTime)
	if got := metricsTimestampFromCtx(ctx); got != MetricsTimestampDateTime {
		t.Errorf("metricsTimestampFromCtx = %v, want DateTime", got)
	}
	if got := metricsTimestampFromCtx(context.Background()); got != MetricsTimestampDateTime64 {
		t.Errorf("metricsTimestampFromCtx(no value) = %v, want the zero strategy", got)
	}
}

// TestUnixTimestamp64_NoDirectCalls guards the strategy's reach: every
// emitter call site renders the DateTime64-only timestamp functions through
// ToUnixNanos / ToUnixMillis (or a chplan call exprUnixTimestamp64
// renders), never through a hand-written Call, which would render the bare
// function over a DateTime column and fail every query on that schema.
// fnresolution.go names the functions in the resolution table and is the
// one file allowed to spell them.
func TestUnixTimestamp64_NoDirectCalls(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "fnresolution.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if strings.Contains(code, `"toUnixTimestamp64`) {
				t.Errorf("%s:%d spells a toUnixTimestamp64 function directly; use ToUnixNanos / ToUnixMillis: %s",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
