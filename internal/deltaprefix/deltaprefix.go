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
// c.DeltaPrefixTable for c.SumTable history strictly older than before — the
// MV's own exact creation instant, NOT rounded to a calendar-day boundary
// (see the package doc comment: a live deployment's MV is almost always
// created mid-day, and the live MV only starts capturing INSERTs from that
// exact instant onward, so an exact-instant cutoff here is the only bound
// that leaves no gap between what this backfill covers and what the MV
// covers). The stored bucket column (DeltaPrefixBucketColumn) is still
// calendar-day granularity — that is the aggregate table's own storage
// resolution, set by GROUP BY / dayBucket below, not by this WHERE bound —
// so the cutover day's bucket can legitimately carry two partial
// contributions (this backfill's pre-instant rows, the live MV's
// post-instant rows) that merge additively via the table's
// SimpleAggregateFunction(sum, ...) column, exactly like any other pair of
// same-key rows the table receives from ordinary concurrent inserts. The
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
			chsql.Lt(chsql.Col(c.TimestampColumn), chsql.Lit(before)),
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

// nowFunc is the retention-boundary clock — a package-level var (never a
// direct time.Now() call in retentionBoundary/Backfill/Verify) so
// deltaprefix_test.go can pin "now" and exercise the day-exclusion logic
// deterministically, matching this package's doc-comment promise that the
// SQL composition + result diffing stays unit testable without a live
// ClickHouse connection (cerberus issue #2652).
var nowFunc = time.Now

// retentionBoundary computes the instant before which a day's own
// BucketStart + retention has already elapsed as of now — the exact
// unrecoverable-TTL-reap boundary Backfill and Verify must both respect
// (cerberus issue #2652; see the package doc's "One-time backfill vs. the
// aggregate table's own steady-state TTL" section). A non-positive
// retention means the aggregate table has no TTL clause at all
// (internal/schema/ddl's ttlExpr emits none for a zero duration), so
// nothing can ever be reaped by age: active is false and boundary is the
// zero time.
func retentionBoundary(now time.Time, retention time.Duration) (boundary time.Time, active bool) {
	if retention <= 0 {
		return time.Time{}, false
	}
	return now.Add(-retention), true
}

// outsideRetentionDaysSQL renders the DISTINCT day-bucket read against the
// base table's own DELTA history, restricted to Backfill/Verify's own
// `[-inf, before)` scope AND already past the aggregate table's retention
// as of boundary (BucketStart < boundary) — the exact set of days a
// Backfill write, or a Verify comparison, must treat as structurally
// unrecoverable rather than a real completeness gap (cerberus issue
// #2652). Reads the BASE table, not the aggregate table: the base table
// carries no TTL tied to this retention, so it stays a reliable source of
// truth for "which days exist in scope" regardless of whether the
// aggregate table's own rows for a doomed day have been reaped yet.
func outsideRetentionDaysSQL(c Columns, before, boundary time.Time) (string, []any) {
	bucket := dayBucket(c.TimestampColumn)
	q := chsql.NewQuery().
		Select(bucket).
		From(chsql.Qual(c.Database, c.SumTable)).
		Where(
			chsql.Eq(chsql.Col(c.AggregationTemporalityColumn), chsql.InlineLit(schema.AggregationTemporalityDelta)),
			chsql.Lt(chsql.Col(c.TimestampColumn), chsql.Lit(before)),
			chsql.Lt(bucket, chsql.Lit(boundary)),
		).
		GroupBy(bucket).
		OrderBy(bucket, false)
	return q.Build()
}

// queryOutsideRetentionDays runs outsideRetentionDaysSQL against conn and
// scans the resulting day-buckets, sorted ascending (ORDER BY in the SQL
// itself).
func queryOutsideRetentionDays(ctx context.Context, conn Conn, c Columns, before, boundary time.Time) ([]time.Time, error) {
	sql, args := outsideRetentionDaysSQL(c, before, boundary)
	rows, err := conn.Query(withCaps(ctx), sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var days []time.Time
	for rows.Next() {
		var day time.Time
		if err := rows.Scan(&day); err != nil {
			return nil, err
		}
		days = append(days, day)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return days, nil
}

// BackfillResult is what Backfill returns beyond a plain error: the days
// (if any) it detected as already outside the DeltaPrefixTable's own
// retention window as of now. Non-empty means this backfill wrote — or is
// about to have written — rows ClickHouse's own background TTL merge will
// reap almost immediately, often before an operator gets to run Verify
// (cerberus issue #2652). This is a warning signal, not necessarily a
// problem the caller must treat as fatal: the operator may have already
// accepted the loss for those days, or be intentionally re-running for the
// still-recoverable ones.
type BackfillResult struct {
	OutsideRetentionDays []time.Time
}

// Backfill executes BackfillSQL against conn. retention is the
// DeltaPrefixTable's own resolved TTL (0 = no TTL, see retentionBoundary);
// Backfill checks — against the base table, BEFORE issuing its own INSERT,
// so the check cannot race the aggregate table's TTL reaping the very rows
// it is about to write — whether any day within c's `[-inf, before)` scope
// is already outside that retention, and reports it on the returned
// BackfillResult rather than succeeding silently (cerberus issue #2652).
func Backfill(ctx context.Context, conn Conn, c Columns, before time.Time, retention time.Duration) (BackfillResult, error) {
	boundary, active := retentionBoundary(nowFunc(), retention)
	var outsideDays []time.Time
	if active {
		var err error
		outsideDays, err = queryOutsideRetentionDays(ctx, conn, c, before, boundary)
		if err != nil {
			return BackfillResult{}, fmt.Errorf("deltaprefix: check retention window against %s: %w", c.SumTable, err)
		}
	}

	sql, args := BackfillSQL(c, before)
	if err := conn.Exec(withCaps(ctx), sql, args...); err != nil {
		return BackfillResult{}, fmt.Errorf("deltaprefix: backfill %s: %w", c.DeltaPrefixTable, err)
	}
	return BackfillResult{OutsideRetentionDays: outsideDays}, nil
}

// aggregateTotalsSQL renders the per-metric-name sum(PartialSum) read from
// the DELTA-prefix table, bounded the same exact instant BackfillSQL bounds
// its own write — strictly before before, not before's calendar day. Using
// the exact instant here (rather than rounding down to the cutover day, as
// an earlier revision did) is what makes this comparison actually capable of
// catching a backfill/MV coverage gap confined to the cutover day itself:
// rounding both this and baseTotalsSQL's bound to the SAME calendar day
// excluded that day's bucket from the comparison entirely, so a gap inside
// it was invisible to Verify no matter how badly BackfillSQL under-covered
// it. DeltaPrefixBucketColumn is calendar-day granularity, so this compares
// a day-truncated column against an exact instant — that is fine: a bucket
// whose day-start is before the instant is still "< before" even when the
// instant itself falls partway through that same day.
//
// When retentionActive, an additional `DeltaPrefixBucketColumn >= boundary`
// bound excludes every day already outside the aggregate table's own TTL as
// of now (cerberus issue #2652) — those days are reported separately via
// queryOutsideRetentionDays, never folded into this total.
func aggregateTotalsSQL(c Columns, before, boundary time.Time, retentionActive bool) (string, []any) {
	conds := []chsql.Frag{chsql.Lt(chsql.Col(c.DeltaPrefixBucketColumn), chsql.Lit(before))}
	if retentionActive {
		conds = append(conds, chsql.Gte(chsql.Col(c.DeltaPrefixBucketColumn), chsql.Lit(boundary)))
	}
	q := chsql.NewQuery().
		Select(chsql.Col(c.MetricNameColumn), chsql.Call("sum", chsql.Col(c.DeltaPrefixSumColumn))).
		From(chsql.Qual(c.Database, c.DeltaPrefixTable)).
		Where(conds...).
		GroupBy(chsql.Col(c.MetricNameColumn))
	return q.Build()
}

// baseTotalsSQL renders the per-metric-name sum(Value) read from the base
// sum table, restricted to DELTA-temporality rows strictly before before —
// the exact population BackfillSQL writes into the DELTA-prefix table, so a
// correct backfill makes this total and aggregateTotalsSQL's total agree per
// metric name.
//
// When retentionActive, an additional `toStartOfDay(TimestampColumn) >=
// boundary` bound excludes the same already-outside-retention days
// aggregateTotalsSQL excludes — compared at DAY granularity (not the exact
// instant the `< before` bound uses), matching DeltaPrefixBucketColumn's own
// day-granularity storage resolution and outsideRetentionDaysSQL's day list
// exactly, so both sides of the comparison drop precisely the same days
// (cerberus issue #2652).
func baseTotalsSQL(c Columns, before, boundary time.Time, retentionActive bool) (string, []any) {
	conds := []chsql.Frag{
		chsql.Eq(chsql.Col(c.AggregationTemporalityColumn), chsql.InlineLit(schema.AggregationTemporalityDelta)),
		chsql.Lt(chsql.Col(c.TimestampColumn), chsql.Lit(before)),
	}
	if retentionActive {
		conds = append(conds, chsql.Gte(dayBucket(c.TimestampColumn), chsql.Lit(boundary)))
	}
	q := chsql.NewQuery().
		Select(chsql.Col(c.MetricNameColumn), chsql.Call("sum", chsql.Col(c.ValueColumn))).
		From(chsql.Qual(c.Database, c.SumTable)).
		Where(conds...).
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

	// Retention is the DeltaPrefixTable's own resolved TTL retention as of
	// this Verify run (CERBERUS_SCHEMA_TTL_METRICS, inherited from
	// CERBERUS_SCHEMA_TTL when unset — see internal/schemaboot.
	// MetricsRetention). 0 means no TTL clause is emitted at all
	// (internal/schema/ddl's ttlExpr), so no day can ever be structurally
	// unrecoverable and OutsideRetentionDays is always empty.
	Retention time.Duration

	// RetentionBoundary is the instant this Report's own run computed as
	// now - Retention (the zero time when Retention is 0). A day whose
	// BucketStart falls strictly before this instant is already past its
	// own TTL as of right now — see OutsideRetentionDays.
	RetentionBoundary time.Time

	// OutsideRetentionDays are day-buckets, within before's own scope,
	// whose BucketStart + Retention has already elapsed as of this
	// Report's run time (cerberus issue #2652): a one-time backfill
	// writing rows for one of these days writes data ClickHouse's own
	// background TTL merge reaps almost immediately, and no sequence of
	// backfill re-runs can ever recover it — the constraint is the row's
	// own age, not the write path. These days are EXCLUDED from
	// AggregateTotals / BaseTotals / Mismatches above: folding an
	// unrecoverable day into the same FAIL as a genuine completeness gap
	// would be indistinguishable from a real bug. See docs/operations.md's
	// DELTA-prefix backfill runbook.
	OutsideRetentionDays []time.Time
}

// Pass reports whether every metric's totals agreed within tolerance.
// OutsideRetentionDays does not affect Pass: those days are excluded from
// the comparison entirely, not counted as failures (see the field's doc).
func (r Report) Pass() bool { return len(r.Mismatches) == 0 }

// Verify reads both per-metric totals and diffs them, returning a Report an
// operator (or the `delta-prefix-verify` CLI verb) can render before
// flipping CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED=true. retention is the
// DeltaPrefixTable's own resolved TTL (0 = no TTL); when non-zero, Verify
// first detects any day already outside that retention as of now — see
// retentionBoundary — excludes it from both totals reads, and reports it
// separately on Report.OutsideRetentionDays rather than an unexplained
// mismatch (cerberus issue #2652).
func Verify(ctx context.Context, conn Conn, c Columns, before time.Time, tolerance float64, retention time.Duration) (Report, error) {
	boundary, active := retentionBoundary(nowFunc(), retention)

	var outsideDays []time.Time
	if active {
		var err error
		outsideDays, err = queryOutsideRetentionDays(ctx, conn, c, before, boundary)
		if err != nil {
			return Report{}, fmt.Errorf("deltaprefix: check retention window against %s: %w", c.SumTable, err)
		}
	}

	aggSQL, aggArgs := aggregateTotalsSQL(c, before, boundary, active)
	agg, err := scanTotals(ctx, conn, aggSQL, aggArgs)
	if err != nil {
		return Report{}, fmt.Errorf("deltaprefix: read %s totals: %w", c.DeltaPrefixTable, err)
	}
	baseSQL, baseArgs := baseTotalsSQL(c, before, boundary, active)
	base, err := scanTotals(ctx, conn, baseSQL, baseArgs)
	if err != nil {
		return Report{}, fmt.Errorf("deltaprefix: read %s totals: %w", c.SumTable, err)
	}

	rep := Diff(agg, base, before, tolerance)
	rep.Retention = retention
	if active {
		rep.RetentionBoundary = boundary
	}
	rep.OutsideRetentionDays = outsideDays
	return rep, nil
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
