package chplan

// TraceIDTsEndPadSeconds compensates the trace_id_ts lookup MV storing End
// as a DateTime (1-second precision) computed as `max(Timestamp)` over
// spans whose Timestamp is DateTime64(9). The DateTime64->DateTime cast
// floors toward second granularity, so End is the second-truncated max; a
// naive `Timestamp <= End` upper bound would drop every span in the
// trace's final fractional second. Padding the bound by exactly one
// second makes it a strict superset of the true max Timestamp (the
// dropped sub-second carry is < 1s), so no in-cohort span can fall above
// the window. The value is the DateTime-vs-DateTime64 granularity, not a
// tuning knob.
const TraceIDTsEndPadSeconds = 1

// TraceIDTsBounds builds the correctness-preserving Timestamp-window
// SUPERSET bounds (lo, hi) read from a `trace_id_ts` lookup MV, scoped to
// cohortPred (the predicate selecting the trace id(s) of interest — a
// single-trace `TraceId = id` Eq, or a multi-trace `TraceId IN (seed)`
// InSubquery; callers own that choice and its own correctness). cohortPred
// is a factory, called once per bound, so lo and hi each embed their own
// Expr instance rather than aliasing one node from two places in the plan
// tree (see CloneNode's no-shared-mutable-state contract). It AND-folds
// neither bound itself — the caller decides whether the two returned
// Exprs are conjoined together or with other predicates:
//
//	timestampColumn >= (SELECT min(startColumn) FROM table WHERE cohortPred())
//	timestampColumn <= addSeconds((SELECT max(endColumn) FROM table WHERE cohortPred()), TraceIDTsEndPadSeconds)
//
// The lower bound is inherently safe: startColumn is already floored
// downward at MV-population time. The upper bound needs the pad (see
// TraceIDTsEndPadSeconds). Each bound is a scalar subquery over a
// no-GROUP Aggregate, which projects exactly one column and one row —
// satisfying ScalarSubquery's contract.
//
// Hoisted from two independent copies (internal/chsql's
// rootLookupTraceIDTsBounds and internal/api/tempo's traceIDTsWindow,
// see #1438) that differed only in cohort predicate — the correctness-
// bearing pad constant had drifted into two places that had to be kept
// in lock-step by hand.
func TraceIDTsBounds(table, startColumn, endColumn, timestampColumn string, cohortPred func() Expr) (lo, hi Expr) {
	scalar := func(agg, column string) Expr {
		return &ScalarSubquery{Input: &Aggregate{
			Input: &Filter{
				Input:     &Scan{Table: table},
				Predicate: cohortPred(),
			},
			AggFuncs: []AggFunc{{
				Name:  agg,
				Args:  []Expr{&ColumnRef{Name: column}},
				Alias: column,
			}},
		}}
	}
	lo = &Binary{
		Op:    OpGe,
		Left:  &ColumnRef{Name: timestampColumn},
		Right: scalar("min", startColumn),
	}
	hi = &Binary{
		Op:   OpLe,
		Left: &ColumnRef{Name: timestampColumn},
		Right: &FuncCall{Name: "addSeconds", Args: []Expr{
			scalar("max", endColumn),
			&LitInt{V: TraceIDTsEndPadSeconds},
		}},
	}
	return lo, hi
}

// TraceIDTsEnvelopeAlias names the ClickHouse scalar binding that
// [TraceIDTsEnvelopeBounds] reads its two bounds from: one
// `(min(startColumn), max(endColumn))` tuple over the cohort's trace_id_ts
// rows, declared once on the outermost statement and therefore evaluated
// once however many bounds reference it.
const TraceIDTsEnvelopeAlias = "_cerberus_trace_id_ts_env"

// TraceIDTsEnvelopeStartElement / TraceIDTsEnvelopeEndElement are the 1-based
// tuple positions the bounds read out of that binding. The emitter that builds
// the binding body projects the two aggregates in exactly this order, so the
// order lives here next to the readers rather than being restated at the
// producer.
const (
	TraceIDTsEnvelopeStartElement = 1
	TraceIDTsEnvelopeEndElement   = 2
)

// TraceIDTsEnvelopeBounds builds the same (lo, hi) Timestamp-window SUPERSET
// bounds [TraceIDTsBounds] does — including the identical
// [TraceIDTsEndPadSeconds] pad on the upper bound, for the identical reason —
// reading the envelope out of the scalar binding named by alias instead of
// re-deriving it per bound:
//
//	timestampColumn >= tupleElement(alias, TraceIDTsEnvelopeStartElement)
//	timestampColumn <= addSeconds(tupleElement(alias, TraceIDTsEnvelopeEndElement), TraceIDTsEndPadSeconds)
//
// The cohort predicate a caller passes to [TraceIDTsBounds] is a factory
// because that form embeds the predicate once per bound; here the predicate
// lives in the binding body alone, so a cohort selected by a whole subquery
// (a compare()'s seed scan) is scanned once for both bounds rather than once
// each. ClickHouse evaluates a scalar WITH binding exactly once per statement
// and resolves the alias inside nested subqueries, which is what makes the two
// bounds share one evaluation; a relational (`WITH x AS (SELECT …)`) CTE would
// not, since ClickHouse inlines a relational CTE at every reference.
//
// Both bound shapes select the same rows: a tuple of two aggregates over one
// scan equals the two single-aggregate scans it replaces.
func TraceIDTsEnvelopeBounds(alias, timestampColumn string) (lo, hi Expr) {
	element := func(idx int64) Expr {
		return &FuncCall{Name: "tupleElement", Args: []Expr{
			&BareIdent{Name: alias},
			&LitInt{V: idx},
		}}
	}
	lo = &Binary{
		Op:    OpGe,
		Left:  &ColumnRef{Name: timestampColumn},
		Right: element(TraceIDTsEnvelopeStartElement),
	}
	hi = &Binary{
		Op:   OpLe,
		Left: &ColumnRef{Name: timestampColumn},
		Right: &FuncCall{Name: "addSeconds", Args: []Expr{
			element(TraceIDTsEnvelopeEndElement),
			&LitInt{V: TraceIDTsEndPadSeconds},
		}},
	}
	return lo, hi
}
