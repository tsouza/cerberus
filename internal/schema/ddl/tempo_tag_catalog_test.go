package ddl

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestTempoTagCatalogScopeArmFor_CoversEveryCatalogScope proves
// tempoTagCatalogScopeArmFor's switch is exhaustive over
// schema.TagCatalogCoveredScopes — the canonical list
// renderTempoTagCatalogView now ranges over to build its UNION ALL arms
// (cerberus issue #3021, closing the DRY gap #3019 left open on the write
// side). A scope reaching the switch without a case panics (see that
// function's doc); this test converts that panic into a named test
// failure for every entry the canonical list carries today, so removing a
// case here without also removing the scope from
// schema.TagCatalogCoveredScopes fails loudly in CI instead of only
// panicking against a live cluster.
func TestTempoTagCatalogScopeArmFor_CoversEveryCatalogScope(t *testing.T) {
	cfg := Config{}.withDefaults()
	windowStart := chsql.InlineLit(int64(0))
	for _, scope := range schema.TagCatalogCoveredScopes {
		t.Run(scope, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("tempoTagCatalogScopeArmFor(%q) panicked: %v", scope, r)
				}
			}()
			if arm := tempoTagCatalogScopeArmFor(cfg, scope, windowStart); arm == nil {
				t.Fatalf("tempoTagCatalogScopeArmFor(%q) returned a nil arm", scope)
			}
		})
	}
}

// TestTempoTagCatalogScopeArmFor_UnhandledScopePanics is this file's
// non-vacuity proof for the completeness test above: an unhandled scope
// value (one schema.TagCatalogCoveredScopes does not carry) panics naming
// that value. That panic is exactly the mechanism that would turn a
// future scope added to schema.TagCatalogCoveredScopes without a matching
// case in tempoTagCatalogScopeArmFor into a loud CoversEveryCatalogScope
// failure instead of a silently missing UNION ALL arm.
func TestTempoTagCatalogScopeArmFor_UnhandledScopePanics(t *testing.T) {
	cfg := Config{}.withDefaults()
	windowStart := chsql.InlineLit(int64(0))
	const unhandledScope = "instrumentation"
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("tempoTagCatalogScopeArmFor(%q) did not panic", unhandledScope)
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, unhandledScope) {
			t.Fatalf("panic = %v; want it to name the unhandled scope %q", r, unhandledScope)
		}
	}()
	tempoTagCatalogScopeArmFor(cfg, unhandledScope, windowStart)
}

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
// REFRESH EVERY 5 MINUTE, the TO target, a UNION ALL of the resource-scope,
// span-scope, event-scope and link-scope ARRAY JOIN arms (cerberus issue
// #2850 added the latter two — the SCOPE COVERAGE doc above
// tempoTagCatalogNestedScopeArm has the measured cost that justified it)
// — each bounding the window to the trailing hour via a re-evaluated
// `now() - toIntervalHour(1)`, excluding empty values — and a
// topKState(50)(TagValue) aggregate GROUP BY (Scope, TagKey).
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
		"WHERE `Timestamp` >= now() - toIntervalHour(1) AND `v` != '') " +
		"UNION ALL " +
		"(SELECT 'event' AS `Scope`, `k` AS `TagKey`, `v` AS `TagValue` " +
		"FROM `default`.`otel_traces` " +
		"ARRAY JOIN arrayFlatten(arrayMap(m -> mapKeys(m), `Events`.`Attributes`)) AS `k`, " +
		"arrayFlatten(arrayMap(m -> mapValues(m), `Events`.`Attributes`)) AS `v` " +
		"WHERE `Timestamp` >= now() - toIntervalHour(1) AND `v` != '') " +
		"UNION ALL " +
		"(SELECT 'link' AS `Scope`, `k` AS `TagKey`, `v` AS `TagValue` " +
		"FROM `default`.`otel_traces` " +
		"ARRAY JOIN arrayFlatten(arrayMap(m -> mapKeys(m), `Links`.`Attributes`)) AS `k`, " +
		"arrayFlatten(arrayMap(m -> mapValues(m), `Links`.`Attributes`)) AS `v` " +
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
