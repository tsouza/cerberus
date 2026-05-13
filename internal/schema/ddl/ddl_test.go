package ddl

import (
	"strings"
	"testing"
	"time"
)

// TestRenderSignal_Metrics checks all five metrics templates render with
// the right table names + engine + database substituted in.
func TestRenderSignal_Metrics(t *testing.T) {
	cfg := Config{}.withDefaults()

	stmts, err := renderSignal(cfg, Metrics)
	if err != nil {
		t.Fatalf("renderSignal(Metrics): %v", err)
	}
	if got, want := len(stmts), 5; got != want {
		t.Fatalf("metrics: got %d statements, want %d", got, want)
	}
	wantTables := []string{
		"otel_metrics_gauge",
		"otel_metrics_sum",
		"otel_metrics_histogram",
		"otel_metrics_exp_histogram",
		"otel_metrics_summary",
	}
	for i, stmt := range stmts {
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
}

// TestRenderSignal_Logs checks the single logs template renders.
func TestRenderSignal_Logs(t *testing.T) {
	cfg := Config{}.withDefaults()
	stmts, err := renderSignal(cfg, Logs)
	if err != nil {
		t.Fatalf("renderSignal(Logs): %v", err)
	}
	if got, want := len(stmts), 1; got != want {
		t.Fatalf("logs: got %d statements, want %d", got, want)
	}
	if !strings.Contains(stmts[0], "otel_logs") {
		t.Errorf("logs: missing table name in:\n%s", stmts[0])
	}
	if !strings.Contains(stmts[0], "CREATE TABLE IF NOT EXISTS") {
		t.Errorf("logs: missing IF NOT EXISTS")
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
		TTL:      48 * time.Hour,
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
			if !strings.Contains(stmt, `ON CLUSTER "my_cluster"`) {
				t.Errorf("%s[%d]: cluster clause not rendered:\n%s", sig, i, stmt)
			}
		}
	}

	// Metrics + Logs + Traces table renders should include the custom
	// engine + a 48-hour TTL expression.
	metrics, _ := renderSignal(cfg, Metrics)
	for i, stmt := range metrics {
		if !strings.Contains(stmt, "ReplicatedMergeTree") {
			t.Errorf("metrics[%d]: custom engine not rendered", i)
		}
		if !strings.Contains(stmt, "TTL toDateTime(TimeUnix) + toIntervalDay(2)") {
			t.Errorf("metrics[%d]: TTL not rendered:\n%s", i, stmt)
		}
	}
	logs, _ := renderSignal(cfg, Logs)
	if !strings.Contains(logs[0], "TTL TimestampTime + toIntervalDay(2)") {
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

// TestRenderSignal_UnknownSignal returns an error rather than panicking.
func TestRenderSignal_UnknownSignal(t *testing.T) {
	_, err := renderSignal(Config{}.withDefaults(), Signal(99))
	if err == nil {
		t.Fatalf("expected error for unknown signal, got nil")
	}
}

// TestTTLExpr_RoundingBuckets checks the TTL rounding logic that mirrors
// the upstream GenerateTTLExpr — round-up to day/hour/minute when the
// duration falls on a clean boundary.
func TestTTLExpr_RoundingBuckets(t *testing.T) {
	cases := []struct {
		name string
		ttl  time.Duration
		want string
	}{
		{"zero", 0, ""},
		{"1d", 24 * time.Hour, "TTL t + toIntervalDay(1)"},
		{"2h", 2 * time.Hour, "TTL t + toIntervalHour(2)"},
		{"30m", 30 * time.Minute, "TTL t + toIntervalMinute(30)"},
		{"45s", 45 * time.Second, "TTL t + toIntervalSecond(45)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (Config{TTL: tc.ttl}).ttlExpr("t")
			if got != tc.want {
				t.Errorf("ttlExpr: got %q, want %q", got, tc.want)
			}
		})
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
