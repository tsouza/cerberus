package ddl

import (
	"strings"
	"testing"
)

// TestRenderLogsTable_TextIndexEnabled pins the render-layer version-gate
// contract cerberus issue #2773 specifies: TextIndexEnabled=false (the zero
// value, a below-floor deployment or the feature simply off) renders the
// tokenbf_v1 branch (byte-identical to today); true renders the text-index
// branch (TYPE text(tokenizer = 'splitByNonAlpha')) instead — the SAME
// idx_lower_body name, mutually exclusive, matching the upstream template's
// own HasFullTextSearch branch.
func TestRenderLogsTable_TextIndexEnabled(t *testing.T) {
	for _, tt := range []struct {
		name    string
		enabled bool
		want    string
		notWant string
	}{
		{"disabled", false, "INDEX idx_lower_body lower(Body) TYPE tokenbf_v1(32768, 3, 0)", "TYPE text("},
		{"enabled", true, "INDEX idx_lower_body lower(Body) TYPE text(tokenizer = 'splitByNonAlpha')", "tokenbf_v1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{TextIndexEnabled: tt.enabled}.withDefaults()
			logs, err := renderLogsTable(cfg)
			if err != nil {
				t.Fatalf("renderLogsTable: %v", err)
			}
			if !strings.Contains(logs, tt.want) {
				t.Errorf("renderLogsTable(TextIndexEnabled=%v) missing %q:\n%s", tt.enabled, tt.want, logs)
			}
			if strings.Contains(logs, tt.notWant) {
				t.Errorf("renderLogsTable(TextIndexEnabled=%v) unexpectedly contains %q:\n%s", tt.enabled, tt.notWant, logs)
			}
		})
	}
}

// TestRenderAddBodyTextIndex_ExactSQL pins the additive ALTER TABLE ADD
// INDEX statement installed on an EXISTING logs table: idx_body_text (a
// SEPARATE name from idx_lower_body — see the function's own doc comment
// for why), lower(Body), TYPE text(tokenizer = 'splitByNonAlpha'), and the
// GRANULARITY 100000000 constant reproducing ClickHouse's own implicit
// default for a text index.
func TestRenderAddBodyTextIndex_ExactSQL(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			"default",
			Config{}.withDefaults(),
			"ALTER TABLE default.otel_logs ADD INDEX IF NOT EXISTS idx_body_text lower(`Body`) " +
				"TYPE text(tokenizer = 'splitByNonAlpha') GRANULARITY 100000000",
		},
		{
			"on_cluster",
			Config{Cluster: "prod"}.withDefaults(),
			"ALTER TABLE default.otel_logs ON CLUSTER `prod` ADD INDEX IF NOT EXISTS idx_body_text lower(`Body`) " +
				"TYPE text(tokenizer = 'splitByNonAlpha') GRANULARITY 100000000",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := renderAddBodyTextIndex(tt.cfg)
			if got != tt.want {
				t.Errorf("renderAddBodyTextIndex() =\n  %s\nwant:\n  %s", got, tt.want)
			}
		})
	}
}

// TestDropLegacyBodyTokenBFIndexSQL_ExactSQL pins the ALTER TABLE DROP INDEX
// statement cerberus issue #2839's `cerberus schema retire-idx-lower-body`
// verb renders/executes to retire the legacy idx_lower_body tokenbf_v1
// index on an upgraded existing table — see that function's doc comment for
// the live ClickHouse 26.6 measurement establishing the index provides zero
// query-time benefit for the exact predicate shape it was built to
// accelerate.
func TestDropLegacyBodyTokenBFIndexSQL_ExactSQL(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			"default",
			Config{}.withDefaults(),
			"ALTER TABLE default.otel_logs DROP INDEX IF EXISTS idx_lower_body",
		},
		{
			"on_cluster",
			Config{Cluster: "prod"}.withDefaults(),
			"ALTER TABLE default.otel_logs ON CLUSTER `prod` DROP INDEX IF EXISTS idx_lower_body",
		},
		{
			"custom_database_and_table",
			Config{Database: "otel", Tables: Tables{Logs: "logs_v2"}}.withDefaults(),
			"ALTER TABLE otel.logs_v2 DROP INDEX IF EXISTS idx_lower_body",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := DropLegacyBodyTokenBFIndexSQL(tt.cfg)
			if got != tt.want {
				t.Errorf("DropLegacyBodyTokenBFIndexSQL() =\n  %s\nwant:\n  %s", got, tt.want)
			}
		})
	}
}

// TestRenderSignal_TextIndexEnabled pins the version-gate contract at the
// render layer, mirroring TestRenderSignal_TraceIDProjectionEnabled:
// TextIndexEnabled=false renders NO idx_body_text statement at all — not a
// statement that is rendered and then refused — and true renders exactly
// one, trailing the CREATE + codec + proj_trace_id ALTERs.
func TestRenderSignal_TextIndexEnabled(t *testing.T) {
	for _, tt := range []struct {
		name    string
		enabled bool
		want    int
	}{
		{"disabled", false, 0},
		{"enabled", true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{TextIndexEnabled: tt.enabled}.withDefaults()
			stmts, err := renderSignal(cfg, Logs)
			if err != nil {
				t.Fatalf("renderSignal(Logs): %v", err)
			}
			got := 0
			for _, stmt := range stmts {
				if strings.Contains(stmt, "ADD INDEX IF NOT EXISTS "+bodyTextIndexName) {
					got++
				}
			}
			if got != tt.want {
				t.Errorf("%d idx_body_text statements, want %d, rendered:\n%v", got, tt.want, stmts)
			}
		})
	}
}

// TestRenderSignal_TextIndexEnabled_OtherSignalsUntouched confirms the
// additive index is scoped to Logs only — Metrics and Traces carry no Body
// column at all.
func TestRenderSignal_TextIndexEnabled_OtherSignalsUntouched(t *testing.T) {
	cfg := Config{TextIndexEnabled: true}.withDefaults()
	for _, sig := range []Signal{Metrics, Traces} {
		stmts, err := renderSignal(cfg, sig)
		if err != nil {
			t.Fatalf("renderSignal(%s): %v", sig, err)
		}
		for _, stmt := range stmts {
			if strings.Contains(stmt, bodyTextIndexName) {
				t.Errorf("%s: unexpected idx_body_text statement:\n%s", sig, stmt)
			}
		}
	}
}
