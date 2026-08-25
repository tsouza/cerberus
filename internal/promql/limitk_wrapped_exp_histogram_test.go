package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_LimitKAndLimitRatioComposeUnderFloatOnlyWrappers
// pins cerberus issue #2575: `limitk(K, <exp-hist selector>)` /
// `limit_ratio(R, <exp-hist selector>)`, wrapped by a further float-only
// function or arithmetic operator, used to PANIC inside
// [chplan.RowShapeOf]'s consumer histogram_shape_guard.go's
// assertValueShapedInput rather than either computing correctly or
// cleanly rejecting.
//
// Root cause: [lowerLimitKInput] (cerberus issue #2518) already recognises
// and PRESERVES a histogram-valued operand to limitk/limit_ratio, but
// neither `isExpHistogramValuedShape` nor `isExpHistogramDroppingShape`
// (histogram_native_scalar_binop.go / histogram_native_float_fn.go /
// histogram_native_dropping_shape.go) had a limitk/limit_ratio entry, so a
// float-only wrapper's own opt-in
// ([lowerExpHistogramArgAsCanonicalFloat]) never recognised the operand as
// histogram-shaped and fell through to the generic `lower()` dispatcher —
// which produced the SAME histogram-shaped plan anyway, just without the
// wrapper's own postprocessing ever expecting it.
//
// Reference Prometheus's float-only functions (abs/ceil/sqrt/…) and plain
// arithmetic silently DROP a native-histogram sample rather than erroring
// (see issue #2221's own "float-only functions" family) — so the fix
// shape is the same "preserve-family recognizer, consumed and dropped by
// the wrapper" pattern every other wrapper in
// internal/promql/histogram_native_*.go already gets. This test covers
// that DROP-family half; `label_replace`/`label_join` and `timestamp()`
// — two of the issue's own named reproducers — are NOT float-only/drop
// wrappers (they preserve the payload or answer a real derived value
// respectively) and get their own tests just below.
func TestLower_ExpHistogram_LimitKAndLimitRatioComposeUnderFloatOnlyWrappers(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	queries := []string{
		// The issue's own trigger query, plus every other float-only
		// wrapper the issue names.
		`abs(limitk(2, latency_exp_hist))`,
		`ceil(limitk(2, latency_exp_hist))`,
		`sqrt(limitk(2, latency_exp_hist))`,
		`round(limitk(2, latency_exp_hist))`,
		`exp(limitk(2, latency_exp_hist))`,
		`ln(limitk(2, latency_exp_hist))`,
		`log2(limitk(2, latency_exp_hist))`,
		`log10(limitk(2, latency_exp_hist))`,
		`clamp(limitk(2, latency_exp_hist), 0, 1)`,
		`clamp_min(limitk(2, latency_exp_hist), 0)`,
		`clamp_max(limitk(2, latency_exp_hist), 1)`,
		// Plain arithmetic.
		`limitk(2, latency_exp_hist) + 0`,
		// limit_ratio reproduces identically.
		`abs(limit_ratio(0.5, latency_exp_hist))`,
		`limit_ratio(0.5, latency_exp_hist) + 0`,
		// Computed-K / by/without partitioning must compose too — these
		// route limitk's input through [lowerTopKComputed] rather than
		// [lowerLimitK] directly, but both share [lowerLimitKInput].
		`abs(limitk(scalar(up), latency_exp_hist))`,
		`abs(limitk(2, latency_exp_hist) by (job))`,
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr := parseExprExp(t, query)
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error (this exact shape hard-rejected or panicking before cerberus issue #2575's fix): %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("LowerAt(%q) row shape = %s, want sample (canonical float, dropped) — a float-only wrapper must drop the histogram, not forward it", query, shape)
			}
			// The plan must actually be empty (a constant-false Filter
			// reachable somewhere in the tree), matching reference's
			// float-only-function drop semantics — not merely
			// float-shaped by accident.
			foundEmptyFilter := false
			chplan.Walk(plan, func(n chplan.Node) bool {
				if f, ok := n.(*chplan.Filter); ok {
					if lit, ok := f.Predicate.(*chplan.LitBool); ok && !lit.V {
						foundEmptyFilter = true
					}
				}
				return true
			})
			if !foundEmptyFilter {
				t.Fatalf("LowerAt(%q) plan has no constant-false Filter; want a dropped (empty) result:\n%#v", query, plan)
			}
		})
	}
}

// TestLower_ExpHistogram_LimitKUnderLabelReplaceStillPreserves pins that
// `label_replace`/`label_join` wrapping `limitk`/`limit_ratio` over an
// exp-histogram operand — one of the issue's own named reproducers, via
// `projectAttributesOverInner` — is a PRESERVE-family wrapper, not a
// float-only DROP-family one: reference's evalLabelReplace/evalLabelJoin
// rewrite only the series labels and carry the histogram sample through
// unchanged (see [labelCallOverExpHistogram]'s own doc). So unlike
// abs()/ceil()/…, the fix does not reproject to an empty float vector —
// it routes through [lowerLabelCallOverExpHistogram] the exact way it
// already does for a bare histogram selector, keeping the row
// histogram-shaped with rewritten Attributes.
func TestLower_ExpHistogram_LimitKUnderLabelReplaceStillPreserves(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	for _, query := range []string{
		`label_replace(limitk(2, latency_exp_hist), "dst", "$1", "job", "(.*)")`,
		`label_replace(limit_ratio(0.5, latency_exp_hist), "dst", "$1", "job", "(.*)")`,
	} {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr := parseExprExp(t, query)
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error (this exact shape panicked via projectAttributesOverInner before cerberus issue #2575's fix): %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("LowerAt(%q) plan publishes %s, want histogram — label_replace/label_join must PRESERVE the payload, not drop it", query, shape)
			}
		})
	}
}

// TestLower_ExpHistogram_LimitKUnderTimestampStillAnswers pins that
// `timestamp(limitk(...))` — another of the issue's own named
// reproducers — is neither a drop-family wrapper NOR a preserve-family
// one: reference's funcTimestamp reads only the selected sample's
// Point.T, answering identically whether the sample is float- or
// histogram-valued (see histogram_native_timestamp.go's own doc). So the
// fix must not reproject this to an EMPTY result the way abs()/ceil()/…
// do — [lowerTimestampOverExpHistogram] already handles any
// histogram-valued shape via [projectExpHistogramEvalInstant], and with
// this issue's fix that dispatch now reaches `limitk`/`limit_ratio` too,
// answering a genuine (non-empty) float timestamp value per surviving
// row rather than panicking or dropping every row.
func TestLower_ExpHistogram_LimitKUnderTimestampStillAnswers(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	for _, query := range []string{
		`timestamp(limitk(2, latency_exp_hist))`,
		`timestamp(limit_ratio(0.5, latency_exp_hist))`,
	} {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr := parseExprExp(t, query)
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error (this exact shape panicked via projectValueOverInner before cerberus issue #2575's fix): %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("LowerAt(%q) row shape = %s, want sample (timestamp() always answers a plain float value)", query, shape)
			}
			// Must NOT be a constant-false-filtered (dropped) result:
			// timestamp() answers a real value for every surviving row,
			// unlike the float-only DROP family above.
			foundEmptyFilter := false
			chplan.Walk(plan, func(n chplan.Node) bool {
				if f, ok := n.(*chplan.Filter); ok {
					if lit, ok := f.Predicate.(*chplan.LitBool); ok && !lit.V {
						foundEmptyFilter = true
					}
				}
				return true
			})
			if foundEmptyFilter {
				t.Fatalf("LowerAt(%q) plan has a constant-false Filter; timestamp() must answer real rows, not drop them", query)
			}
		})
	}
}

// TestLower_ExpHistogram_LimitKUnwrappedStillPreserves is a regression
// guard sitting alongside the wrapped-composition fix above: the
// UNWRAPPED case cerberus issue #2518 shipped —
// `limitk(K, <exp-hist selector>)` evaluated on its own, with no further
// wrapper — must keep answering histogram-shaped, not regress to the
// wrapped-and-dropped behaviour the new recognizer adds for wrappers.
// TestLower_ExpHistogram_LimitKAndLimitRatioPreserveSamples
// (limitk_exp_histogram_test.go) already pins this at length; this is a
// narrow sanity check living next to the new wrapped-composition test so
// the two behaviours are visibly contrasted in one file.
func TestLower_ExpHistogram_LimitKUnwrappedStillPreserves(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	for _, query := range []string{
		`limitk(2, latency_exp_hist)`,
		`limit_ratio(0.5, latency_exp_hist)`,
	} {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr := parseExprExp(t, query)
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("LowerAt(%q) plan publishes %s, want histogram — unwrapped limitk/limit_ratio must keep preserving the sample (cerberus issue #2518 must not regress)", query, shape)
			}
		})
	}
}
