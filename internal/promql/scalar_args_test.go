package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerScalarArg_TimePerStep pins #1455 (half b): a `time()` call
// nested inside another function's computed-scalar argument
// (`lowerScalarArg`'s "time" case in scalar_args.go) must bind per
// range-query step rather than once for the whole statement.
//
// Before the fix, lowerScalarArg's "time" branch always resolved the
// eval anchor to the request's fixed `ctx.end` — a single SQL literal
// shared by every row, so `round(v, time())` and `clamp_max(v, time())`
// used the SAME divisor/bound at every step of a range query instead of
// the step's own timestamp. lowerTime (internal/promql/synthetic.go),
// the sibling used for a bare top-level `time()`, already got this
// right in range mode via chplan.NowNano() + rewriteAnchorRefs — but
// that rewrite only fires inside syntheticScalarVector, which
// lowerScalarArg's nested embedding sites never go through. The fix
// reads the row's own TimeUnix column instead, which the Sample
// contract guarantees equals anchor_ts on every row in range mode.
func TestLowerScalarArg_TimePerStep(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := time.Minute

	for _, q := range []string{
		`round(http_requests_total, time())`,
		`clamp_max(http_requests_total, time())`,
		`round(rate(http_requests_total[5m]), time())`,
		`clamp_max(rate(http_requests_total[5m]), time())`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, step)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): %v", q, err)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", q, err)
			}
			// The computed time() anchor must read the row's own
			// TimeUnix column (per-step, matches the Sample contract's
			// TimeUnix == anchor_ts invariant in range mode) — NOT a
			// single toDateTime64(...) literal shared by every row.
			const perStepAnchor = "toUnixTimestamp64Nano(`TimeUnix`)"
			if !strings.Contains(sql, perStepAnchor) {
				t.Fatalf("query=%q: emitted SQL does not reference the per-step anchor %q; got:\n%s",
					q, perStepAnchor, sql)
			}
		})
	}
}

// TestLowerScalarArg_TimeInstantUnchanged pins the instant-mode (ctx.step
// == 0) shape of lowerScalarArg's "time" case: it must keep resolving to
// the fixed eval-anchor literal, exactly as before the #1455 half-b fix,
// so every existing instant-query golden stays byte-identical.
func TestLowerScalarArg_TimeInstantUnchanged(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	const q = `round(http_requests_total, time())`
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower(%q): %v", q, err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", q, err)
	}
	if strings.Contains(sql, "toUnixTimestamp64Nano(`TimeUnix`)") {
		t.Fatalf("query=%q: instant-mode SQL unexpectedly references a per-row TimeUnix anchor; got:\n%s", q, sql)
	}
	// Lower (no explicit end) anchors to now64(9); the fixed-anchor shape
	// is what matters here, not which literal it resolves to.
	if !strings.Contains(sql, "toUnixTimestamp64Nano(now64(") {
		t.Fatalf("query=%q: instant-mode SQL should still resolve time() to the fixed now64(9) eval anchor; got:\n%s", q, sql)
	}
}
