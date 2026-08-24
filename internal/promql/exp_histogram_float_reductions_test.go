package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_ResetsChangesCountAreFloatValued pins issue
// #1772's last cut: `resets()` / `changes()` over an exp-histogram
// range-vector selector and `count()` over an exp-histogram instant
// selector are answerable, and — unlike every other exp-histogram
// lowering in this package — their answer is an ordinary FLOAT sample.
//
// The row-shape assertion is the load-bearing half. All three reference
// functions return `Sample{F: ...}`, so a plan that capped itself with a
// chplan.HistogramProjection would publish thirteen columns into a
// four-column contract: the wire would render a histogram where Grafana
// expects a number, and every consumer that reads Value would read the
// projection's placeholder instead of the count. That is the single most
// likely way to ship a plausible-looking wrong answer here, because the
// three shapes sit next to three siblings that ARE histogram-valued and
// share their entire window-aggregate stage.
//
// The `__name__` assertion is the same rule its histogram-valued
// siblings pin: reference drops `__name__` from every range-vector
// function result and every aggregation result alike, so the quartet's
// first slot must be an EMPTY literal rather than the MetricName column
// the bare selector carries up.
//
// The "wrapped" queries below pin cerberus issue #2549: resets() /
// changes() / count() / group() over a bare exp-histogram selector used
// to reject the instant the call was itself wrapped by a further scalar
// op, aggregation or instant math function — the wrapper's own operand
// lowering fell back to the generic descent, which hit
// [lowerVectorSelector]'s ordinary rejection on the histogram selector
// underneath, never reaching the recognisers pinned by the bare cases
// above. See [lowerRangeVectorCall] and [lowerExpHistogramCountFamily]'s
// own doc for the fix. The shape assertions below are identical to the
// bare cases' — the wrapper composes with an already float-valued
// result, so nothing about the row shape or the dropped `__name__`
// changes; TestLower_ExpHistogram_ResetsChangesCountNested_ChDB (chdb
// build tag) is what pins the actual numeric answer through the
// wrapper's own arithmetic.
func TestLower_ExpHistogram_ResetsChangesCountAreFloatValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	queries := []string{
		`resets(latency_exp_hist[5m])`,
		`changes(latency_exp_hist[5m])`,
		`resets(latency_exp_hist{service="api"}[10m])`,
		`(changes(latency_exp_hist[5m]))`,
		`resets(latency_exp_hist[5m] offset 10m)`,
		`resets(latency_exp_hist[5m] @ 1767225600)`,
		`count(latency_exp_hist)`,
		`count by (service) (latency_exp_hist)`,
		`count without (pod) (latency_exp_hist{service="api"})`,
		`(count(latency_exp_hist))`,

		// Wrapped shapes (cerberus issue #2549) — this issue's own four
		// trigger queries, plus the count()/group() family the issue's
		// architectural note names alongside them.
		`resets(latency_exp_hist[5m]) * 2`,
		`sum(resets(latency_exp_hist[5m]))`,
		`changes(latency_exp_hist[5m]) + 1`,
		`abs(changes(latency_exp_hist[5m]))`,
		`count(latency_exp_hist) + 1`,
		`count(count(latency_exp_hist))`,
		`group(latency_exp_hist) * 2`,
		`abs(group(latency_exp_hist))`,
		`sum(resets(latency_exp_hist[5m]) * 2)`,
	}
	modes := []struct {
		name  string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{name: "instant", lower: func(e parser.Expr) (chplan.Node, error) {
			return promql.LowerAt(context.Background(), e, s, end, end)
		}},
		{name: "range", lower: func(e parser.Expr) (chplan.Node, error) {
			return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
		}},
	}

	for _, q := range queries {
		for _, mode := range modes {
			t.Run(q+"/"+mode.name, func(t *testing.T) {
				t.Parallel()
				expr, err := p.ParseExpr(q)
				if err != nil {
					t.Fatalf("ParseExpr(%q): %v", q, err)
				}
				plan, err := mode.lower(expr)
				if err != nil {
					t.Fatalf("lower(%q): unexpected error: %v", q, err)
				}
				if shape := chplan.RowShapeOf(plan); shape == chplan.HistogramRowShape {
					t.Fatalf("lower(%q): plan root publishes a HISTOGRAM row shape — "+
						"reference returns Sample{F: ...} for all three of these, so the answer "+
						"is a four-column float sample, not the thirteen-column contract", q)
				}
				proj, ok := plan.(*chplan.Project)
				if !ok {
					t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", q, plan)
				}
				wantAliases := []string{
					s.MetricNameColumn, s.AttributesColumn, s.TimestampColumn, s.ValueColumn,
				}
				if len(proj.Projections) != len(wantAliases) {
					t.Fatalf("lower(%q): root publishes %d columns, want the %d-column sample quartet",
						q, len(proj.Projections), len(wantAliases))
				}
				for i, want := range wantAliases {
					if got := projectionColumnName(proj.Projections[i]); got != want {
						t.Fatalf("lower(%q): output column %d is %q, want %q", q, i, got, want)
					}
				}
				name, ok := proj.Projections[0].Expr.(*chplan.LitString)
				if !ok || name.V != "" {
					t.Fatalf("lower(%q): __name__ projection is %#v, want an empty literal — "+
						"reference drops __name__ from a range-vector function and an aggregation alike",
						q, proj.Projections[0].Expr)
				}
				if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
					t.Fatalf("Emit(%q): %v", q, err)
				}
			})
		}
	}
}

// projectionColumnName reports the SQL-visible name of a projection: its
// own Alias when the lowering set one explicitly, or the underlying
// ColumnRef's Name when it did not. A pass-through projection built by
// [guardedValueProjection] (the scalar-binop / instant-fn wrapper path a
// [chplan.Project] taking this shortcut) leaves Alias empty for a column
// it forwards unchanged — the emitter's own SQL still names the output
// column after the ref, so an empty Alias is not a missing column, only
// an unaliased one. The dedicated exp-histogram Projects this test also
// covers (expHistogramPairCountProjection, lowerExpHistogramCount's own
// projection) always set Alias explicitly, so this fallback never masks
// a genuinely wrong column there.
func projectionColumnName(p chplan.Projection) string {
	if p.Alias != "" {
		return p.Alias
	}
	if ref, ok := p.Expr.(*chplan.ColumnRef); ok {
		return ref.Name
	}
	return ""
}

// TestLower_ExpHistogram_ResetsAndChangesUseDifferentKernels pins the one
// thing a structural assertion cannot see and the compat corpus would
// only catch on a fixture that happens to distinguish them: `resets()`
// and `changes()` ask DIFFERENT questions of the same samples.
//
// `resets()` transcribes FloatHistogram.DetectReset — a directional
// REGRESSION test, so its comparisons are `<` (and the schema one `>`,
// since only a resolution increase implies a restart). `changes()`
// transcribes !FloatHistogram.Equals — structural whole-row INEQUALITY,
// so its comparisons are `!=` across every stored field.
//
// Swapping the two kernels is the defect this catches, and it is a
// plausible one precisely because both reduce a per-pair mask with the
// same arraySum over the same time-ordered positions. It would be
// invisible to the shape assertions above, and on a monotonically
// growing counter — the common case — `changes()` answered with the
// reset kernel returns 0 where reference returns the sample count.
//
// Sum is the sharpest single discriminator and is asserted both ways:
// reference's DetectReset deliberately does NOT read Sum ("in any
// bucket, in the zero count, or in the count of observations, but NOT
// the sum of observations") while Equals does, so the Sum list must
// appear in exactly one of the two emitted queries.
func TestLower_ExpHistogram_ResetsAndChangesUseDifferentKernels(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	emit := func(t *testing.T, query string) string {
		t.Helper()
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
		if err != nil {
			t.Fatalf("LowerAt(%q): %v", query, err)
		}
		sql, _, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit(%q): %v", query, err)
		}
		return sql
	}

	// The emitter's own spelling of hqWindowSumArrayAlias, which is
	// unexported and so cannot be referenced from this external test
	// package. Pinning the literal is the point: it is the column the
	// two kernels must disagree about.
	const sumListColumn = "`_hq_sum_list`"

	resetsSQL := emit(t, `resets(latency_exp_hist[5m])`)
	changesSQL := emit(t, `changes(latency_exp_hist[5m])`)

	// Both reduce a per-pair mask the same way, over the same pairing.
	for _, want := range []string{"arraySum(", "arrayPopBack(", "arrayPopFront(", "arraySort("} {
		if !strings.Contains(resetsSQL, want) {
			t.Fatalf("resets() SQL is missing %q — it must reduce a per-pair mask:\n%s", want, resetsSQL)
		}
		if !strings.Contains(changesSQL, want) {
			t.Fatalf("changes() SQL is missing %q — it must reduce a per-pair mask:\n%s", want, changesSQL)
		}
	}

	// resets() is the DetectReset kernel: a bucket-ladder regression walk
	// (arrayExists over the rescaled ladders) and no equality anywhere.
	if !strings.Contains(resetsSQL, "arrayExists(") {
		t.Fatalf("resets() SQL never walks the bucket ladders for a regression — "+
			"DetectReset condemns a pair when ANY bucket falls:\n%s", resetsSQL)
	}
	// The verdict's DIRECTION, read off the Count comparison specifically.
	// A blanket scan for "!=" would be meaningless here: the attribute
	// canonicalisation every query carries renders `v != ?`, so the token
	// is present in both SQLs no matter which kernel ran. The two lambdas
	// bind different parameter names (ra/rb vs ca/cb), so these two
	// substrings cannot collide.
	if !strings.Contains(resetsSQL, "`_hq_count_list`[rb] < `_hq_count_list`[ra]") {
		t.Fatalf("resets() does not compare Count as a REGRESSION (`<`) — that is "+
			"DetectReset's first condition:\n%s", resetsSQL)
	}
	if strings.Contains(resetsSQL, "`_hq_count_list`[rb] != `_hq_count_list`[ra]") {
		t.Fatalf("resets() compares Count for INEQUALITY — DetectReset is directional; "+
			"this is changes()'s kernel:\n%s", resetsSQL)
	}
	if strings.Contains(resetsSQL, sumListColumn) {
		t.Fatalf("resets() SQL reads the Sum list — reference's DetectReset deliberately does "+
			"NOT let Sum vote on a reset:\n%s", resetsSQL)
	}

	// changes() is the !Equals kernel: structural inequality over every
	// stored field, Sum included, and no regression walk at all.
	if !strings.Contains(changesSQL, "`_hq_count_list`[cb] != `_hq_count_list`[ca]") {
		t.Fatalf("changes() does not compare Count for INEQUALITY — !Equals is structural "+
			"whole-row inequality:\n%s", changesSQL)
	}
	if strings.Contains(changesSQL, "`_hq_count_list`[cb] < `_hq_count_list`[ca]") {
		t.Fatalf("changes() compares Count as a regression (`<`) — that is resets()'s "+
			"kernel; a histogram that merely GROWS is a change:\n%s", changesSQL)
	}
	if !strings.Contains(changesSQL, sumListColumn) {
		t.Fatalf("changes() SQL never reads the Sum list — FloatHistogram.Equals compares Sum, "+
			"so two samples differing only in Sum are a change:\n%s", changesSQL)
	}
	if strings.Contains(changesSQL, "arrayExists(") {
		t.Fatalf("changes() SQL walks the ladders with arrayExists — that is DetectReset's "+
			"per-bucket regression search, not an equality test:\n%s", changesSQL)
	}
	// The both-NaN carve-out: reference's Equals compares Sum by BIT
	// PATTERN, under which two NaNs are equal, while ClickHouse's `!=`
	// follows IEEE-754 and would report a change at every sample of a
	// NaN-valued series.
	if !strings.Contains(changesSQL, "isNaN(") {
		t.Fatalf("changes() SQL has no both-NaN carve-out on Sum — Equals compares bit "+
			"patterns, so a NaN Sum equals itself:\n%s", changesSQL)
	}
}
