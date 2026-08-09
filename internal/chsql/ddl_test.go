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

// TestTableTTLTiered pins the hot/cold clause: ClickHouse takes ONE TTL clause
// per table whose actions are comma-separated, so the move rule and the delete
// rule land in the same clause, move first. The degenerate inputs are the
// backward-compatibility contract — with no volume or no move age the output
// must be byte-identical to TableTTL, because that is what every deployment
// that never configures tiering renders.
func TestTableTTLTiered(t *testing.T) {
	const (
		day      = 24 * time.Hour
		coldVol  = "cold"
		moveWeek = 7 * day
		keep30d  = 30 * day
	)
	cases := []struct {
		name      string
		col       string
		retention time.Duration
		moveAfter time.Duration
		volume    string
		want      string
	}{
		{
			name: "move_and_delete", col: "Timestamp",
			retention: keep30d, moveAfter: moveWeek, volume: coldVol,
			want: "TTL toDateTime(Timestamp) + toIntervalWeek(1) TO VOLUME 'cold', " +
				"toDateTime(Timestamp) + toIntervalDay(30) DELETE",
		},
		{
			name: "move_only_no_retention", col: "TimeUnix",
			retention: 0, moveAfter: moveWeek, volume: coldVol,
			want: "TTL toDateTime(TimeUnix) + toIntervalWeek(1) TO VOLUME 'cold'",
		},
		{
			// No volume: the move age alone cannot name a destination, so the
			// clause degrades to the retention-only form (implicit DELETE).
			name: "no_volume_degrades_to_delete_only", col: "Timestamp",
			retention: keep30d, moveAfter: moveWeek, volume: "",
			want: "TTL toDateTime(Timestamp) + toIntervalDay(30)",
		},
		{
			// No move age: same degradation, from the other side.
			name: "no_move_age_degrades_to_delete_only", col: "Start",
			retention: keep30d, moveAfter: 0, volume: coldVol,
			want: "TTL toDateTime(Start) + toIntervalDay(30)",
		},
		{
			// A volume name is a CH string literal, so an embedded quote is
			// escaped rather than closing the literal early.
			name: "volume_name_is_quoted_literal", col: "Timestamp",
			retention: 0, moveAfter: day, volume: "co'ld",
			want: `TTL toDateTime(Timestamp) + toIntervalDay(1) TO VOLUME 'co\'ld'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderFrag(TableTTLTiered(tc.col, tc.retention, tc.moveAfter, tc.volume))
			if got != tc.want {
				t.Errorf("TableTTLTiered = %q; want %q", got, tc.want)
			}
		})
	}

	// Neither rule → no clause at all (nil), the same contract TableTTL has.
	if f := TableTTLTiered("Timestamp", 0, 0, coldVol); f != nil {
		t.Errorf("TableTTLTiered with no retention and no move age must be nil, got %q", renderFrag(f))
	}

	// The non-tiered path is byte-identical to TableTTL for every bucket, which
	// is what keeps existing deployments' DDL unchanged.
	for _, d := range []time.Duration{45 * time.Second, 90 * time.Minute, 3 * time.Hour, 90 * day, 14 * day} {
		plain := renderFrag(TableTTL("Timestamp", d))
		tiered := renderFrag(TableTTLTiered("Timestamp", d, 0, ""))
		if plain != tiered {
			t.Errorf("untiered TableTTLTiered(%v) = %q; want TableTTL's %q", d, tiered, plain)
		}
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

// seriesProjectionBody is the curated proj_series body: one row per
// (MetricName, Attributes) carrying max(TimeUnix). It serves every windowless
// enumeration shape (label_values, label-names, __name__, cardinality). Built
// with the same typed QueryBuilder used for reads.
func seriesProjectionBody() *QueryBuilder {
	return NewQuery().
		Select(Col("MetricName"), Col("Attributes"), Call("max", Col("TimeUnix"))).
		GroupBy(Col("MetricName"), Col("Attributes"))
}

// metadataProjectionBody is the curated proj_metric_metadata body: one row per
// MetricName carrying any(MetricDescription)/any(MetricUnit) + max(TimeUnix),
// serving the windowless /api/v1/metadata listing.
func metadataProjectionBody() *QueryBuilder {
	return NewQuery().
		Select(Col("MetricName"), Call("any", Col("MetricDescription")), Call("any", Col("MetricUnit")), Call("max", Col("TimeUnix"))).
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
			"series_plain",
			AlterTableAddProjection("otel", "otel_metrics_gauge", "proj_series", seriesProjectionBody()),
			"ALTER TABLE otel.otel_metrics_gauge ADD PROJECTION IF NOT EXISTS proj_series " +
				"(SELECT `MetricName`, `Attributes`, max(`TimeUnix`) GROUP BY `MetricName`, `Attributes`)",
		},
		{
			"series_default_db",
			AlterTableAddProjection("default", "otel_metrics_sum", "proj_series", seriesProjectionBody()),
			"ALTER TABLE default.otel_metrics_sum ADD PROJECTION IF NOT EXISTS proj_series " +
				"(SELECT `MetricName`, `Attributes`, max(`TimeUnix`) GROUP BY `MetricName`, `Attributes`)",
		},
		{
			"metadata_on_cluster",
			AlterTableAddProjection("otel", "otel_metrics_histogram", "proj_metric_metadata", metadataProjectionBody()).OnCluster("prod"),
			"ALTER TABLE otel.otel_metrics_histogram ON CLUSTER `prod` ADD PROJECTION IF NOT EXISTS proj_metric_metadata " +
				"(SELECT `MetricName`, any(`MetricDescription`), any(`MetricUnit`), max(`TimeUnix`) GROUP BY `MetricName`)",
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

// TestAlterTableModifyColumn pins the MODIFY COLUMN statement: the optional
// <db>. qualifier, the idempotent IF EXISTS guard, the quoted column name, the
// caller's type fragment, and the optional ON CLUSTER clause. The statement
// carries no positional args, so RenderDDL accepts it.
func TestAlterTableModifyColumn(t *testing.T) {
	enum := TypeEnum8(EnumPair{Name: "ok", Value: 0}, EnumPair{Name: "error", Value: 1})
	cases := []struct {
		name string
		stmt *ModifyColumnBuilder
		want string
	}{
		{
			"unqualified_table",
			AlterTableModifyColumn("", "cerberus_router_corpus", "exit_status", enum),
			"ALTER TABLE cerberus_router_corpus MODIFY COLUMN IF EXISTS `exit_status` " +
				"Enum8('ok' = 0, 'error' = 1)",
		},
		{
			"qualified_table",
			AlterTableModifyColumn("otel", "cerberus_router_corpus", "exit_status", enum),
			"ALTER TABLE otel.cerberus_router_corpus MODIFY COLUMN IF EXISTS `exit_status` " +
				"Enum8('ok' = 0, 'error' = 1)",
		},
		{
			"on_cluster",
			AlterTableModifyColumn("otel", "cerberus_router_corpus", "exit_status", enum).OnCluster("prod"),
			"ALTER TABLE otel.cerberus_router_corpus ON CLUSTER `prod` MODIFY COLUMN IF EXISTS `exit_status` " +
				"Enum8('ok' = 0, 'error' = 1)",
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

// TestAlterTableAddColumn pins the ADD COLUMN statement: the optional <db>.
// qualifier, the idempotent IF NOT EXISTS guard, the quoted column name, the
// caller's type fragment, and the optional ON CLUSTER clause. The guard is IF
// NOT EXISTS (not MODIFY's IF EXISTS) because the statement exists precisely
// for the case where the column is absent. It carries no positional args, so
// RenderDDL accepts it.
func TestAlterTableAddColumn(t *testing.T) {
	u8 := TypeRaw("UInt8")
	cases := []struct {
		name string
		stmt *AddColumnBuilder
		want string
	}{
		{
			"unqualified_table",
			AlterTableAddColumn("", "cerberus_router_corpus", "parallelism", u8),
			"ALTER TABLE cerberus_router_corpus ADD COLUMN IF NOT EXISTS `parallelism` UInt8",
		},
		{
			"qualified_table",
			AlterTableAddColumn("otel", "cerberus_router_corpus", "parallelism", u8),
			"ALTER TABLE otel.cerberus_router_corpus ADD COLUMN IF NOT EXISTS `parallelism` UInt8",
		},
		{
			"on_cluster",
			AlterTableAddColumn("otel", "cerberus_router_corpus", "parallelism", u8).OnCluster("prod"),
			"ALTER TABLE otel.cerberus_router_corpus ON CLUSTER `prod` " +
				"ADD COLUMN IF NOT EXISTS `parallelism` UInt8",
		},
		{
			"low_cardinality_type",
			AlterTableAddColumn("", "cerberus_router_corpus", "note", TypeLowCardinality(TypeRaw("String"))),
			"ALTER TABLE cerberus_router_corpus ADD COLUMN IF NOT EXISTS `note` LowCardinality(String)",
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
