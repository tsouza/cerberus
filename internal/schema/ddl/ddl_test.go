package ddl

import (
	"strings"
	"testing"
	"time"
)

// TestRenderSignal_Metrics checks all five metrics templates render with
// the right table names + engine + database substituted in, followed by the
// three idempotent metric-name ADD PROJECTION ALTERs.
func TestRenderSignal_Metrics(t *testing.T) {
	cfg := Config{}.withDefaults()

	stmts, err := renderSignal(cfg, Metrics)
	if err != nil {
		t.Fatalf("renderSignal(Metrics): %v", err)
	}
	// 5 CREATE TABLE + 3 ADD PROJECTION (gauge/sum/histogram catalog tables).
	const wantCreates = 5
	if got, want := len(stmts), wantCreates+3; got != want {
		t.Fatalf("metrics: got %d statements, want %d", got, want)
	}
	wantTables := []string{
		"otel_metrics_gauge",
		"otel_metrics_sum",
		"otel_metrics_histogram",
		"otel_metrics_exp_histogram",
		"otel_metrics_summary",
	}
	for i, stmt := range stmts[:wantCreates] {
		if !strings.Contains(stmt, "CREATE TABLE IF NOT EXISTS") {
			t.Errorf("metrics[%d]: missing IF NOT EXISTS:\n%s", i, stmt)
		}
		if !strings.Contains(stmt, wantTables[i]) {
			t.Errorf("metrics[%d]: missing table %q in:\n%s", i, wantTables[i], stmt)
		}
		if !strings.Contains(stmt, "MergeTree()") {
			t.Errorf("metrics[%d]: missing default MergeTree() engine", i)
		}
		if strings.Contains(stmt, "ON CLUSTER") {
			t.Errorf("metrics[%d]: empty cluster should not render ON CLUSTER", i)
		}
		if strings.Contains(stmt, "TTL toDateTime") {
			t.Errorf("metrics[%d]: zero TTL should not render TTL clause", i)
		}
	}
	// The projection ALTERs follow the CREATEs, one per catalog table, in
	// gauge/sum/histogram order. CREATE precedes ALTER so the ALTER never
	// races a missing table.
	wantProjTables := []string{"otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_histogram"}
	for i, stmt := range stmts[wantCreates:] {
		wantPrefix := "ALTER TABLE default." + wantProjTables[i] +
			" ADD PROJECTION IF NOT EXISTS proj_metric_name "
		if !strings.HasPrefix(stmt, wantPrefix) {
			t.Errorf("metrics projection[%d]: got %q, want prefix %q", i, stmt, wantPrefix)
		}
		if !strings.Contains(stmt, "max(`TimeUnix`) GROUP BY `MetricName`") {
			t.Errorf("metrics projection[%d]: missing aggregating body in:\n%s", i, stmt)
		}
		if strings.Contains(stmt, "ON CLUSTER") {
			t.Errorf("metrics projection[%d]: empty cluster should not render ON CLUSTER", i)
		}
	}
}

// TestRenderSignal_Logs checks the single logs template renders with the
// v0.152.0 schema shape: no TimestampTime column, the new partition/order
// keys, the materialized resource-attribute columns, and the bloom-filter
// index branch (HasFullTextSearch=false — the text-index branch needs
// ClickHouse >= 26.2).
func TestRenderSignal_Logs(t *testing.T) {
	cfg := Config{}.withDefaults()
	stmts, err := renderSignal(cfg, Logs)
	if err != nil {
		t.Fatalf("renderSignal(Logs): %v", err)
	}
	if got, want := len(stmts), 1; got != want {
		t.Fatalf("logs: got %d statements, want %d", got, want)
	}
	logs := stmts[0]
	if !strings.Contains(logs, "otel_logs") {
		t.Errorf("logs: missing table name in:\n%s", logs)
	}
	if !strings.Contains(logs, "CREATE TABLE IF NOT EXISTS") {
		t.Errorf("logs: missing IF NOT EXISTS")
	}
	if strings.Contains(logs, "TimestampTime") {
		t.Errorf("logs: TimestampTime column was removed upstream in v0.150.0 and must not render:\n%s", logs)
	}
	if !strings.Contains(logs, "PARTITION BY toDate(Timestamp)") {
		t.Errorf("logs: missing PARTITION BY toDate(Timestamp):\n%s", logs)
	}
	if !strings.Contains(logs, "ORDER BY (toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp)") {
		t.Errorf("logs: missing v0.152.0 ORDER BY key:\n%s", logs)
	}
	if strings.Contains(logs, "PRIMARY KEY") {
		t.Errorf("logs: v0.152.0 schema carries no explicit PRIMARY KEY:\n%s", logs)
	}
	// The 8 materialized resource-attribute columns introduced upstream.
	for _, col := range []string{
		"`__otel_materialized_k8s.cluster.name`",
		"`__otel_materialized_k8s.container.name`",
		"`__otel_materialized_k8s.deployment.name`",
		"`__otel_materialized_k8s.namespace.name`",
		"`__otel_materialized_k8s.node.name`",
		"`__otel_materialized_k8s.pod.name`",
		"`__otel_materialized_k8s.pod.uid`",
		"`__otel_materialized_deployment.environment.name`",
	} {
		if !strings.Contains(logs, col) {
			t.Errorf("logs: missing materialized column %s:\n%s", col, logs)
		}
	}
	// HasFullTextSearch=false renders the bloom-filter index branch, not
	// the TYPE text() branch (which needs ClickHouse >= 26.2).
	if !strings.Contains(logs, "INDEX idx_trace_id TraceId TYPE bloom_filter(0.001) GRANULARITY 1") {
		t.Errorf("logs: missing bloom_filter trace-id index:\n%s", logs)
	}
	if !strings.Contains(logs, "INDEX idx_lower_body lower(Body) TYPE tokenbf_v1(32768, 3, 0) GRANULARITY 8") {
		t.Errorf("logs: missing tokenbf_v1 body index:\n%s", logs)
	}
	if strings.Contains(logs, "TYPE text(") {
		t.Errorf("logs: full-text-search index branch must not render with HasFullTextSearch=false:\n%s", logs)
	}
}

// TestRenderSignal_Traces checks the three traces statements render — the
// spans table, the trace_id_ts lookup table, and its materialized view.
func TestRenderSignal_Traces(t *testing.T) {
	cfg := Config{}.withDefaults()
	stmts, err := renderSignal(cfg, Traces)
	if err != nil {
		t.Fatalf("renderSignal(Traces): %v", err)
	}
	if got, want := len(stmts), 3; got != want {
		t.Fatalf("traces: got %d statements, want %d", got, want)
	}
	if !strings.Contains(stmts[0], "CREATE TABLE IF NOT EXISTS") || !strings.Contains(stmts[0], "otel_traces") {
		t.Errorf("traces[0]: missing spans-table CREATE:\n%s", stmts[0])
	}
	if !strings.Contains(stmts[1], "otel_traces_trace_id_ts") {
		t.Errorf("traces[1]: missing trace_id_ts lookup table:\n%s", stmts[1])
	}
	if !strings.Contains(stmts[2], "CREATE MATERIALIZED VIEW IF NOT EXISTS") {
		t.Errorf("traces[2]: missing MV CREATE:\n%s", stmts[2])
	}
	if !strings.Contains(stmts[2], "otel_traces_trace_id_ts_mv") {
		t.Errorf("traces[2]: missing MV name:\n%s", stmts[2])
	}
	// The MV body should reference the spans table in its FROM clause.
	if !strings.Contains(stmts[2], `FROM "default"."otel_traces"`) {
		t.Errorf("traces[2]: MV should select FROM the spans table:\n%s", stmts[2])
	}
}

// TestRenderSignal_CustomConfig exercises every Config override.
func TestRenderSignal_CustomConfig(t *testing.T) {
	cfg := Config{
		Database: "cerberus_test",
		Cluster:  "my_cluster",
		Engine:   "ReplicatedMergeTree('/clickhouse/{shard}/tables/{table}', '{replica}')",
		TTL:      TTL{Metrics: 48 * time.Hour, Logs: 48 * time.Hour, Traces: 48 * time.Hour},
		Tables: Tables{
			MetricsGauge:        "custom_gauge",
			MetricsSum:          "custom_sum",
			MetricsHistogram:    "custom_histogram",
			MetricsExpHistogram: "custom_exp_histogram",
			MetricsSummary:      "custom_summary",
			Logs:                "custom_logs",
			Traces:              "custom_traces",
		},
	}.withDefaults()

	for _, sig := range All {
		stmts, err := renderSignal(cfg, sig)
		if err != nil {
			t.Fatalf("renderSignal(%s): %v", sig, err)
		}
		for i, stmt := range stmts {
			if !strings.Contains(stmt, "cerberus_test") {
				t.Errorf("%s[%d]: custom database not rendered:\n%s", sig, i, stmt)
			}
			if !strings.Contains(stmt, "ON CLUSTER `my_cluster`") {
				t.Errorf("%s[%d]: cluster clause not rendered:\n%s", sig, i, stmt)
			}
		}
	}

	// Metrics + Logs + Traces table renders should include the custom
	// engine + a 48-hour TTL expression.
	metrics, _ := renderSignal(cfg, Metrics)
	for i, stmt := range metrics {
		// ADD PROJECTION ALTERs carry neither engine nor TTL — those live on
		// the CREATE TABLE statements only.
		if strings.HasPrefix(stmt, "ALTER TABLE") {
			continue
		}
		if !strings.Contains(stmt, "ReplicatedMergeTree") {
			t.Errorf("metrics[%d]: custom engine not rendered", i)
		}
		if !strings.Contains(stmt, "TTL toDateTime(TimeUnix) + toIntervalDay(2)") {
			t.Errorf("metrics[%d]: TTL not rendered:\n%s", i, stmt)
		}
	}
	logs, _ := renderSignal(cfg, Logs)
	if !strings.Contains(logs[0], "TTL toDateTime(Timestamp) + toIntervalDay(2)") {
		t.Errorf("logs: TTL not rendered:\n%s", logs[0])
	}
	traces, _ := renderSignal(cfg, Traces)
	if !strings.Contains(traces[0], "TTL toDateTime(Timestamp) + toIntervalDay(2)") {
		t.Errorf("traces[0]: TTL not rendered:\n%s", traces[0])
	}
	if !strings.Contains(traces[1], "TTL toDateTime(Start) + toIntervalDay(2)") {
		t.Errorf("traces[1]: TTL not rendered:\n%s", traces[1])
	}
}

// TestRenderSignal_ReplicatedDatabaseDefaultsToReplicatedMergeTree pins the
// prod-bug fix: a Replicated database does NOT auto-convert MergeTree, so with
// DatabaseEngine.Replicated set and no explicit Engine, the tables must render
// with the BARE ReplicatedMergeTree engine — no (path, replica) args, which a
// Replicated database rejects with code 36, and never plain MergeTree() — so
// the DATA actually replicates across replicas.
func TestRenderSignal_ReplicatedDatabaseDefaultsToReplicatedMergeTree(t *testing.T) {
	cfg := Config{
		DatabaseEngine: DatabaseEngine{
			Replicated:        true,
			ReplicatedZooPath: "/clickhouse/databases/otel",
		},
	}.withDefaults()

	for _, sig := range All {
		stmts, err := renderSignal(cfg, sig)
		if err != nil {
			t.Fatalf("renderSignal(%s): %v", sig, err)
		}
		for i, stmt := range stmts {
			// The trace_id_ts MV (traces[2]) has no engine of its own.
			if sig == Traces && i == 2 {
				continue
			}
			// ADD PROJECTION ALTERs inherit the table engine; they name none.
			if strings.HasPrefix(stmt, "ALTER TABLE") {
				continue
			}
			if !strings.Contains(stmt, "ReplicatedMergeTree") {
				t.Errorf("%s[%d]: want bare ReplicatedMergeTree engine in:\n%s", sig, i, stmt)
			}
			// No explicit args: a Replicated database rejects
			// ReplicatedMergeTree('...', '...') with code 36.
			if strings.Contains(stmt, "ReplicatedMergeTree(") {
				t.Errorf("%s[%d]: ReplicatedMergeTree must take NO args under a Replicated database:\n%s", sig, i, stmt)
			}
			if strings.Contains(stmt, "MergeTree()") {
				t.Errorf("%s[%d]: plain MergeTree() must not render under a Replicated database:\n%s", sig, i, stmt)
			}
		}
	}
}

// TestRenderSignal_ExplicitEngineWinsOverReplicated pins that an explicit
// Engine override beats the Replicated-database default — the operator's
// chosen engine string is used verbatim, not the derived ReplicatedMergeTree.
func TestRenderSignal_ExplicitEngineWinsOverReplicated(t *testing.T) {
	cfg := Config{
		Engine:         "ReplicatedReplacingMergeTree('/x/{shard}', '{replica}')",
		DatabaseEngine: DatabaseEngine{Replicated: true, ReplicatedZooPath: "/clickhouse/databases/otel"},
	}.withDefaults()

	metrics, _ := renderSignal(cfg, Metrics)
	for i, stmt := range metrics {
		// ADD PROJECTION ALTERs inherit the table engine; they name none.
		if strings.HasPrefix(stmt, "ALTER TABLE") {
			continue
		}
		if !strings.Contains(stmt, "ReplicatedReplacingMergeTree('/x/{shard}', '{replica}')") {
			t.Errorf("metrics[%d]: explicit engine override not honoured:\n%s", i, stmt)
		}
	}
}

// TestRenderSignal_UnknownSignal returns an error rather than panicking.
func TestRenderSignal_UnknownSignal(t *testing.T) {
	_, err := renderSignal(Config{}.withDefaults(), Signal(99))
	if err == nil {
		t.Fatalf("expected error for unknown signal, got nil")
	}
}

// TestTTLExpr_RoundingBuckets checks the TTL rounding logic that mirrors
// the upstream GenerateTTLExpr — round-up to day/hour/minute when the
// duration falls on a clean boundary. ttlExpr takes the bare retention
// column and wraps it in toDateTime(...) itself (via chsql.TableTTL), so
// the rendered clause is `TTL toDateTime(<col>) + toIntervalXxx(N)`.
func TestTTLExpr_RoundingBuckets(t *testing.T) {
	cases := []struct {
		name string
		ttl  time.Duration
		want string
	}{
		{"zero", 0, ""},
		{"1d", 24 * time.Hour, "TTL toDateTime(t) + toIntervalDay(1)"},
		{"2h", 2 * time.Hour, "TTL toDateTime(t) + toIntervalHour(2)"},
		{"30m", 30 * time.Minute, "TTL toDateTime(t) + toIntervalMinute(30)"},
		{"45s", 45 * time.Second, "TTL toDateTime(t) + toIntervalSecond(45)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ttlExpr("t", tc.ttl)
			if got != tc.want {
				t.Errorf("ttlExpr: got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRenderSignal_PerSignalTTL pins independent per-signal retention: a
// different TTL for metrics, logs, and traces lands on the right tables
// (and a zero for a signal emits no TTL), proving the signals don't share
// one global duration.
func TestRenderSignal_PerSignalTTL(t *testing.T) {
	cfg := Config{
		TTL: TTL{
			Metrics: 90 * 24 * time.Hour, // 90 days (not a whole week → stays days)
			Logs:    14 * 24 * time.Hour, // 14 days = 2 weeks → coarsest bucket is weeks
			Traces:  0,                   // no TTL on traces
		},
	}.withDefaults()

	metrics, _ := renderSignal(cfg, Metrics)
	for i, stmt := range metrics {
		// ADD PROJECTION ALTERs inherit retention from the table; no TTL clause.
		if strings.HasPrefix(stmt, "ALTER TABLE") {
			continue
		}
		if !strings.Contains(stmt, "TTL toDateTime(TimeUnix) + toIntervalDay(90)") {
			t.Errorf("metrics[%d]: want 90d TTL:\n%s", i, stmt)
		}
	}
	logs, _ := renderSignal(cfg, Logs)
	if !strings.Contains(logs[0], "TTL toDateTime(Timestamp) + toIntervalWeek(2)") {
		t.Errorf("logs: want 2-week TTL:\n%s", logs[0])
	}
	traces, _ := renderSignal(cfg, Traces)
	for i, stmt := range traces {
		if strings.Contains(stmt, "TTL toDateTime") {
			t.Errorf("traces[%d]: TTL=0 must emit no TTL clause:\n%s", i, stmt)
		}
	}
}

// TestSignalString sanity-checks the String() method since it's surfaced
// in error messages.
func TestSignalString(t *testing.T) {
	cases := map[Signal]string{
		Metrics:    "metrics",
		Logs:       "logs",
		Traces:     "traces",
		Signal(99): "unknown",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("Signal(%d).String() = %q, want %q", int(s), got, want)
		}
	}
}
