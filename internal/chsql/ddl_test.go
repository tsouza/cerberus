package chsql

import (
	"fmt"
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

// TestAlterTableModifyColumnCodec pins the codec-only MODIFY COLUMN statement
// (cerberus issue #2768): the optional <db>. qualifier, the idempotent IF
// EXISTS guard, the quoted column name, NO type (deliberately, unlike
// TestAlterTableModifyColumn's retyping shape), the CODEC(...) clause, and
// the optional ON CLUSTER clause. The statement carries no positional args,
// so RenderDDL accepts it.
func TestAlterTableModifyColumnCodec(t *testing.T) {
	doubleDeltaZSTD1 := Codec(BareIdent("DoubleDelta"), Call("ZSTD", InlineLit(1)))
	cases := []struct {
		name string
		stmt *ModifyColumnCodecBuilder
		want string
	}{
		{
			"unqualified_table",
			AlterTableModifyColumnCodec("", "otel_metrics_gauge", "TimeUnix", doubleDeltaZSTD1),
			"ALTER TABLE otel_metrics_gauge MODIFY COLUMN IF EXISTS `TimeUnix` CODEC(DoubleDelta, ZSTD(1))",
		},
		{
			"qualified_table",
			AlterTableModifyColumnCodec("otel", "otel_metrics_gauge", "TimeUnix", doubleDeltaZSTD1),
			"ALTER TABLE otel.otel_metrics_gauge MODIFY COLUMN IF EXISTS `TimeUnix` CODEC(DoubleDelta, ZSTD(1))",
		},
		{
			"on_cluster",
			AlterTableModifyColumnCodec("otel", "otel_metrics_gauge", "TimeUnix", doubleDeltaZSTD1).OnCluster("prod"),
			"ALTER TABLE otel.otel_metrics_gauge ON CLUSTER `prod` MODIFY COLUMN IF EXISTS `TimeUnix` CODEC(DoubleDelta, ZSTD(1))",
		},
		{
			"single_stage_no_args",
			AlterTableModifyColumnCodec("otel", "otel_logs", "Body", Codec(Call("ZSTD", InlineLit(3)))),
			"ALTER TABLE otel.otel_logs MODIFY COLUMN IF EXISTS `Body` CODEC(ZSTD(3))",
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

// TestAlterTableAddIndex pins the ADD INDEX statement: the fully-qualified
// (or bare) <db>.<table>, the idempotent IF NOT EXISTS guard, the indexed
// expression, the TYPE and GRANULARITY clauses, and the optional ON CLUSTER
// clause. The statement carries no positional args.
func TestAlterTableAddIndex(t *testing.T) {
	cases := []struct {
		name string
		stmt *AddIndexBuilder
		want string
	}{
		{
			"unqualified_table",
			AlterTableAddIndex("", "otel_metrics_sum", "idx_agg_temporality", Col("AggregationTemporality"), "minmax", 1),
			"ALTER TABLE otel_metrics_sum ADD INDEX IF NOT EXISTS idx_agg_temporality `AggregationTemporality` TYPE minmax GRANULARITY 1",
		},
		{
			"qualified_table",
			AlterTableAddIndex("otel", "otel_metrics_histogram", "idx_agg_temporality", Col("AggregationTemporality"), "minmax", 1),
			"ALTER TABLE otel.otel_metrics_histogram ADD INDEX IF NOT EXISTS idx_agg_temporality `AggregationTemporality` TYPE minmax GRANULARITY 1",
		},
		{
			"on_cluster",
			AlterTableAddIndex("otel", "otel_metrics_sum", "idx_agg_temporality", Col("AggregationTemporality"), "minmax", 1).OnCluster("prod"),
			"ALTER TABLE otel.otel_metrics_sum ON CLUSTER `prod` " +
				"ADD INDEX IF NOT EXISTS idx_agg_temporality `AggregationTemporality` TYPE minmax GRANULARITY 1",
		},
		{
			"different_granularity",
			AlterTableAddIndex("", "otel_metrics_sum", "idx_agg_temporality", Col("AggregationTemporality"), "minmax", 4),
			"ALTER TABLE otel_metrics_sum ADD INDEX IF NOT EXISTS idx_agg_temporality `AggregationTemporality` TYPE minmax GRANULARITY 4",
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

// TestAlterTableAddIndex_RejectsNonPositiveGranularity pins the
// construction-time guard on GRANULARITY: 0 or negative is not a valid
// ClickHouse skip-index clause, so AlterTableAddIndex must panic rather than
// let a caller render DDL ClickHouse would reject at apply time.
func TestAlterTableAddIndex_RejectsNonPositiveGranularity(t *testing.T) {
	for _, g := range []int{0, -1} {
		t.Run(fmt.Sprintf("granularity_%d", g), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("AlterTableAddIndex(granularity=%d) did not panic", g)
				}
			}()
			AlterTableAddIndex("", "otel_metrics_sum", "idx_agg_temporality", Col("AggregationTemporality"), "minmax", g)
		})
	}
}

// TestAlterTableAddStatistics pins the ADD STATISTICS statement: the
// fully-qualified (or bare) <db>.<table>, the idempotent IF NOT EXISTS
// guard, the bare (no-parens) comma-separated column and TYPE lists, and the
// optional ON CLUSTER clause. The statement carries no positional args.
func TestAlterTableAddStatistics(t *testing.T) {
	cases := []struct {
		name string
		stmt *AddStatisticsBuilder
		want string
	}{
		{
			"unqualified_table_single_column_single_type",
			AlterTableAddStatistics("", "otel_traces", []string{"Duration"}, []string{"minmax"}),
			"ALTER TABLE otel_traces ADD STATISTICS IF NOT EXISTS `Duration` TYPE minmax",
		},
		{
			"qualified_table_multi_column_multi_type",
			AlterTableAddStatistics("otel", "otel_metrics_sum", []string{"ServiceName", "MetricName"}, []string{"minmax", "uniq"}),
			"ALTER TABLE otel.otel_metrics_sum ADD STATISTICS IF NOT EXISTS `ServiceName`, `MetricName` TYPE minmax, uniq",
		},
		{
			"on_cluster",
			AlterTableAddStatistics("otel", "otel_traces", []string{"Duration"}, []string{"minmax", "uniq", "tdigest"}).OnCluster("prod"),
			"ALTER TABLE otel.otel_traces ON CLUSTER `prod` " +
				"ADD STATISTICS IF NOT EXISTS `Duration` TYPE minmax, uniq, tdigest",
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

// TestAlterTableAddStatistics_RejectsEmptyLists pins the construction-time
// guard: ClickHouse's grammar requires both the column list and the TYPE
// list non-empty, so AlterTableAddStatistics must panic rather than let a
// caller render DDL ClickHouse would reject at apply time.
func TestAlterTableAddStatistics_RejectsEmptyLists(t *testing.T) {
	cases := []struct {
		name    string
		columns []string
		types   []string
	}{
		{"empty_columns", nil, []string{"minmax"}},
		{"empty_types", []string{"Duration"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("AlterTableAddStatistics(columns=%v, types=%v) did not panic", tc.columns, tc.types)
				}
			}()
			AlterTableAddStatistics("", "otel_traces", tc.columns, tc.types)
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

// TestCreateTableDatabase pins CreateTableBuilder's Database() qualifier:
// unset renders the bare table name (the pre-existing optcorpus shape),
// set renders `<db>.<table>`.
func TestCreateTableDatabase(t *testing.T) {
	cols := []ColumnDef{{Name: "a", Type: TypeRaw("String")}}
	unqualified := CreateTable("t").IfNotExists().Columns(cols...).Engine(EngineMergeTree()).SQL()
	wantUnqualified := "CREATE TABLE IF NOT EXISTS t (`a` String) ENGINE = MergeTree"
	if unqualified != wantUnqualified {
		t.Errorf("unqualified SQL() = %q; want %q", unqualified, wantUnqualified)
	}

	qualified := CreateTable("t").Database("otel").IfNotExists().Columns(cols...).Engine(EngineMergeTree()).SQL()
	wantQualified := "CREATE TABLE IF NOT EXISTS otel.t (`a` String) ENGINE = MergeTree"
	if qualified != wantQualified {
		t.Errorf("qualified SQL() = %q; want %q", qualified, wantQualified)
	}
}

// TestEngineAggregatingMergeTree pins the bare AggregatingMergeTree /
// ReplicatedAggregatingMergeTree engine clauses — no positional arguments,
// mirroring EngineMergeTree / EngineReplicatedMergeTree.
func TestEngineAggregatingMergeTree(t *testing.T) {
	if got := renderFrag(EngineAggregatingMergeTree()); got != "AggregatingMergeTree" {
		t.Errorf("EngineAggregatingMergeTree = %q; want AggregatingMergeTree", got)
	}
	if got := renderFrag(EngineReplicatedAggregatingMergeTree()); got != "ReplicatedAggregatingMergeTree" {
		t.Errorf("EngineReplicatedAggregatingMergeTree = %q; want ReplicatedAggregatingMergeTree", got)
	}
}

// deltaPrefixMVBody builds a small representative aggregate SELECT — the
// same shape internal/deltaprefix and internal/schema/ddl compose for the
// real DELTA-prefix table — reused by TestCreateMaterializedView and
// TestInsertSelect below.
func deltaPrefixMVBody() *QueryBuilder {
	return NewQuery().
		Select(
			Col("MetricName"),
			Col("Attributes"),
			As(Call("toStartOfDay", Col("TimeUnix")), "BucketStart"),
			As(Call("sum", Col("Value")), "PartialSum"),
		).
		From(Qual("otel", "otel_metrics_sum")).
		Where(Eq(Col("AggregationTemporality"), InlineLit(int64(1)))).
		GroupBy(Col("MetricName"), Col("Attributes"), Call("toStartOfDay", Col("TimeUnix")))
}

// TestCreateMaterializedView pins the CREATE MATERIALIZED VIEW ... TO ...
// AS ... shape: the optional <db>. qualifier on the view's own name, the
// idempotent IF NOT EXISTS guard, the optional ON CLUSTER clause, the
// <db>.<table> TO target, and the bare (unparenthesized) SELECT body. The
// statement carries no positional args (the body uses only InlineLit), so
// RenderDDL accepts it.
func TestCreateMaterializedView(t *testing.T) {
	wantBody := "SELECT `MetricName`, `Attributes`, toStartOfDay(`TimeUnix`) AS `BucketStart`, " +
		"sum(`Value`) AS `PartialSum` FROM `otel`.`otel_metrics_sum` WHERE `AggregationTemporality` = 1 " +
		"GROUP BY `MetricName`, `Attributes`, toStartOfDay(`TimeUnix`)"

	got := CreateMaterializedView("otel_metrics_sum_delta_prefix_mv").
		Database("otel").
		IfNotExists().
		To("otel", "otel_metrics_sum_delta_prefix").
		As(deltaPrefixMVBody()).
		SQL()
	want := "CREATE MATERIALIZED VIEW IF NOT EXISTS otel.otel_metrics_sum_delta_prefix_mv " +
		"TO otel.otel_metrics_sum_delta_prefix AS " + wantBody
	if got != want {
		t.Errorf("SQL() = %q; want %q", got, want)
	}

	gotCluster := CreateMaterializedView("v").
		IfNotExists().
		OnCluster("prod").
		To("", "t").
		As(NewQuery().Select(Col("a")).From(Col("t"))).
		SQL()
	wantCluster := "CREATE MATERIALIZED VIEW IF NOT EXISTS v ON CLUSTER `prod` TO t AS SELECT `a` FROM `t`"
	if gotCluster != wantCluster {
		t.Errorf("clustered SQL() = %q; want %q", gotCluster, wantCluster)
	}
}

// TestCreateMaterializedView_RefreshEveryMinutes pins the `REFRESH EVERY
// <n> MINUTE` clause's exact position in the statement — between the
// optional ON CLUSTER clause and the TO target, matching ClickHouse's own
// grammar (cerberus issue #2770) — and that a non-positive n panics rather
// than silently omitting the clause.
func TestCreateMaterializedView_RefreshEveryMinutes(t *testing.T) {
	got := CreateMaterializedView("loki_label_catalog_mv").
		Database("otel").
		IfNotExists().
		RefreshEveryMinutes(5).
		To("otel", "loki_label_catalog").
		As(NewQuery().Select(Col("LabelKey")).From(Col("otel_logs"))).
		SQL()
	want := "CREATE MATERIALIZED VIEW IF NOT EXISTS otel.loki_label_catalog_mv REFRESH EVERY 5 MINUTE " +
		"TO otel.loki_label_catalog AS SELECT `LabelKey` FROM `otel_logs`"
	if got != want {
		t.Errorf("SQL() = %q; want %q", got, want)
	}

	gotCluster := CreateMaterializedView("v").
		OnCluster("prod").
		RefreshEveryMinutes(10).
		To("", "t").
		As(NewQuery().Select(Col("a")).From(Col("t"))).
		SQL()
	wantCluster := "CREATE MATERIALIZED VIEW v ON CLUSTER `prod` REFRESH EVERY 10 MINUTE TO t AS SELECT `a` FROM `t`"
	if gotCluster != wantCluster {
		t.Errorf("clustered SQL() = %q; want %q", gotCluster, wantCluster)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("RefreshEveryMinutes(0) did not panic")
		}
	}()
	CreateMaterializedView("v").RefreshEveryMinutes(0)
}

// TestCreateMaterializedView_PanicsWithoutToOrAs pins the fail-fast guard:
// calling SQL() without To(...) or without As(...) must panic naming the
// missing call, rather than silently rendering a `TO  AS ...` statement
// with an empty target identifier (invalid SQL ClickHouse would only
// reject at apply time) or nil-dereferencing inside frag's writeInto call.
func TestCreateMaterializedView_PanicsWithoutToOrAs(t *testing.T) {
	t.Run("missing_To", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("SQL() without To(...) did not panic")
			}
		}()
		CreateMaterializedView("v").As(NewQuery().Select(Col("a")).From(Col("t"))).SQL()
	})
	t.Run("missing_As", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("SQL() without As(...) did not panic")
			}
		}()
		CreateMaterializedView("v").To("", "t").SQL()
	})
}

// TestInsertSelect pins the INSERT INTO ... (<cols>) <SELECT ...> shape,
// including the case that actually motivates the builder: a body carrying a
// real positional `?` binding (Lit), which Build() must surface in its args
// return rather than dropping — the DML counterpart of RenderDDL's
// no-bindings assertion for the CREATE/ALTER builders.
func TestInsertSelect(t *testing.T) {
	sql, args := InsertSelect(
		"otel", "otel_metrics_sum_delta_prefix",
		[]string{"MetricName", "Attributes", "BucketStart", "PartialSum"},
		deltaPrefixMVBody(),
	).Build()
	want := "INSERT INTO otel.otel_metrics_sum_delta_prefix (`MetricName`, `Attributes`, `BucketStart`, `PartialSum`) " +
		"SELECT `MetricName`, `Attributes`, toStartOfDay(`TimeUnix`) AS `BucketStart`, sum(`Value`) AS `PartialSum` " +
		"FROM `otel`.`otel_metrics_sum` WHERE `AggregationTemporality` = 1 " +
		"GROUP BY `MetricName`, `Attributes`, toStartOfDay(`TimeUnix`)"
	if sql != want {
		t.Errorf("Build() sql = %q; want %q", sql, want)
	}
	if len(args) != 0 {
		t.Errorf("inline-only body must bind no args, got %v", args)
	}

	body := NewQuery().
		Select(Col("MetricName")).
		From(Qual("otel", "otel_metrics_sum")).
		Where(Lt(Col("TimeUnix"), Lit(int64(1700000000))))
	sqlBound, argsBound := InsertSelect("otel", "t", []string{"MetricName"}, body).Build()
	wantBound := "INSERT INTO otel.t (`MetricName`) SELECT `MetricName` FROM `otel`.`otel_metrics_sum` WHERE `TimeUnix` < ?"
	if sqlBound != wantBound {
		t.Errorf("bound Build() sql = %q; want %q", sqlBound, wantBound)
	}
	if len(argsBound) != 1 || argsBound[0] != int64(1700000000) {
		t.Errorf("bound Build() args = %v; want [1700000000]", argsBound)
	}

	unqualified, _ := InsertSelect("", "t", []string{"a"}, NewQuery().Select(Col("a")).From(Col("t"))).Build()
	wantUnqualified := "INSERT INTO t (`a`) SELECT `a` FROM `t`"
	if unqualified != wantUnqualified {
		t.Errorf("unqualified Build() sql = %q; want %q", unqualified, wantUnqualified)
	}
}
