package downsampletier

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	chdriver "github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// settingExperimentalTSGridAggregate MUST stay byte-identical to
// chclient.SettingExperimentalTSGridAggregate — this package cannot import
// internal/chclient (.go-arch-lint.yml: this leaf package, like
// internal/deltaprefix and internal/routerrules, must not depend on
// chclient; the CLI supplies the resolved config + the narrow Conn at boot)
// so the setting's exact string KEY is duplicated here rather than shared,
// the same "leaf packages can't share one helper" constraint
// bucketEndExpr's own doc explains for internal/schema/ddl.
// TestSettingMatchesChclient pins the two against each other. See
// chclient.SettingExperimentalTSGridAggregate's own doc for the full
// history of why this exact spelling (not the deprecated
// allow_experimental_ts_to_grid_aggregate_function alias) is the one that
// must never drift.
const settingExperimentalTSGridAggregate = "allow_experimental_time_series_aggregate_functions"

// Conn is the narrow ClickHouse surface this package needs — mirrors
// internal/deltaprefix's own Conn exactly (Exec for the
// backfill/rebuild/truncate statements, Query for verify's two completeness
// reads), for the same reason: this is a leaf package that should not pull
// in internal/chclient's full Client surface for a type name alone.
type Conn interface {
	Exec(ctx context.Context, query string, args ...any) error
	Query(ctx context.Context, query string, args ...any) (chdriver.Rows, error)
}

// Resource caps stamped on every statement this package issues — the SAME
// values and the SAME rationale as internal/deltaprefix's own maxExecutionTimeSeconds
// / maxThreads / priority / maxRowsToRead / maxBytesToRead: an
// operator-run, off-hot-path tool sharing a ClickHouse cluster with live
// query serving.
const (
	maxExecutionTimeSeconds = 900.0 // 15 minutes
	maxThreads              = 4
	priority                = 10
	maxRowsToRead           = 20_000_000_000
	maxBytesToRead          = 256 << 30 // 256 GiB
)

// withCaps stamps the conservative resource-cap settings PLUS the
// experimental timeSeries*ToGrid gate onto ctx via clickhouse-go's
// client-side query-settings mechanism. Unlike internal/deltaprefix this
// package's statements reference AggregateFunction(timeSeriesLastTwoSamples,
// ...) / call timeSeriesLastTwoSamplesState — ClickHouse rejects both unless
// settingExperimentalTSGridAggregate is set on the SAME session
// (UNKNOWN_AGGREGATE_FUNCTION otherwise; verified empirically against a
// real ClickHouse instance — see internal/chsql's downsample-tier chDB
// test). This package Execs/Queries a raw driver.Conn directly (like
// internal/deltaprefix), not through internal/chclient.Client, so it must
// stamp the clickhouse-go client-side settings map itself rather than
// route through chclient.WithTSGridSetting's composable per-request
// carrier (which only chclient.Client's own query path reads).
func withCaps(ctx context.Context) context.Context {
	return clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"max_execution_time":               maxExecutionTimeSeconds,
		"timeout_overflow_mode":            "throw",
		"max_threads":                      maxThreads,
		"priority":                         priority,
		"max_rows_to_read":                 maxRowsToRead,
		"max_bytes_to_read":                maxBytesToRead,
		"read_overflow_mode":               "break",
		settingExperimentalTSGridAggregate: 1,
	}))
}

// Columns names the physical database/table/column identifiers the
// backfill + rebuild + verify queries reference. Unlike
// internal/deltaprefix.Columns, the tier table/column names are FIXED
// schema.DownsampleTier* constants (see that package's doc for why), so
// only the database + base Sum table name vary by deployment.
type Columns struct {
	Database                     string
	SumTable                     string
	MetricNameColumn             string
	AttributesColumn             string
	ResourceAttributesColumn     string
	ServiceNameColumn            string
	TimestampColumn              string
	ValueColumn                  string
	AggregationTemporalityColumn string
}

// FromSchema builds Columns from the ClickHouse database name plus the
// resolved runtime schema.Metrics (config.Config.Schema) — mirrors
// internal/deltaprefix.FromSchema.
func FromSchema(database string, m schema.Metrics) Columns {
	return Columns{
		Database:                     database,
		SumTable:                     m.SumTable,
		MetricNameColumn:             m.MetricNameColumn,
		AttributesColumn:             m.AttributesColumn,
		ResourceAttributesColumn:     m.ResourceAttributesColumn,
		ServiceNameColumn:            m.ServiceNameColumn,
		TimestampColumn:              m.TimestampColumn,
		ValueColumn:                  m.ValueColumn,
		AggregationTemporalityColumn: m.AggregationTemporalityColumn,
	}
}

// bucketEndExpr renders the SAME ceiling bucket-boundary expression
// internal/schema/ddl's downsampleTierBucketEndExpr renders for the live
// MV — see that function's doc for the full PromQL-half-open-window
// reasoning. The two copies MUST stay byte-identical (a backfilled row and
// a live-MV row for the same raw sample must land in the same bucket); they
// cannot share a Go func because internal/schema/ddl and this package are
// both leaf packages neither imports the other's exported surface for one
// shared helper alone — TestBucketEndExprMatchesDDL pins the two against
// each other.
func bucketEndExpr(col string) chsql.Frag {
	epsilon := chsql.Sub(chsql.Col(col), chsql.Call("toIntervalNanosecond", chsql.InlineLit(int64(1))))
	floor := chsql.Call("toStartOfInterval", epsilon, chsql.Call("toIntervalSecond", chsql.InlineLit(bucketSeconds())))
	return chsql.Add(floor, chsql.Call("toIntervalSecond", chsql.InlineLit(bucketSeconds())))
}

func bucketSeconds() int64 { return int64(schema.DownsampleTierBucket / time.Second) }

// backfillSelectSQL renders the SELECT half shared by BackfillSQL (bounded
// by `before`) and RebuildSQL (unbounded, the full history) — factored out
// so the two statements cannot drift on the GROUP BY / projection shape.
func backfillSelectSQL(c Columns, before *time.Time) *chsql.QueryBuilder {
	bucket := bucketEndExpr(c.TimestampColumn)
	q := chsql.NewQuery().
		Select(
			chsql.Col(c.MetricNameColumn),
			chsql.Col(c.AttributesColumn),
			chsql.Col(c.ResourceAttributesColumn),
			chsql.Col(c.ServiceNameColumn),
			chsql.As(bucket, schema.DownsampleTierBucketColumn),
			chsql.As(chsql.Call("timeSeriesLastTwoSamplesState", chsql.Col(c.TimestampColumn), chsql.Col(c.ValueColumn)), schema.DownsampleTierSamplesColumn),
			chsql.As(chsql.Call("any", chsql.Col(c.AggregationTemporalityColumn)), schema.DownsampleTierTemporalityColumn),
		).
		From(chsql.Qual(c.Database, c.SumTable))
	if before != nil {
		q = q.Where(chsql.Lt(chsql.Col(c.TimestampColumn), chsql.Lit(*before)))
	}
	return q.GroupBy(
		chsql.Col(c.MetricNameColumn),
		chsql.Col(c.AttributesColumn),
		chsql.Col(c.ResourceAttributesColumn),
		chsql.Col(c.ServiceNameColumn),
		bucket,
	)
}

// tierColumns is the target column list BackfillSQL / RebuildSQL insert
// into, in the SAME order backfillSelectSQL projects them.
func tierColumns(c Columns) []string {
	return []string{
		c.MetricNameColumn, c.AttributesColumn, c.ResourceAttributesColumn, c.ServiceNameColumn,
		schema.DownsampleTierBucketColumn, schema.DownsampleTierSamplesColumn, schema.DownsampleTierTemporalityColumn,
	}
}

// BackfillSQL renders the one-time `INSERT INTO ... SELECT` that populates
// the tier table for c.SumTable history strictly older than before — the
// MV's own exact creation instant (see internal/deltaprefix's package doc
// for why an exact instant, never a calendar-day-rounded one). Mirrors
// internal/deltaprefix.BackfillSQL's own bound discipline exactly.
func BackfillSQL(c Columns, before time.Time) (string, []any) {
	return chsql.InsertSelect(c.Database, schema.DownsampleTierTable, tierColumns(c), backfillSelectSQL(c, &before)).Build()
}

// RebuildSQL renders the FULL, unbounded `INSERT INTO ... SELECT` a Rebuild
// issues after truncating the tier table (see this package's doc for why a
// full rebuild — not just an incremental backfill — is the recovery path
// for a suspected stranded/incompatible persisted state).
func RebuildSQL(c Columns) (string, []any) {
	return chsql.InsertSelect(c.Database, schema.DownsampleTierTable, tierColumns(c), backfillSelectSQL(c, nil)).Build()
}

// TruncateSQL renders the `TRUNCATE TABLE` Rebuild issues before its full
// re-populate.
func TruncateSQL(c Columns) string {
	return chsql.TruncateTable(c.Database, schema.DownsampleTierTable)
}

// nowFunc is the retention-boundary clock — mirrors internal/deltaprefix's
// own package-level var, pinned in tests the same way.
var nowFunc = time.Now

// retentionBoundary mirrors internal/deltaprefix.retentionBoundary exactly
// — see that function's doc.
func retentionBoundary(now time.Time, retention time.Duration) (boundary time.Time, active bool) {
	if retention <= 0 {
		return time.Time{}, false
	}
	return now.Add(-retention), true
}

// outsideRetentionDaysSQL renders the DISTINCT bucket-boundary read against
// the base table's own history, restricted to Backfill's own `[-inf,
// before)` scope AND already past the tier table's retention as of
// boundary — the tier's analogue of internal/deltaprefix's own
// outsideRetentionDaysSQL (cerberus issue #2652's finding, reproduced here
// rather than re-derived: this table shares the base Sum table's TTL, see
// internal/schema/ddl's renderDownsampleTierTable).
func outsideRetentionDaysSQL(c Columns, before, boundary time.Time) (string, []any) {
	bucket := bucketEndExpr(c.TimestampColumn)
	q := chsql.NewQuery().
		Select(bucket).
		From(chsql.Qual(c.Database, c.SumTable)).
		Where(
			chsql.Lt(chsql.Col(c.TimestampColumn), chsql.Lit(before)),
			chsql.Lt(bucket, chsql.Lit(boundary)),
		).
		GroupBy(bucket).
		OrderBy(bucket, false)
	return q.Build()
}

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

// Result is what Backfill and Rebuild return beyond a plain error — mirrors
// internal/deltaprefix.BackfillResult.
type Result struct {
	OutsideRetentionDays []time.Time
}

// Backfill executes BackfillSQL against conn — the incremental, non-
// retroactive one-time historical pass. retention is the tier table's own
// resolved TTL (0 = no TTL); see Result.OutsideRetentionDays and this
// package's doc for the retention-boundary hazard.
func Backfill(ctx context.Context, conn Conn, c Columns, before time.Time, retention time.Duration) (Result, error) {
	boundary, active := retentionBoundary(nowFunc(), retention)
	var outsideDays []time.Time
	if active {
		var err error
		outsideDays, err = queryOutsideRetentionDays(ctx, conn, c, before, boundary)
		if err != nil {
			return Result{}, fmt.Errorf("downsampletier: check retention window against %s: %w", c.SumTable, err)
		}
	}
	sql, args := BackfillSQL(c, before)
	if err := conn.Exec(withCaps(ctx), sql, args...); err != nil {
		return Result{}, fmt.Errorf("downsampletier: backfill %s: %w", schema.DownsampleTierTable, err)
	}
	return Result{OutsideRetentionDays: outsideDays}, nil
}

// Rebuild TRUNCATEs the tier table and re-populates it in full — the
// recovery path for a suspected stranded or format-incompatible persisted
// state (see this package's doc). now (the retention-boundary clock) is
// used exactly as Backfill uses it, but the scanned history has no `before`
// bound: EVERY day the base table still retains is in scope, so
// OutsideRetentionDays reports days already outside the tier's TTL as of
// right now across the base table's FULL retained history, not just a
// caller-chosen window.
func Rebuild(ctx context.Context, conn Conn, c Columns, retention time.Duration) (Result, error) {
	var outsideDays []time.Time
	if boundary, active := retentionBoundary(nowFunc(), retention); active {
		var err error
		// before = now: the same "[-inf, before)" scope Backfill's own check
		// uses, with before pinned to now so the scope is the base table's
		// entire currently-retained history.
		outsideDays, err = queryOutsideRetentionDays(ctx, conn, c, nowFunc(), boundary)
		if err != nil {
			return Result{}, fmt.Errorf("downsampletier: check retention window against %s: %w", c.SumTable, err)
		}
	}
	if err := conn.Exec(withCaps(ctx), TruncateSQL(c)); err != nil {
		return Result{}, fmt.Errorf("downsampletier: truncate %s: %w", schema.DownsampleTierTable, err)
	}
	sql, args := RebuildSQL(c)
	if err := conn.Exec(withCaps(ctx), sql, args...); err != nil {
		return Result{}, fmt.Errorf("downsampletier: rebuild %s: %w", schema.DownsampleTierTable, err)
	}
	return Result{OutsideRetentionDays: outsideDays}, nil
}

// baseBucketsSQL / tierBucketsSQL render the per-metric DISTINCT bucket
// count Verify compares — a COMPLETENESS check (does every bucket the base
// table has raw data for also have a tier row), not a value-parity check
// like internal/deltaprefix.Verify's sum comparison. See this package's doc
// for why: the tier's read-side failure mode for a missing bucket is a safe
// absent series point, never a wrong number, so there is no aggregate total
// to parity-check in the first place.
func baseBucketsSQL(c Columns, before, boundary time.Time, retentionActive bool) (string, []any) {
	bucket := bucketEndExpr(c.TimestampColumn)
	conds := []chsql.Frag{chsql.Lt(chsql.Col(c.TimestampColumn), chsql.Lit(before))}
	if retentionActive {
		conds = append(conds, chsql.Gte(bucket, chsql.Lit(boundary)))
	}
	q := chsql.NewQuery().
		Select(chsql.Col(c.MetricNameColumn), chsql.As(chsql.Call("uniqExact", bucket), "n")).
		From(chsql.Qual(c.Database, c.SumTable)).
		Where(conds...).
		GroupBy(chsql.Col(c.MetricNameColumn))
	return q.Build()
}

func tierBucketsSQL(c Columns, before, boundary time.Time, retentionActive bool) (string, []any) {
	bucketCol := chsql.Col(schema.DownsampleTierBucketColumn)
	conds := []chsql.Frag{chsql.Lt(bucketCol, chsql.Lit(before))}
	if retentionActive {
		conds = append(conds, chsql.Gte(bucketCol, chsql.Lit(boundary)))
	}
	q := chsql.NewQuery().
		Select(chsql.Col(c.MetricNameColumn), chsql.As(chsql.Call("uniqExact", bucketCol), "n")).
		From(chsql.Qual(c.Database, schema.DownsampleTierTable)).
		Where(conds...).
		GroupBy(chsql.Col(c.MetricNameColumn))
	return q.Build()
}

// Mismatch is one metric name whose base-table bucket count disagrees with
// the tier table's own bucket count — Missing is how many of the base
// table's buckets have no corresponding tier row (base - tier, floored at
// 0; a tier count that legitimately EXCEEDS the base count is not an error
// here — see BaseBuckets/TierBuckets' own doc — so Missing never goes
// negative).
type Mismatch struct {
	MetricName  string
	BaseBuckets int64
	TierBuckets int64
}

// Missing is BaseBuckets - TierBuckets, floored at 0.
func (m Mismatch) Missing() int64 {
	if d := m.BaseBuckets - m.TierBuckets; d > 0 {
		return d
	}
	return 0
}

// Report is Verify's result — mirrors internal/deltaprefix.Report's shape
// with BUCKET COUNTS in place of SUM TOTALS (see this package's doc).
type Report struct {
	Before      time.Time
	BaseBuckets map[string]int64
	TierBuckets map[string]int64
	Mismatches  []Mismatch

	Retention            time.Duration
	RetentionBoundary    time.Time
	OutsideRetentionDays []time.Time
}

// Pass reports whether every metric's bucket count matched.
// OutsideRetentionDays does not affect Pass — see Report.OutsideRetentionDays.
func (r Report) Pass() bool { return len(r.Mismatches) == 0 }

// Verify reads both per-metric bucket counts and diffs them, returning a
// Report an operator (or the `downsample-tier-verify` CLI verb) can render
// before relying on the tier for long-range queries.
func Verify(ctx context.Context, conn Conn, c Columns, before time.Time, retention time.Duration) (Report, error) {
	boundary, active := retentionBoundary(nowFunc(), retention)

	var outsideDays []time.Time
	if active {
		var err error
		outsideDays, err = queryOutsideRetentionDays(ctx, conn, c, before, boundary)
		if err != nil {
			return Report{}, fmt.Errorf("downsampletier: check retention window against %s: %w", c.SumTable, err)
		}
	}

	baseSQL, baseArgs := baseBucketsSQL(c, before, boundary, active)
	base, err := scanCounts(ctx, conn, baseSQL, baseArgs)
	if err != nil {
		return Report{}, fmt.Errorf("downsampletier: read %s bucket counts: %w", c.SumTable, err)
	}
	tierSQL, tierArgs := tierBucketsSQL(c, before, boundary, active)
	tier, err := scanCounts(ctx, conn, tierSQL, tierArgs)
	if err != nil {
		return Report{}, fmt.Errorf("downsampletier: read %s bucket counts: %w", schema.DownsampleTierTable, err)
	}

	rep := Diff(base, tier, before)
	rep.Retention = retention
	if active {
		rep.RetentionBoundary = boundary
	}
	rep.OutsideRetentionDays = outsideDays
	return rep, nil
}

// Diff compares two per-metric-name bucket-count maps and returns the
// Report — split out from Verify so the comparison logic is unit-testable
// against fixture maps, without a Conn, mirroring internal/deltaprefix.Diff.
func Diff(base, tier map[string]int64, before time.Time) Report {
	names := make(map[string]struct{}, len(base)+len(tier))
	for name := range base {
		names[name] = struct{}{}
	}
	for name := range tier {
		names[name] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	var mismatches []Mismatch
	for _, name := range sorted {
		b, t := base[name], tier[name]
		if b != t {
			mismatches = append(mismatches, Mismatch{MetricName: name, BaseBuckets: b, TierBuckets: t})
		}
	}
	return Report{
		Before:      before,
		BaseBuckets: base,
		TierBuckets: tier,
		Mismatches:  mismatches,
	}
}

// scanCounts runs sql and scans a (MetricName, count int64) result into a
// map — mirrors internal/deltaprefix.scanTotals.
func scanCounts(ctx context.Context, conn Conn, sql string, args []any) (map[string]int64, error) {
	rows, err := conn.Query(withCaps(ctx), sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
