package chsql

import (
	"testing"
	"time"
)

// renderFrag is a tiny helper: render a standalone Frag to its SQL string.
func renderFrag(f Frag) string {
	sql, _ := Render(f)
	return sql
}

// TestOnCluster pins the ON CLUSTER clause: the keyword plus a
// backtick-quoted cluster name, with embedded backticks doubled.
func TestOnCluster(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "prod", "ON CLUSTER `prod`"},
		{"with_dash", "ch-prod", "ON CLUSTER `ch-prod`"},
		{"embedded_backtick", "a`b", "ON CLUSTER `a``b`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderFrag(OnCluster(tc.in)); got != tc.want {
				t.Errorf("OnCluster(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDatabaseEngineReplicated pins the Replicated database engine clause —
// the three string-literal args, single-quoted, with the server macros
// passed through verbatim.
func TestDatabaseEngineReplicated(t *testing.T) {
	got := renderFrag(DatabaseEngineReplicated("/clickhouse/databases/otel", "{shard}", "{replica}"))
	want := "Replicated('/clickhouse/databases/otel', '{shard}', '{replica}')"
	if got != want {
		t.Errorf("DatabaseEngineReplicated = %q; want %q", got, want)
	}
}

// TestEngineReplicatedMergeTree pins the BARE ReplicatedMergeTree table-engine
// clause — no arguments. This is the form a Replicated database requires: the
// database supplies the Keeper path / replica, and ClickHouse 24.8+ rejects
// explicit (path, replica) args there with code 36. A Replicated database does
// not auto-convert MergeTree, so this bare engine is what cerberus emits to
// replicate table DATA.
func TestEngineReplicatedMergeTree(t *testing.T) {
	got := renderFrag(EngineReplicatedMergeTree())
	want := "ReplicatedMergeTree"
	if got != want {
		t.Errorf("EngineReplicatedMergeTree = %q; want %q", got, want)
	}
}

// TestTableTTL pins the TTL clause across every rounding bucket and the
// no-TTL (nil Frag) case. The column is wrapped in toDateTime(...) as a
// bare identifier, matching the upstream template form.
func TestTableTTL(t *testing.T) {
	if f := TableTTL("TimeUnix", 0); f != nil {
		t.Errorf("TableTTL with d=0 must return nil, got %q", renderFrag(f))
	}
	if f := TableTTL("TimeUnix", -time.Hour); f != nil {
		t.Errorf("TableTTL with negative d must return nil, got %q", renderFrag(f))
	}
	cases := []struct {
		name string
		col  string
		d    time.Duration
		want string
	}{
		{"week", "Timestamp", 14 * 24 * time.Hour, "TTL toDateTime(Timestamp) + toIntervalWeek(2)"},
		{"day", "TimeUnix", 48 * time.Hour, "TTL toDateTime(TimeUnix) + toIntervalDay(2)"},
		// 90 days is not a whole number of weeks, so it stays in days.
		{"day_not_week", "TimeUnix", 90 * 24 * time.Hour, "TTL toDateTime(TimeUnix) + toIntervalDay(90)"},
		{"hour", "Timestamp", 3 * time.Hour, "TTL toDateTime(Timestamp) + toIntervalHour(3)"},
		{"minute", "Start", 90 * time.Minute, "TTL toDateTime(Start) + toIntervalMinute(90)"},
		{"second", "TimeUnix", 45 * time.Second, "TTL toDateTime(TimeUnix) + toIntervalSecond(45)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderFrag(TableTTL(tc.col, tc.d)); got != tc.want {
				t.Errorf("TableTTL(%q, %v) = %q; want %q", tc.col, tc.d, got, tc.want)
			}
		})
	}
}

// TestCreateDatabase pins the CREATE DATABASE statement builder across its
// fluent options: IF NOT EXISTS, ON CLUSTER, and a Replicated ENGINE. The
// database name is emitted bare (matching the established cerberus +
// upstream-exporter form) and the statement carries no positional args.
func TestCreateDatabase(t *testing.T) {
	cases := []struct {
		name string
		stmt *CreateDatabaseBuilder
		want string
	}{
		{
			"bare",
			CreateDatabase("otel"),
			"CREATE DATABASE otel",
		},
		{
			"if_not_exists",
			CreateDatabase("otel").IfNotExists(),
			"CREATE DATABASE IF NOT EXISTS otel",
		},
		{
			"default_db",
			CreateDatabase("default").IfNotExists(),
			"CREATE DATABASE IF NOT EXISTS default",
		},
		{
			"on_cluster",
			CreateDatabase("otel").IfNotExists().OnCluster("prod"),
			"CREATE DATABASE IF NOT EXISTS otel ON CLUSTER `prod`",
		},
		{
			"replicated_engine",
			CreateDatabase("otel").IfNotExists().Engine(
				DatabaseEngineReplicated("/clickhouse/databases/otel", "{shard}", "{replica}"),
			),
			"CREATE DATABASE IF NOT EXISTS otel ENGINE = Replicated('/clickhouse/databases/otel', '{shard}', '{replica}')",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stmt.SQL(); got != tc.want {
				t.Errorf("SQL() = %q; want %q", got, tc.want)
			}
		})
	}
}

// metricNameProjectionBody is the aggregating projection body the metric-name
// catalog enumeration is served from: one row per MetricName carrying its
// max(TimeUnix). Built with the same typed QueryBuilder used for reads.
func metricNameProjectionBody() *QueryBuilder {
	return NewQuery().
		Select(Col("MetricName"), Call("max", Col("TimeUnix"))).
		GroupBy(Col("MetricName"))
}

// TestAlterTableAddProjection pins the ADD PROJECTION statement: the
// fully-qualified <db>.<table>, the idempotent IF NOT EXISTS guard, the
// projection body wrapped in exactly one pair of parentheses, and the
// optional ON CLUSTER clause. The statement carries no positional args.
func TestAlterTableAddProjection(t *testing.T) {
	cases := []struct {
		name string
		stmt *AddProjectionBuilder
		want string
	}{
		{
			"plain",
			AlterTableAddProjection("otel", "otel_metrics_gauge", "proj_metric_name", metricNameProjectionBody()),
			"ALTER TABLE otel.otel_metrics_gauge ADD PROJECTION IF NOT EXISTS proj_metric_name " +
				"(SELECT `MetricName`, max(`TimeUnix`) GROUP BY `MetricName`)",
		},
		{
			"default_db",
			AlterTableAddProjection("default", "otel_metrics_sum", "proj_metric_name", metricNameProjectionBody()),
			"ALTER TABLE default.otel_metrics_sum ADD PROJECTION IF NOT EXISTS proj_metric_name " +
				"(SELECT `MetricName`, max(`TimeUnix`) GROUP BY `MetricName`)",
		},
		{
			"on_cluster",
			AlterTableAddProjection("otel", "otel_metrics_histogram", "proj_metric_name", metricNameProjectionBody()).OnCluster("prod"),
			"ALTER TABLE otel.otel_metrics_histogram ON CLUSTER `prod` ADD PROJECTION IF NOT EXISTS proj_metric_name " +
				"(SELECT `MetricName`, max(`TimeUnix`) GROUP BY `MetricName`)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stmt.SQL(); got != tc.want {
				t.Errorf("SQL() = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestQueryBuilderHaving pins the HAVING clause render: it follows GROUP BY,
// precedes ORDER BY, and AND-joins multiple conditions. HAVING (not WHERE) is
// what lets the metric-name enumeration route to the aggregating projection.
func TestQueryBuilderHaving(t *testing.T) {
	sql, args := NewQuery().
		Select(As(Col("MetricName"), "value")).
		From(Col("otel_metrics_gauge")).
		GroupBy(Col("MetricName")).
		Having(Gte(Call("max", Col("TimeUnix")), InlineLit(int64(1700000000)))).
		OrderBy(Col("value"), false).
		Build()
	want := "SELECT `MetricName` AS `value` FROM `otel_metrics_gauge` GROUP BY `MetricName` " +
		"HAVING max(`TimeUnix`) >= 1700000000 ORDER BY `value`"
	if sql != want {
		t.Errorf("Build() = %q; want %q", sql, want)
	}
	if len(args) != 0 {
		t.Errorf("inline HAVING must bind no args, got %v", args)
	}

	multi, _ := NewQuery().
		Select(Col("MetricName")).
		From(Col("t")).
		GroupBy(Col("MetricName")).
		Having(
			Gte(Call("max", Col("TimeUnix")), InlineLit(int64(1))),
			Lte(Call("min", Col("TimeUnix")), InlineLit(int64(2))),
		).
		Build()
	wantMulti := "SELECT `MetricName` FROM `t` GROUP BY `MetricName` " +
		"HAVING max(`TimeUnix`) >= 1 AND min(`TimeUnix`) <= 2"
	if multi != wantMulti {
		t.Errorf("multi-HAVING Build() = %q; want %q", multi, wantMulti)
	}
}

// TestRenderDDL_PanicsOnBoundArg locks the DDL no-bindings invariant: a
// fragment that binds a positional `?` (here via Lit) must panic rather
// than silently drop the binding and emit an unfillable `?` into the DDL.
// This is what makes the DDL render path safe to return a bare string.
func TestRenderDDL_PanicsOnBoundArg(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("RenderDDL must panic when the fragment binds positional args")
		}
	}()
	_ = RenderDDL(Lit(5)) // Lit emits a `?` and binds 5 — illegal in DDL
}

// TestRenderDDL_InlineValuesOK confirms the legitimate DDL path: inline
// values (InlineLit / Call) bind nothing, so RenderDDL returns the text
// without panicking.
func TestRenderDDL_InlineValuesOK(t *testing.T) {
	if got := RenderDDL(Call("toIntervalDay", InlineLit(int64(2)))); got != "toIntervalDay(2)" {
		t.Errorf("RenderDDL = %q; want toIntervalDay(2)", got)
	}
}
