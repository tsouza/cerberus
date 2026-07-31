// Package chsql emits ClickHouse SQL from a chplan.Node tree.
//
// Emit produces a parameterized SQL string with `?` placeholders plus a
// matching args slice; the caller (api/chclient) passes both to the
// ClickHouse driver. Every node type is emitted as a self-contained SELECT
// statement and children are inlined as subqueries; PR6's optimizer is
// expected to collapse trivial single-row subqueries.
//
// Emit always renders one plan into one CH statement. The default route A
// executes exactly that statement per request. The sharded-pushdown solver
// (internal/solver, docs/solver.md) does not change this: for
// the narrow memory-unbounded anchor-fan-out class it re-anchors K deep copies
// of the same optimized plan onto disjoint anchor slices and calls Emit once
// per slice — no new SQL template, just the same per-statement emission run K
// times. The relaxed "one statement per request" invariant lives in
// docs/performance.md.
package chsql

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/spansscan"
)

// tracer emits the `emit` pipeline-stage span.
var tracer = otel.Tracer("github.com/tsouza/cerberus/internal/chsql")

// defaultSpansScanTable is the spans table the universal emit-chokepoint guard
// matches against when the emit context carries no explicit spans table —
// PromQL / LogQL emissions and the spec/golden fixtures that never thread
// chsql.WithSpansTable. It is the schema default, so the guard still scans the
// golden lane's literal `otel_traces` SQL and the whole corpus becomes a
// boundedness proof. Non-spans heads never emit this table name, so scoping the
// fallback to it leaves them unaffected.
var defaultSpansScanTable = schema.DefaultOTelTraces().SpansTable

// attributeMapColumns names every label-Map column the three OTel-CH schemas
// expose. It is what chplan.CanonicalizeSeriesIdentityKeys resolves a
// series-identity key against at the emit chokepoint: a key that reads one of
// these raw, without chplan.CanonicalMapFunc, splits one logical series across
// the physical Map key orders the store happens to hold.
//
// The set is built from the schema DEFAULTS rather than threaded per request,
// because the chokepoint has no schema in hand and the fixture lane threads no
// context at all — defaults keep the golden corpus under the gate. A deployment
// that RENAMES an attribute column is covered by its head's lowering, which is
// schema-aware; this gate is the defence-in-depth layer behind it.
var attributeMapColumns = func() chplan.AttributeMapColumns {
	m, l, t := schema.DefaultOTelMetrics(), schema.DefaultOTelLogs(), schema.DefaultOTelTraces()
	return chplan.NewAttributeMapColumns(
		m.AttributesColumn, m.ResourceAttributesColumn, m.ScopeAttributesColumn,
		l.AttributesColumn, l.ResourceAttributesColumn, l.ScopeAttributesColumn,
		t.AttributesColumn, t.ResourceAttributesColumn, t.ScopeAttributesColumn,
	)
}()

// GuardEmittedSQL is the single universal partition-pruning backstop every
// spans-SQL string must pass before it reaches a ClickHouse querier. It scans
// the final SQL text for a physical otel_traces scan sitting in a scope where
// ClickHouse cannot push the request window into the partition pruner — a
// recursive (`WITH RECURSIVE`) arm or a pre-`TraceId IN` `GROUP BY` — yet
// carrying no co-scope `Timestamp` predicate, and returns ErrUnboundedSpansScan
// on any such finding. The matcher only fires when the statement otherwise
// carries a request window, so windowless-by-design plans are left to the
// node-level resource-bound gate.
//
// The per-site requireSpansScanWindow / fromSpansScan guards fire earlier with
// node-level precision, but they are forgettable — a new emitter or a re-routed
// plan can reach the wire without one. Scanning the final string makes a
// windowless recursive / GROUP-BY otel_traces scan impossible to emit
// regardless of which handler, builder, or fixture produced it.
//
// chsql.Emit calls this on its rendered SQL, and so must every other path that
// builds otel_traces SQL outside Emit (EmitMetricsExemplars; the Tempo head's
// guardedQuerier wrapper over the /search/tags + /search/tag/*/values + root
// lookup string queries). The spans table is resolved from ctx
// (chsql.WithSpansTable); when none is threaded — PromQL / LogQL / spec
// fixtures — it falls back to the schema default so the golden lane is still
// scanned. The matcher is a no-op on SQL that contains no otel_traces scan, so
// over-applying it across non-spans heads is safe.
func GuardEmittedSQL(ctx context.Context, sql string) error {
	guardTable := spansTableFromCtx(ctx)
	if guardTable == "" {
		guardTable = defaultSpansScanTable
	}
	if findings := spansscan.UnwindowedSpansScans(sql, guardTable); len(findings) > 0 {
		return fmt.Errorf("%w: %s", ErrUnboundedSpansScan, findings[0].Reason)
	}
	return nil
}

// ErrUnsupported is returned when the emitter encounters a node or
// expression it doesn't know how to render. Test fixtures cover every
// supported case; bumping coverage means extending the switch in
// emitNode/emitExpr.
var ErrUnsupported = errors.New("chsql: unsupported")

// Emit serializes a chplan tree as a ClickHouse SQL statement plus the
// positional argument list to bind. The SQL uses `?` placeholders.
//
// The ctx parameter carries the parent OpenTelemetry span (typically
// the otelhttp request span). Emit wraps the rendering in an `emit`
// pipeline-stage span so a query's flame graph shows how long SQL
// serialization took. The emitted SQL byte length is surfaced as
// `cerberus.sql_length` on the span.
func Emit(ctx context.Context, n chplan.Node) (string, []any, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanEmit)
	defer span.End()
	if n == nil {
		err := fmt.Errorf("%w: nil node", ErrUnsupported)
		span.RecordError(err)
		return "", nil, err
	}
	// Establish the IR-level scan time bound on any instant windowed-array
	// leaf Scan that lacks one. In production the optimizer's
	// NormalizeScanTimeBound analyzer rule has already done this (so this is
	// a no-op read-only walk); on the test/spec lower→emit lane, which skips
	// the optimizer, this is where the bound is established so the emitted
	// SQL still prunes granules. Single mechanism, derived once in
	// chplan — emitters no longer remember it.
	n = chplan.AttachInstantScanTimeBounds(n)
	// Enforce the spans-scan resource-bound invariant at the emit chokepoint.
	// spansTableFromCtx is "" for PromQL / metrics matrix emission (no spans
	// table under enforcement), so this is a table-scoped no-op for those
	// heads; for the Tempo plans whose context carries the spans table (root
	// lookup, structure-tab search) it rejects any otel_traces Scan that would
	// reach ClickHouse without partition pruning or a finite trace-id set.
	spansTable := spansTableFromCtx(ctx)
	if err := chplan.RequireSpansScansBounded(spansTable, n); err != nil {
		span.RecordError(err)
		return "", nil, err
	}
	// Establish the Map key-order invariant on every series-identity key at the
	// same chokepoint. Each head's lowering already establishes it, so this is
	// an identity rewrite in production; it exists so a plan assembled without
	// crossing a lowering — a hand-built tree, or a refactor that repoints a
	// node past the canonicalising projection — cannot emit SQL that splits one
	// series across two Map key orders.
	n = chplan.CanonicalizeSeriesIdentityKeys(n, attributeMapColumns)
	e := &emitter{spansTable: spansTable, ctxSpansTable: spansTable}
	if err := e.emitNode(n); err != nil {
		span.RecordError(err)
		return "", nil, err
	}
	sql := e.b.String()
	if err := GuardEmittedSQL(ctx, sql); err != nil {
		span.RecordError(err)
		return "", nil, err
	}
	// chaosSleepWrap is a no-op in every build except the chaos e2e
	// lane's `chaos_sleep`-tagged image, where it splices a server-side
	// ClickHouse sleep when the request ctx carries one (see
	// chaos_sleep.go / chaos_sleep_stub.go). Production links the stub,
	// so this is the identity transform.
	sql, args := chaosSleepWrap(ctx, sql, e.args)
	span.SetAttributes(cerbtrace.AttrSQLLength.Int(len(sql)))
	return sql, args, nil
}

type emitter struct {
	b    strings.Builder
	args []any

	// spansTable is the TraceQL spans table under resource-bound enforcement,
	// or "" when none (PromQL / metrics matrix emit). When non-empty, the
	// per-site fromSpansScan helper rejects any FROM <spansTable> rendered
	// without a declared resource bound. It is seeded from the emit context
	// (chsql.Emit) and re-asserted by the whole-trace emitters
	// (emitNestedSetAnnotate / emitStructuralJoin) from the node's own table
	// so the structure-tab synthetic recursive scans are gated even when the
	// caller did not thread WithSpansTable.
	spansTable string

	// ctxSpansTable is the spans table threaded purely from the emit context
	// (chsql.WithSpansTable), captured before the whole-trace emitters
	// (emitStructuralJoin / emitNestedSetAnnotate) overwrite spansTable from
	// the node's own table. It is the genuine "this is a production Tempo
	// request under enforcement" signal: the spec/golden lane never threads
	// WithSpansTable, so ctxSpansTable stays "" there even while spansTable is
	// force-set by the recursive emitters. The request-window fail-closed
	// (requireSpansScanWindow) keys on it so the partition-prune window is
	// required only on the real Tempo path and the golden lane stays
	// byte-identical (no window-bearing fixtures churn).
	ctxSpansTable string

	// cteSeq is a monotonic counter handed out to every emitter that
	// registers a named CTE, so each one gets a unique name: the
	// recursive structural-join closure (`_struct_closure_<n>`) and the
	// `&&` set-op arms (`_setand_{l,r}_<n>`). Nested structural joins
	// (`A << B << C`) embed an inner closure inside the outer closure's
	// recursive arm (via the #77 seed-trace-id pushdown subquery);
	// without unique names CH binds the inner same-named CTE in the
	// outer scope and rejects the outer as "not recursive" (error 49).
	// Nested `&&` (`A && B && C`) has the same shape: the inner set-op
	// renders as a subquery of the outer one, and CH would bind the
	// inner arm CTE in the outer scope. One counter across both
	// emitters keeps every name in a single statement distinct.
	cteSeq int
}

// nextCTESeq returns the next unique CTE sequence number, advancing the
// counter.
func (e *emitter) nextCTESeq() int {
	e.cteSeq++
	return e.cteSeq
}

// emitNode writes a `SELECT ...` statement for n into e.b.
func (e *emitter) emitNode(n chplan.Node) error {
	switch v := n.(type) {
	case *chplan.Scan:
		return e.emitScan(v)
	case *chplan.OneRow:
		return e.emitOneRow(v)
	case *chplan.StepGrid:
		return e.emitStepGrid(v)
	case *chplan.Filter:
		return e.emitFilter(v)
	case *chplan.Project:
		return e.emitProject(v)
	case *chplan.Aggregate:
		return e.emitAggregate(v)
	case *chplan.Limit:
		return e.emitLimit(v)
	case *chplan.OrderBy:
		return e.emitOrderBy(v)
	case *chplan.TopK:
		return e.emitTopK(v)
	case *chplan.VectorJoin:
		return e.emitVectorJoin(v)
	case *chplan.VectorSetOp:
		return e.emitVectorSetOp(v)
	case *chplan.NaryVectorSetOp:
		return e.emitNaryVectorSetOp(v)
	case *chplan.InfoJoin:
		return e.emitInfoJoin(v)
	case *chplan.StructuralJoin:
		return e.emitStructuralJoin(v)
	case *chplan.NestedSetAnnotate:
		return e.emitNestedSetAnnotate(v)
	case *chplan.SearchTraceLimit:
		return e.emitSearchTraceLimit(v)
	case *chplan.CrossJoin:
		return e.emitCrossJoin(v)
	case *chplan.SetOperation:
		return e.emitSetOperation(v)
	case *chplan.UnionAll:
		return e.emitUnionAll(v)
	default:
		if handled, err := e.emitMetricNode(n); handled {
			return err
		}
		return fmt.Errorf("%w: node %T", ErrUnsupported, n)
	}
}

// emitMetricNode dispatches the metric / range-window / histogram node
// family — the analytical nodes the PromQL & TraceQL metrics pipelines
// produce. Split out of emitNode so that switch stays under the cyclop
// complexity budget as new relational nodes are added; returns handled=false
// for any node it doesn't own so emitNode falls through to ErrUnsupported.
func (e *emitter) emitMetricNode(n chplan.Node) (bool, error) {
	switch v := n.(type) {
	case *chplan.MetricsAggregate:
		return true, e.emitMetricsAggregate(v)
	case *chplan.MetricsSecondStage:
		return true, e.emitMetricsSecondStage(v)
	case *chplan.MetricsHistogramOverTime:
		return true, e.emitMetricsHistogramOverTime(v)
	case *chplan.MetricsCompare:
		return true, e.emitMetricsCompare(v)
	case *chplan.RangeWindow:
		return true, e.emitRangeWindow(v)
	case *chplan.RangeWindowNative:
		return true, e.emitRangeWindowNative(v)
	case *chplan.RangeLWR:
		return true, e.emitRangeLWR(v)
	case *chplan.RangeWindowResample:
		return true, e.emitRangeWindowResample(v)
	case *chplan.RangeBucketFanout:
		return true, e.emitRangeBucketFanout(v)
	case *chplan.AbsentOverTime:
		return true, e.emitAbsentOverTime(v)
	case *chplan.HistogramQuantile:
		return true, e.emitHistogramQuantile(v)
	case *chplan.HistogramQuantileNative:
		return true, e.emitHistogramQuantileNative(v)
	}
	return false, nil
}

// emitSubquery wraps emitNode(n) in parentheses, used wherever a node feeds
// a parent SELECT's FROM clause.
func (e *emitter) emitSubquery(n chplan.Node) error {
	e.b.WriteByte('(')
	if err := e.emitNode(n); err != nil {
		return err
	}
	e.b.WriteByte(')')
	return nil
}
