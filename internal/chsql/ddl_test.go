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
		{"day", "TimeUnix", 48 * time.Hour, "TTL toDateTime(TimeUnix) + toIntervalDay(2)"},
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
			sql, args := tc.stmt.Build()
			if sql != tc.want {
				t.Errorf("Build() sql = %q; want %q", sql, tc.want)
			}
			if len(args) != 0 {
				t.Errorf("Build() args = %v; want none (DDL has no bindings)", args)
			}
		})
	}
}

// TestCreateDatabase_Subqueryable confirms the builder satisfies the
// Subqueryable contract (Build) so it composes uniformly with the rest of
// the chsql surface, even though a DDL statement is never used as a
// subquery in practice.
func TestCreateDatabase_Subqueryable(t *testing.T) {
	var _ Subqueryable = CreateDatabase("otel")
}
