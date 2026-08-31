package ddl

import (
	"strings"
	"testing"
)

// TestRenderAddTraceIDProjection_ExactSQL pins the ADD PROJECTION statement
// cerberus issue #2767 specifies verbatim: the idempotent IF NOT EXISTS
// guard, `SELECT TraceId, _part_offset ORDER BY TraceId` (no GROUP BY — this
// projection re-sorts rather than pre-aggregates, unlike every entry in
// metricCatalogProjections), and the optional ON CLUSTER clause — mirroring
// chsql's own TestAlterTableAddProjection.
func TestRenderAddTraceIDProjection_ExactSQL(t *testing.T) {
	cases := []struct {
		name  string
		table string
		cfg   Config
		want  string
	}{
		{
			"traces",
			"otel_traces",
			Config{}.withDefaults(),
			"ALTER TABLE default.otel_traces ADD PROJECTION IF NOT EXISTS proj_trace_id " +
				"(SELECT `TraceId`, `_part_offset` ORDER BY `TraceId`)",
		},
		{
			"logs",
			"otel_logs",
			Config{}.withDefaults(),
			"ALTER TABLE default.otel_logs ADD PROJECTION IF NOT EXISTS proj_trace_id " +
				"(SELECT `TraceId`, `_part_offset` ORDER BY `TraceId`)",
		},
		{
			"on_cluster",
			"otel_traces",
			Config{Cluster: "prod"}.withDefaults(),
			"ALTER TABLE default.otel_traces ON CLUSTER `prod` ADD PROJECTION IF NOT EXISTS proj_trace_id " +
				"(SELECT `TraceId`, `_part_offset` ORDER BY `TraceId`)",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := renderAddTraceIDProjection(tt.cfg, tt.table)
			if got != tt.want {
				t.Errorf("renderAddTraceIDProjection() =\n  %s\nwant:\n  %s", got, tt.want)
			}
		})
	}
}

// TestRenderSignal_TraceIDProjectionEnabled pins the version-gate contract at
// the render layer: TraceIDProjectionEnabled=false (a below-floor deployment,
// or the feature simply off) renders NO proj_trace_id statement at all for
// either Logs or Traces — not a statement that is rendered and then refused
// — and true renders exactly one on each. This is what makes "deployments
// below the version gate simply don't get the projection, no error, no
// half-applied state" true: there is nothing to half-apply, because nothing
// is ever sent.
func TestRenderSignal_TraceIDProjectionEnabled(t *testing.T) {
	for _, tt := range []struct {
		name    string
		enabled bool
		want    int
	}{
		{"disabled", false, 0},
		{"enabled", true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{TraceIDProjectionEnabled: tt.enabled}.withDefaults()
			for _, sig := range []Signal{Logs, Traces} {
				stmts, err := renderSignal(cfg, sig)
				if err != nil {
					t.Fatalf("renderSignal(%s): %v", sig, err)
				}
				got := 0
				for _, stmt := range stmts {
					if strings.Contains(stmt, "ADD PROJECTION IF NOT EXISTS "+traceIDProjectionName) {
						got++
					}
				}
				if got != tt.want {
					t.Errorf("%s: %d proj_trace_id statements, want %d, rendered:\n%v", sig, got, tt.want, stmts)
				}
			}
		})
	}
}

// TestRenderSignal_TraceIDProjectionEnabled_MetricsUntouched confirms the
// projection is scoped to Logs and Traces only — Metrics has no TraceId
// column at all, so TraceIDProjectionEnabled must be inert there regardless
// of the flag.
func TestRenderSignal_TraceIDProjectionEnabled_MetricsUntouched(t *testing.T) {
	cfg := Config{TraceIDProjectionEnabled: true}.withDefaults()
	stmts, err := renderSignal(cfg, Metrics)
	if err != nil {
		t.Fatalf("renderSignal(Metrics): %v", err)
	}
	for _, stmt := range stmts {
		if strings.Contains(stmt, traceIDProjectionName) {
			t.Errorf("Metrics: unexpected proj_trace_id statement:\n%s", stmt)
		}
	}
}
