package chsql

import (
	"context"
	"fmt"
	"time"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/schema"
)

// EmitQueryExemplars renders the SQL for Prometheus's
// /api/v1/query_exemplars endpoint over the OTel ClickHouse Exporter's
// `Exemplars` Nested column. The endpoint returns every exemplar the
// caller's matchers select within `[start, end]`, fanned out one row
// per per-data-point exemplar via `arrayJoin(arrayEnumerate(...))`.
//
// Distinct from EmitMetricsExemplars (Tempo path), which scans
// `otel_traces` and picks one representative span per (series, bucket)
// anchor via `argMax`. The two emitters share only the wire envelope
// (the `{traceID, spanID, value, timestamp, labels}` per-exemplar tuple
// the handler projects on top); the SQL shape, the source table, and
// the per-row cardinality differ.
//
// The emitted SQL projects the eight columns the handler consumes,
// positionally:
//
//	MetricName String, Attributes Map(String,String), ServiceName String,
//	Timestamp DateTime64(9), Value Float64, TraceID String, SpanID String,
//	ExemplarAttributes Map(LowCardinality(String),String)
//
// — one row per exemplar. The handler groups rows by
// `(MetricName, Attributes, ServiceName)` into ExemplarSeries, merges
// the reserved `trace_id` / `span_id` keys into ExemplarAttributes,
// and emits the upstream Prom wire envelope.
//
// SQL shape (the inner SELECT fans out via `arrayJoin(arrayEnumerate(...))`;
// the outer projects each exemplar tuple component via `<arr>[i]`):
//
//	SELECT
//	    `MetricName`, `Attributes`, `ServiceName`,
//	    ts[i]        AS `Timestamp`,
//	    val[i]       AS `Value`,
//	    tid[i]       AS `TraceID`,
//	    sid[i]       AS `SpanID`,
//	    attrs_arr[i] AS `ExemplarAttributes`
//	FROM (
//	    SELECT
//	        `MetricName`, `Attributes`, `ServiceName`,
//	        `Exemplars`.`TimeUnix`           AS `ts`,
//	        `Exemplars`.`Value`              AS `val`,
//	        `Exemplars`.`TraceId`            AS `tid`,
//	        `Exemplars`.`SpanId`             AS `sid`,
//	        `Exemplars`.`FilteredAttributes` AS `attrs_arr`,
//	        arrayJoin(arrayEnumerate(`Exemplars`.`TimeUnix`)) AS `i`
//	    FROM `<exemplarsTable>`
//	    WHERE <predicate>
//	      AND `TimeUnix` >= toDateTime64(?, ?)
//	      AND `TimeUnix` <= toDateTime64(?, ?)
//	      AND length(`Exemplars`.`TimeUnix`) > 0
//	)
//
// The outer SELECT references the inner aliases (ts, val, tid, sid,
// attrs_arr, i) bare — without backticks — matching the convention
// the existing exemplars / range-window emitters use for
// emitter-pinned synthetic alias references via BareIdent. The time
// bounds bind as parameterised toDateTime64(?, ?) calls — the first
// `?` is the UTC-formatted timestamp string, the second is the
// precision constant 9 — mirroring the bind shape PromQL lowering
// produces in internal/promql/modifiers.go::anchorBaseExpr.
//
// The inner subquery applies the matcher predicate, time-bounds the
// scan, and drops rows whose Exemplars Nested column is empty before
// the `arrayJoin` fan-out. The outer SELECT then indexes each parallel
// sub-field by `i` to surface one exemplar per row.
//
// Returns ErrUnsupported when:
//
//   - `exemplarsTable` equals `s.SummaryTable` — the OTel-CH summary
//     table has no `Exemplars` column upstream; the handler must
//     short-circuit summary metrics with `data:[]` before reaching this
//     emitter, but we defensively guard here too;
//   - `s.ExemplarsColumn` is empty — the schema has been configured to
//     elide exemplars (e.g. a deployment running an exporter version
//     that pre-dates the Exemplars column);
//   - `s.MetricNameColumn`, `s.AttributesColumn`, `s.ServiceNameColumn`,
//     or `s.TimestampColumn` is empty — required identity / time
//     columns missing from the schema.
//
// Note on schema variants: the upstream DDL ships `Exemplars` as a
// Nested column with the default `flatten_nested = 1`, so each
// sub-field surfaces as a parallel `Array(...)` accessed by
// `Exemplars.TimeUnix` / etc. A deployment running with
// `flatten_nested = 0` would expose a single `Array(Tuple(...))`
// instead; the SQL emitted here targets the parallel-array form.
// Other read paths in cerberus (Events Nested on `otel_traces`, …) make
// the same assumption — this is a whole-codebase invariant rather than
// an exemplars-specific risk.
func EmitQueryExemplars(
	ctx context.Context,
	exemplarsTable string,
	predicate Frag,
	start, end time.Time,
	s schema.Metrics,
) (string, []any, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanEmit)
	defer span.End()

	sql, args, err := emitQueryExemplarsArm(ExemplarArm{Table: exemplarsTable, Predicate: predicate}, start, end, s)
	if err != nil {
		span.RecordError(err)
		return "", nil, err
	}
	span.SetAttributes(cerbtrace.AttrSQLLength.Int(len(sql)))
	return sql, args, nil
}

// ExemplarArm is one arm of an exemplar fan-out: the candidate table to
// read, paired with the matcher predicate that resolves the queried
// series ON THAT table.
//
// The predicate is per-arm rather than shared because the row key is
// per-arm: a `<base>_count` selector filters `MetricName = '<base>_count'`
// on the Sum and Gauge tables but `MetricName = '<base>'` on the
// histogram table, where the OTel-CH exporter serves the companion series
// off the bare-named row's columns. See [schema.Metrics.ExemplarSources],
// which resolves the (table, row key) pairs the caller turns into arms.
type ExemplarArm struct {
	Table     string
	Predicate Frag
}

// EmitQueryExemplarsUnion renders one statement reading every arm in
// arms, in order, as `(<arm>) UNION ALL (<arm>) …`. Each arm is the exact
// SELECT [EmitQueryExemplars] renders for a single table, so the column
// contract the handler decodes positionally is identical across arms and
// the union is row-compatible by construction. A single arm renders
// byte-identically to [EmitQueryExemplars] — no parentheses, no UNION
// keyword.
//
// The union is the TOP-LEVEL statement rather than the FROM of a
// wrapping SELECT: the projection carries two Map-typed columns
// (Attributes, ExemplarAttributes) and a redundant `SELECT * FROM (…)`
// boundary makes some ClickHouse drivers refuse to cast a Map back
// (see [Render]).
//
// Each arm's args bind in emit order — arm 0's predicate literal, arm 0's
// two time bounds, then arm 1's, and so on.
//
// Returns ErrUnsupported for an empty arm list: a caller with no candidate
// table must answer with an empty result rather than ask for SQL, and the
// same per-arm schema guards [EmitQueryExemplars] applies (summary table,
// missing identity columns, exemplars elided from the schema) reject here
// too.
func EmitQueryExemplarsUnion(
	ctx context.Context,
	arms []ExemplarArm,
	start, end time.Time,
	s schema.Metrics,
) (string, []any, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanEmit)
	defer span.End()

	if len(arms) == 0 {
		err := fmt.Errorf("%w: no exemplars table to read", ErrUnsupported)
		span.RecordError(err)
		return "", nil, err
	}
	if len(arms) == 1 {
		sql, args, err := emitQueryExemplarsArm(arms[0], start, end, s)
		if err != nil {
			span.RecordError(err)
			return "", nil, err
		}
		span.SetAttributes(cerbtrace.AttrSQLLength.Int(len(sql)))
		return sql, args, nil
	}

	parts := make([]Frag, 0, len(arms))
	for _, arm := range arms {
		q, err := queryExemplarsSelect(arm, start, end, s)
		if err != nil {
			span.RecordError(err)
			return "", nil, err
		}
		parts = append(parts, q.Frag())
	}

	b := NewBuilder()
	UnionAll(parts...)(b)
	sql, args, err := b.Build()
	if err != nil {
		span.RecordError(err)
		return "", nil, err
	}
	span.SetAttributes(cerbtrace.AttrSQLLength.Int(len(sql)))
	return sql, args, nil
}

// emitQueryExemplarsArm renders one arm as a standalone statement — the
// unparenthesised SELECT both single-table entry points return.
func emitQueryExemplarsArm(arm ExemplarArm, start, end time.Time, s schema.Metrics) (string, []any, error) {
	q, err := queryExemplarsSelect(arm, start, end, s)
	if err != nil {
		return "", nil, err
	}
	return q.subquerySQL()
}

// queryExemplarsSelect builds the outer SELECT for one arm, validating
// the arm's table and the schema columns the projection needs. See
// [EmitQueryExemplars] for the emitted shape and the rejection cases.
func queryExemplarsSelect(
	arm ExemplarArm,
	start, end time.Time,
	s schema.Metrics,
) (*QueryBuilder, error) {
	exemplarsTable, predicate := arm.Table, arm.Predicate

	if exemplarsTable == "" {
		err := fmt.Errorf("%w: exemplarsTable is empty", ErrUnsupported)
		return nil, err
	}
	// The OTel-CH summary table has no Exemplars Nested column upstream.
	// Routing summary-shaped queries here would emit SQL that references
	// a non-existent column and fail at run time.
	// schema.Metrics.ExemplarSources — the resolver the handler routes
	// through — filters the summary table out of every candidate set; we
	// guard defensively in case a future caller bypasses that routing.
	if s.SummaryTable != "" && exemplarsTable == s.SummaryTable {
		err := fmt.Errorf("%w: exemplars not supported on summary table %q", ErrUnsupported, exemplarsTable)
		return nil, err
	}
	if s.ExemplarsColumn == "" {
		err := fmt.Errorf("%w: schema.Metrics.ExemplarsColumn is empty (exporter version pre-dates exemplars?)", ErrUnsupported)
		return nil, err
	}
	if s.MetricNameColumn == "" {
		err := fmt.Errorf("%w: schema.Metrics.MetricNameColumn is empty", ErrUnsupported)
		return nil, err
	}
	if s.AttributesColumn == "" {
		err := fmt.Errorf("%w: schema.Metrics.AttributesColumn is empty", ErrUnsupported)
		return nil, err
	}
	if s.ServiceNameColumn == "" {
		err := fmt.Errorf("%w: schema.Metrics.ServiceNameColumn is empty", ErrUnsupported)
		return nil, err
	}
	if s.TimestampColumn == "" {
		err := fmt.Errorf("%w: schema.Metrics.TimestampColumn is empty", ErrUnsupported)
		return nil, err
	}

	// Inner SELECT — fans out via `arrayJoin(arrayEnumerate(...))` and
	// applies all predicates ahead of the fan-out so the cross product
	// only fires on rows that pass the matcher + time window.
	tsField := Qual(s.ExemplarsColumn, "TimeUnix")
	valField := Qual(s.ExemplarsColumn, "Value")
	tidField := Qual(s.ExemplarsColumn, "TraceId")
	sidField := Qual(s.ExemplarsColumn, "SpanId")
	attrsField := Qual(s.ExemplarsColumn, "FilteredAttributes")

	inner := NewQuery().
		Select(
			Col(s.MetricNameColumn),
			Col(s.AttributesColumn),
			Col(s.ServiceNameColumn),
			As(tsField, "ts"),
			As(valField, "val"),
			As(tidField, "tid"),
			As(sidField, "sid"),
			As(attrsField, "attrs_arr"),
			As(Call("arrayJoin", Call("arrayEnumerate", tsField)), "i"),
		).
		From(Col(exemplarsTable))

	// Predicate composition: (matcher_predicate) AND
	// (TimeUnix >= toDateTime64(start, 9)) AND
	// (TimeUnix <= toDateTime64(end, 9)) AND
	// (length(Exemplars.TimeUnix) > 0).
	//
	// The matcher predicate is rendered first so it lands in argument
	// position 0; the time bounds and the non-empty-exemplars guard
	// follow in source order. The non-empty guard short-circuits the
	// arrayJoin for source rows whose Exemplars array is empty — without
	// it, CH would still emit zero rows for those source rows (arrayJoin
	// on `[]` produces nothing), but the predicate is cheap and makes
	// the intent explicit in the plan.
	where := []Frag{}
	if predicate != nil {
		where = append(where, Paren(predicate))
	}
	where = append(
		where,
		Gte(Col(s.TimestampColumn), dateTime64Frag(start)),
		Lte(Col(s.TimestampColumn), dateTime64Frag(end)),
		Gt(Call("length", tsField), InlineLit(int64(0))),
	)
	inner.Where(where...)

	// Outer SELECT — picks each parallel-array element by the
	// `arrayJoin(arrayEnumerate(...))` index alias `i`. The outer
	// projection's column aliases are the contract the handler decodes
	// rows by: positional binding to MetricName / Attributes /
	// ServiceName / Timestamp / Value / TraceID / SpanID /
	// ExemplarAttributes.
	outer := NewQuery().
		Select(
			Col(s.MetricNameColumn),
			Col(s.AttributesColumn),
			Col(s.ServiceNameColumn),
			As(Subscript(BareIdent("ts"), BareIdent("i")), "Timestamp"),
			As(Subscript(BareIdent("val"), BareIdent("i")), "Value"),
			As(Subscript(BareIdent("tid"), BareIdent("i")), "TraceID"),
			As(Subscript(BareIdent("sid"), BareIdent("i")), "SpanID"),
			As(Subscript(BareIdent("attrs_arr"), BareIdent("i")), "ExemplarAttributes"),
		).
		From(inner.Frag())

	return outer, nil
}

// dateTime64Frag returns a Frag emitting `toDateTime64(?, ?)` with the
// time formatted at nanosecond precision and the precision constant 9
// bound as a second positional argument. Matches the bind pattern the
// promql lowering produces for time-bound predicates
// (see internal/promql/modifiers.go::anchorBaseExpr), so the args
// section in fixtures across the codebase stays uniform: a string
// "2026-01-01 00:00:00.000000000" followed by an int64 9.
//
// Zero `t` panics — callers must validate the time range before
// reaching this emitter.
func dateTime64Frag(t time.Time) Frag {
	return Call(
		"toDateTime64",
		Lit(t.UTC().Format("2006-01-02 15:04:05.000000000")),
		Lit(int64(nanoScale)),
	)
}
