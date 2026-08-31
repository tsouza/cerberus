package ddl

import (
	"strings"
	"testing"
)

// TestRenderTempoTagCatalogTable_ExactSQL pins the catalog table's shape
// (cerberus issue #2771): String Scope + TagKey ORDER BY (Scope, TagKey)
// and an AggregateFunction(topK(50), String) TopValuesState column on
// AggregatingMergeTree (or ReplicatedAggregatingMergeTree under a
// Replicated database engine) — the Tempo sibling of
// TestRenderLokiLabelCatalogTable_ExactSQL.
func TestRenderTempoTagCatalogTable_ExactSQL(t *testing.T) {
	cfg := Config{}.withDefaults()
	got := renderTempoTagCatalogTable(cfg)
	want := "CREATE TABLE IF NOT EXISTS default.tempo_tag_catalog " +
		"(`Scope` String, `TagKey` String, `TopValuesState` AggregateFunction(topK(50), String)) " +
		"ENGINE = AggregatingMergeTree ORDER BY (`Scope`, `TagKey`)"
	if got != want {
		t.Errorf("renderTempoTagCatalogTable() =\n  %s\nwant:\n  %s", got, want)
	}

	replCfg := Config{DatabaseEngine: DatabaseEngine{Replicated: true, ReplicatedZooPath: "/x"}}.withDefaults()
	gotRepl := renderTempoTagCatalogTable(replCfg)
	if !strings.Contains(gotRepl, "ENGINE = ReplicatedAggregatingMergeTree") {
		t.Errorf("renderTempoTagCatalogTable() under Replicated database = %q; want ReplicatedAggregatingMergeTree engine", gotRepl)
	}
}

// TestRenderTempoTagCatalogView_ExactSQL pins the refreshable MV's shape:
// REFRESH EVERY 5 MINUTE, the TO target, a UNION ALL of the resource-scope
// and span-scope ARRAY JOIN arms (each bounding the window to the trailing
// hour via a re-evaluated `now() - toIntervalHour(1)`, excluding empty
// values), and a topKState(50)(TagValue) aggregate GROUP BY (Scope, TagKey).
func TestRenderTempoTagCatalogView_ExactSQL(t *testing.T) {
	cfg := Config{}.withDefaults()
	got := renderTempoTagCatalogView(cfg)
	want := "CREATE MATERIALIZED VIEW IF NOT EXISTS default.tempo_tag_catalog_mv REFRESH EVERY 5 MINUTE " +
		"TO default.tempo_tag_catalog AS " +
		"SELECT `Scope`, `TagKey`, topKState(50)(`TagValue`) AS `TopValuesState` " +
		"FROM ((SELECT 'resource' AS `Scope`, `k` AS `TagKey`, `v` AS `TagValue` " +
		"FROM `default`.`otel_traces` " +
		"ARRAY JOIN mapKeys(`ResourceAttributes`) AS `k`, mapValues(`ResourceAttributes`) AS `v` " +
		"WHERE `Timestamp` >= now() - toIntervalHour(1) AND `v` != '') " +
		"UNION ALL " +
		"(SELECT 'span' AS `Scope`, `k` AS `TagKey`, `v` AS `TagValue` " +
		"FROM `default`.`otel_traces` " +
		"ARRAY JOIN mapKeys(`SpanAttributes`) AS `k`, mapValues(`SpanAttributes`) AS `v` " +
		"WHERE `Timestamp` >= now() - toIntervalHour(1) AND `v` != '')) " +
		"GROUP BY `Scope`, `TagKey`"
	if got != want {
		t.Errorf("renderTempoTagCatalogView() =\n  %s\nwant:\n  %s", got, want)
	}

	clusterCfg := Config{Cluster: "prod"}.withDefaults()
	gotCluster := renderTempoTagCatalogView(clusterCfg)
	if !strings.Contains(gotCluster, "ON CLUSTER `prod`") {
		t.Errorf("renderTempoTagCatalogView() with Cluster = %q; want an ON CLUSTER clause", gotCluster)
	}
}

// TestRenderSignal_TempoTagCatalogEnabled pins the version-gate contract at
// the render layer, mirroring TestRenderSignal_LokiLabelCatalogEnabled:
// TempoTagCatalogEnabled=false (a below-24.10 deployment, or the feature
// simply off) renders NEITHER the catalog table NOR its view for Traces —
// the existing live attribute-map scan stays the only path, byte identical
// to before this feature existed — and true renders exactly one of each,
// table before view (the view's TO clause references the table).
func TestRenderSignal_TempoTagCatalogEnabled(t *testing.T) {
	for _, tt := range []struct {
		name    string
		enabled bool
		want    int
	}{
		{"disabled", false, 0},
		{"enabled", true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{TempoTagCatalogEnabled: tt.enabled}.withDefaults()
			stmts, err := renderSignal(cfg, Traces)
			if err != nil {
				t.Fatalf("renderSignal(Traces): %v", err)
			}
			gotTables, gotViews := 0, 0
			tableIdx, viewIdx := -1, -1
			for i, stmt := range stmts {
				if strings.Contains(stmt, "CREATE TABLE IF NOT EXISTS default.tempo_tag_catalog ") {
					gotTables++
					tableIdx = i
				}
				if strings.Contains(stmt, "CREATE MATERIALIZED VIEW IF NOT EXISTS default.tempo_tag_catalog_mv") {
					gotViews++
					viewIdx = i
				}
			}
			if gotTables != tt.want || gotViews != tt.want {
				t.Errorf("%d tempo_tag_catalog table statement(s), %d view statement(s); want %d each, rendered:\n%v", gotTables, gotViews, tt.want, stmts)
			}
			if tt.enabled && tableIdx >= viewIdx {
				t.Errorf("catalog table statement (index %d) must precede its view statement (index %d) — the view's TO clause references the table", tableIdx, viewIdx)
			}
		})
	}
}

// TestRenderSignal_TempoTagCatalogEnabled_OtherSignalsUntouched confirms
// the catalog is scoped to Traces only — Metrics and Logs have no
// span/resource-attribute-map-tag concept this feature applies to.
func TestRenderSignal_TempoTagCatalogEnabled_OtherSignalsUntouched(t *testing.T) {
	cfg := Config{TempoTagCatalogEnabled: true}.withDefaults()
	for _, sig := range []Signal{Metrics, Logs} {
		stmts, err := renderSignal(cfg, sig)
		if err != nil {
			t.Fatalf("renderSignal(%s): %v", sig, err)
		}
		for _, stmt := range stmts {
			if strings.Contains(stmt, "tempo_tag_catalog") {
				t.Errorf("%s: unexpected tempo_tag_catalog statement:\n%s", sig, stmt)
			}
		}
	}
}
