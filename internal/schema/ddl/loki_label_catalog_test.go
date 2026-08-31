package ddl

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestRenderLokiLabelCatalogTable_ExactSQL pins the catalog table's shape
// (cerberus issue #2770): a String LabelKey ORDER BY key and an
// AggregateFunction(uniq, String) CardinalityState column on
// AggregatingMergeTree (or ReplicatedAggregatingMergeTree under a
// Replicated database engine).
func TestRenderLokiLabelCatalogTable_ExactSQL(t *testing.T) {
	cfg := Config{}.withDefaults()
	got := renderLokiLabelCatalogTable(cfg)
	want := "CREATE TABLE IF NOT EXISTS default.loki_label_catalog " +
		"(`LabelKey` String, `CardinalityState` AggregateFunction(uniq, String)) " +
		"ENGINE = AggregatingMergeTree ORDER BY (`LabelKey`)"
	if got != want {
		t.Errorf("renderLokiLabelCatalogTable() =\n  %s\nwant:\n  %s", got, want)
	}

	replCfg := Config{DatabaseEngine: DatabaseEngine{Replicated: true, ReplicatedZooPath: "/x"}}.withDefaults()
	gotRepl := renderLokiLabelCatalogTable(replCfg)
	if !strings.Contains(gotRepl, "ENGINE = ReplicatedAggregatingMergeTree") {
		t.Errorf("renderLokiLabelCatalogTable() under Replicated database = %q; want ReplicatedAggregatingMergeTree engine", gotRepl)
	}
}

// TestRenderLokiLabelCatalogView_ExactSQL pins the refreshable MV's shape:
// REFRESH EVERY 5 MINUTE, the TO target, and a body that ARRAY JOINs
// mapKeys/mapValues(ResourceAttributes) in lockstep, excludes empty values,
// bounds the window to the trailing 24h via a re-evaluated `now() -
// toIntervalHour(24)` (NOT a literal frozen at CREATE time), and produces
// one uniqState(LabelValue) row per LabelKey.
func TestRenderLokiLabelCatalogView_ExactSQL(t *testing.T) {
	cfg := Config{}.withDefaults()
	got := renderLokiLabelCatalogView(cfg)
	want := "CREATE MATERIALIZED VIEW IF NOT EXISTS default.loki_label_catalog_mv REFRESH EVERY 5 MINUTE " +
		"TO default.loki_label_catalog AS " +
		"SELECT `LabelKey`, uniqState(`LabelValue`) AS `CardinalityState` " +
		"FROM `default`.`otel_logs` " +
		"ARRAY JOIN mapKeys(`ResourceAttributes`) AS `LabelKey`, mapValues(`ResourceAttributes`) AS `LabelValue` " +
		"WHERE `Timestamp` >= now() - toIntervalHour(24) AND `LabelValue` != '' " +
		"GROUP BY `LabelKey`"
	if got != want {
		t.Errorf("renderLokiLabelCatalogView() =\n  %s\nwant:\n  %s", got, want)
	}

	clusterCfg := Config{Cluster: "prod"}.withDefaults()
	gotCluster := renderLokiLabelCatalogView(clusterCfg)
	if !strings.Contains(gotCluster, "ON CLUSTER `prod`") {
		t.Errorf("renderLokiLabelCatalogView() with Cluster = %q; want an ON CLUSTER clause", gotCluster)
	}
}

// TestRenderSignal_LokiLabelCatalogEnabled pins the version-gate contract at
// the render layer, mirroring TestRenderSignal_TraceIDProjectionEnabled:
// LokiLabelCatalogEnabled=false (a below-24.10 deployment, or the feature
// simply off) renders NEITHER the catalog table NOR its view for Logs — the
// existing per-request /detected_labels path stays the only path, byte
// identical to before this feature existed — and true renders exactly one
// of each, table before view (the view's TO clause references the table).
func TestRenderSignal_LokiLabelCatalogEnabled(t *testing.T) {
	for _, tt := range []struct {
		name    string
		enabled bool
		want    int
	}{
		{"disabled", false, 0},
		{"enabled", true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{LokiLabelCatalogEnabled: tt.enabled}.withDefaults()
			stmts, err := renderSignal(cfg, Logs)
			if err != nil {
				t.Fatalf("renderSignal(Logs): %v", err)
			}
			gotTables, gotViews := 0, 0
			tableIdx, viewIdx := -1, -1
			for i, stmt := range stmts {
				if strings.Contains(stmt, "CREATE TABLE IF NOT EXISTS default.loki_label_catalog ") {
					gotTables++
					tableIdx = i
				}
				if strings.Contains(stmt, "CREATE MATERIALIZED VIEW IF NOT EXISTS default.loki_label_catalog_mv") {
					gotViews++
					viewIdx = i
				}
			}
			if gotTables != tt.want || gotViews != tt.want {
				t.Errorf("%d loki_label_catalog table statement(s), %d view statement(s); want %d each, rendered:\n%v", gotTables, gotViews, tt.want, stmts)
			}
			if tt.enabled && tableIdx >= viewIdx {
				t.Errorf("catalog table statement (index %d) must precede its view statement (index %d) — the view's TO clause references the table", tableIdx, viewIdx)
			}
		})
	}
}

// TestRenderSignal_LokiLabelCatalogEnabled_OtherSignalsUntouched confirms
// the catalog is scoped to Logs only — Metrics and Traces have no
// ResourceAttributes-as-log-stream-labels concept this feature applies to.
func TestRenderSignal_LokiLabelCatalogEnabled_OtherSignalsUntouched(t *testing.T) {
	cfg := Config{LokiLabelCatalogEnabled: true}.withDefaults()
	for _, sig := range []Signal{Metrics, Traces} {
		stmts, err := renderSignal(cfg, sig)
		if err != nil {
			t.Fatalf("renderSignal(%s): %v", sig, err)
		}
		for _, stmt := range stmts {
			if strings.Contains(stmt, "loki_label_catalog") {
				t.Errorf("%s: unexpected loki_label_catalog statement:\n%s", sig, stmt)
			}
		}
	}
}

// TestLokiLabelCatalogDefaultMatchesSchemaPackage pins defaultLokiLabelCatalogTable
// in lockstep with schema.DefaultOTelLogs().LabelCatalogTable — the same
// lockstep contract defaultMetricsDeltaPrefixTable's own doc comment
// promises against schema.DefaultOTelMetrics().DeltaPrefixTable.
func TestLokiLabelCatalogDefaultMatchesSchemaPackage(t *testing.T) {
	if defaultLokiLabelCatalogTable != schema.DefaultOTelLogs().LabelCatalogTable {
		t.Errorf("defaultLokiLabelCatalogTable = %q; want %q (schema.DefaultOTelLogs().LabelCatalogTable)",
			defaultLokiLabelCatalogTable, schema.DefaultOTelLogs().LabelCatalogTable)
	}
}
