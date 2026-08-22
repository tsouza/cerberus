package deltaprefix

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	chdriver "github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// Conn is the narrow ClickHouse surface this package needs: Exec for the
// backfill INSERT ... SELECT, Query for verify's two aggregate reads.
// Satisfied structurally by clickhouse-go's driver.Conn (what
// *chclient.Client.Conn() returns), mirroring internal/routerrules.CHConn's
// discipline of re-declaring a narrow interface rather than importing the
// full driver surface or internal/chclient (which would pull chclient into
// this leaf package for no reason beyond a type name).
type Conn interface {
	Exec(ctx context.Context, query string, args ...any) error
	Query(ctx context.Context, query string, args ...any) (chdriver.Rows, error)
}

// Resource caps stamped on every statement this package issues — mirrors
// internal/routerrules' chCorpusSource discipline: this is an
// operator-run, off-hot-path tool sharing a ClickHouse cluster with live
// query serving, so it biases hard toward never disturbing the data plane.
// The backfill's own execution-time cap is more generous than the
// corpus-mining source's: a real backfill scans a metric's full
// pre-cutover DELTA history once, which can legitimately take longer than
// an interactive report.
const (
	maxExecutionTimeSeconds = 900.0 // 15 minutes
	maxThreads              = 4
	priority                = 10
	maxRowsToRead           = 20_000_000_000
	maxBytesToRead          = 256 << 30 // 256 GiB
)

// withCaps stamps the conservative resource-cap settings onto ctx via the
// clickhouse-go client-side query-settings mechanism (the same one
// internal/routerrules' chCorpusSource uses), so a runaway backfill/verify
// against a very large deployment fails loudly under ClickHouse's own
// enforcement rather than starving the data plane.
func withCaps(ctx context.Context) context.Context {
	return clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"max_execution_time":    maxExecutionTimeSeconds,
		"timeout_overflow_mode": "throw",
		"max_threads":           maxThreads,
		"priority":              priority,
		"max_rows_to_read":      maxRowsToRead,
		"max_bytes_to_read":     maxBytesToRead,
		"read_overflow_mode":    "break",
	}))
}

// Columns names the physical database/table/column identifiers the
// backfill + verify queries reference. Built from a resolved schema.Metrics
// (FromSchema) rather than hardcoded, unlike internal/schema/ddl's DDL
// rendering — that package always creates the FIXED canonical upstream
// column shape, while this package queries a REAL, already-deployed
// database whose column names follow whatever schema.Metrics (including any
// operator override) says they are.
type Columns struct {
	Database                     string
	SumTable                     string
	DeltaPrefixTable             string
	MetricNameColumn             string
	AttributesColumn             string
	ResourceAttributesColumn     string
	ServiceNameColumn            string
	TimestampColumn              string
	ValueColumn                  string
	AggregationTemporalityColumn string
	DeltaPrefixBucketColumn      string
	DeltaPrefixSumColumn         string
}

// FromSchema builds Columns from the ClickHouse database name plus the
// resolved runtime schema.Metrics (config.Config.Schema).
func FromSchema(database string, m schema.Metrics) Columns {
	return Columns{
		Database:                     database,
		SumTable:                     m.SumTable,
		DeltaPrefixTable:             m.DeltaPrefixTable,
		MetricNameColumn:             m.MetricNameColumn,
		AttributesColumn:             m.AttributesColumn,
		ResourceAttributesColumn:     m.ResourceAttributesColumn,
		ServiceNameColumn:            m.ServiceNameColumn,
		TimestampColumn:              m.TimestampColumn,
		ValueColumn:                  m.ValueColumn,
		AggregationTemporalityColumn: m.AggregationTemporalityColumn,
		DeltaPrefixBucketColumn:      m.DeltaPrefixBucketColumn,
		DeltaPrefixSumColumn:         m.DeltaPrefixSumColumn,
	}
}

// dayBucket renders `toStartOfDay(<col>)`, the exact expression the
// DELTA-prefix materialized view groups by (internal/schema/ddl's
// renderDeltaPrefixView) — reused here so the backfill's own GROUP BY
// matches the MV's bucket boundaries exactly.
func dayBucket(col string) chsql.Frag {
	return chsql.Call("toStartOfDay", chsql.Col(col))
}

// BackfillSQL renders the one-time `INSERT INTO ... SELECT` that populates
// c.DeltaPrefixTable for c.SumTable history strictly older than before's
// calendar day (see the package doc comment for why the bound is rounded to
// a day boundary and why it must be the MV's own creation time). The
// GROUP BY / column shape matches internal/schema/ddl's
// renderDeltaPrefixView exactly, so a row this backfill writes is
// indistinguishable from one the live MV would have written for the same
// source rows.
func BackfillSQL(c Columns, before time.Time) (string, []any) {
	bucket := dayBucket(c.TimestampColumn)
	body := chsql.NewQuery().
		Select(
			chsql.Col(c.MetricNameColumn),
			chsql.Col(c.AttributesColumn),
			chsql.Col(c.ResourceAttributesColumn),
			chsql.Col(c.ServiceNameColumn),
			chsql.As(bucket, c.DeltaPrefixBucketColumn),
			chsql.As(chsql.Call("sum", chsql.Col(c.ValueColumn)), c.DeltaPrefixSumColumn),
		).
		From(chsql.Qual(c.Database, c.SumTable)).
		Where(
			chsql.Eq(chsql.Col(c.AggregationTemporalityColumn), chsql.InlineLit(schema.AggregationTemporalityDelta)),
			chsql.Lt(chsql.Col(c.TimestampColumn), dayBucketOf(before)),
		).
		GroupBy(
			chsql.Col(c.MetricNameColumn),
			chsql.Col(c.AttributesColumn),
			chsql.Col(c.ResourceAttributesColumn),
			chsql.Col(c.ServiceNameColumn),
			bucket,
		)
	return chsql.InsertSelect(
		c.Database, c.DeltaPrefixTable,
		[]string{c.MetricNameColumn, c.AttributesColumn, c.ResourceAttributesColumn, c.ServiceNameColumn, c.DeltaPrefixBucketColumn, c.DeltaPrefixSumColumn},
		body,
	).Build()
}

// dayBucketOf renders `toStartOfDay(?)` binding t as a real positional
// arg (Lit, never InlineLit — t is operator-supplied data, not part of the
// statement shape). Pairs with dayBucket, which renders the same function
// over a column instead of a bound value.
func dayBucketOf(t time.Time) chsql.Frag {
	return chsql.Call("toStartOfDay", chsql.Lit(t))
}

// Backfill executes BackfillSQL against conn.
func Backfill(ctx context.Context, conn Conn, c Columns, before time.Time) error {
	sql, args := BackfillSQL(c, before)
	if err := conn.Exec(withCaps(ctx), sql, args...); err != nil {
		return fmt.Errorf("deltaprefix: backfill %s: %w", c.DeltaPrefixTable, err)
	}
	return nil
}

// aggregateTotalsSQL renders the per-metric-name sum(PartialSum) read from
// the DELTA-prefix table, bounded the same way BackfillSQL bounds its own
// write: strictly before before's calendar day.
func aggregateTotalsSQL(c Columns, before time.Time) (string, []any) {
	q := chsql.NewQuery().
		Select(chsql.Col(c.MetricNameColumn), chsql.Call("sum", chsql.Col(c.DeltaPrefixSumColumn))).
		From(chsql.Qual(c.Database, c.DeltaPrefixTable)).
		Where(chsql.Lt(chsql.Col(c.DeltaPrefixBucketColumn), dayBucketOf(before))).
		GroupBy(chsql.Col(c.MetricNameColumn))
	return q.Build()
}

// baseTotalsSQL renders the per-metric-name sum(Value) read from the base
// sum table, restricted to DELTA-temporality rows strictly before before's
// calendar day — the exact population BackfillSQL writes into the
// DELTA-prefix table, so a correct backfill makes this total and
// aggregateTotalsSQL's total agree per metric name.
func baseTotalsSQL(c Columns, before time.Time) (string, []any) {
	q := chsql.NewQuery().
		Select(chsql.Col(c.MetricNameColumn), chsql.Call("sum", chsql.Col(c.ValueColumn))).
		From(chsql.Qual(c.Database, c.SumTable)).
		Where(
			chsql.Eq(chsql.Col(c.AggregationTemporalityColumn), chsql.InlineLit(schema.AggregationTemporalityDelta)),
			chsql.Lt(chsql.Col(c.TimestampColumn), dayBucketOf(before)),
		).
		GroupBy(chsql.Col(c.MetricNameColumn))
	return q.Build()
}

// Mismatch is one metric name whose DELTA-prefix aggregate total disagrees
// with the base table's own DELTA total by more than the verify tolerance.
// A metric present on only one side reports 0 for the other, which reads
// naturally as a full mismatch (aggregate table missing a metric entirely,
// or carrying one the base table's DELTA rows never produced).
type Mismatch struct {
	MetricName string
	Aggregate  float64
	Base       float64
}

// Diff is Aggregate - Base — positive means the aggregate table over-counts
// this metric, negative means it under-counts.
func (m Mismatch) Diff() float64 { return m.Aggregate - m.Base }

// Report is Verify's result: the two per-metric total maps it compared, the
// bound + tolerance it compared them under, and the resulting mismatches
// (empty means the backfill is complete for every metric observed).
type Report struct {
	Before          time.Time
	Tolerance       float64
	AggregateTotals map[string]float64
	BaseTotals      map[string]float64
	Mismatches      []Mismatch
}

// Pass reports whether every metric's totals agreed within tolerance.
func (r Report) Pass() bool { return len(r.Mismatches) == 0 }

// Verify reads both per-metric totals and diffs them, returning a Report an
// operator (or the `delta-prefix-verify` CLI verb) can render before
// flipping CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED=true.
func Verify(ctx context.Context, conn Conn, c Columns, before time.Time, tolerance float64) (Report, error) {
	aggSQL, aggArgs := aggregateTotalsSQL(c, before)
	agg, err := scanTotals(ctx, conn, aggSQL, aggArgs)
	if err != nil {
		return Report{}, fmt.Errorf("deltaprefix: read %s totals: %w", c.DeltaPrefixTable, err)
	}
	baseSQL, baseArgs := baseTotalsSQL(c, before)
	base, err := scanTotals(ctx, conn, baseSQL, baseArgs)
	if err != nil {
		return Report{}, fmt.Errorf("deltaprefix: read %s totals: %w", c.SumTable, err)
	}
	return Diff(agg, base, before, tolerance), nil
}

// Diff compares two per-metric-name total maps and returns the Report —
// split out from Verify so the comparison logic is unit-testable against
// fixture maps, without a Conn.
func Diff(agg, base map[string]float64, before time.Time, tolerance float64) Report {
	names := make(map[string]struct{}, len(agg)+len(base))
	for name := range agg {
		names[name] = struct{}{}
	}
	for name := range base {
		names[name] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	var mismatches []Mismatch
	for _, name := range sorted {
		a, b := agg[name], base[name]
		if math.Abs(a-b) > tolerance {
			mismatches = append(mismatches, Mismatch{MetricName: name, Aggregate: a, Base: b})
		}
	}
	return Report{
		Before:          before,
		Tolerance:       tolerance,
		AggregateTotals: agg,
		BaseTotals:      base,
		Mismatches:      mismatches,
	}
}

// scanTotals runs sql and scans a (MetricName, total float64) result into a
// map. A driver.Rows implementation is required to report an error via
// either Query's own return or rows.Err() after the last Next() — this
// checks both, matching internal/chclient's own row-scan idiom elsewhere.
func scanTotals(ctx context.Context, conn Conn, sql string, args []any) (map[string]float64, error) {
	rows, err := conn.Query(withCaps(ctx), sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]float64{}
	for rows.Next() {
		var name string
		var total float64
		if err := rows.Scan(&name, &total); err != nil {
			return nil, err
		}
		out[name] = total
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
