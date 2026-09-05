package ddl

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
)

// TestDataShardCount_ZeroValueByteIdentical pins the backward-compat
// contract: DataShardCount unset (0) — and explicitly 1 — must render
// EXACTLY what renderSignal rendered before this field existed, for every
// signal. This is the Go-layer analogue of the Helm chart's own
// count==1-byte-identical acceptance criterion (cerberus issue #3077).
func TestDataShardCount_ZeroValueByteIdentical(t *testing.T) {
	base := Config{Database: "otel"}.withDefaults()
	withOne := base
	withOne.DataShardCount = 1

	for _, sig := range All {
		want, err := renderSignal(base, sig)
		if err != nil {
			t.Fatalf("renderSignal(%s) base: %v", sig, err)
		}
		got, err := renderSignal(withOne, sig)
		if err != nil {
			t.Fatalf("renderSignal(%s) DataShardCount=1: %v", sig, err)
		}
		if len(want) != len(got) {
			t.Fatalf("%s: DataShardCount=1 rendered %d statements, want %d", sig, len(got), len(want))
		}
		for i := range want {
			if want[i] != got[i] {
				t.Errorf("%s[%d]: DataShardCount=1 differs from DataShardCount=0:\n got:  %s\n want: %s", sig, i, got[i], want[i])
			}
		}
	}
}

// TestDataShardCount_RequiresCluster pins Validate's guard: a Distributed
// wrapper and its local table's ON CLUSTER DDL both need a named cluster.
func TestDataShardCount_RequiresCluster(t *testing.T) {
	cfg := Config{Database: "otel", DataShardCount: 2}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for DataShardCount>1 with no Cluster, got nil")
	} else if !strings.Contains(err.Error(), "Cluster") {
		t.Errorf("error does not name Cluster: %v", err)
	}

	cfg.Cluster = "bwc_cluster"
	if err := cfg.Validate(); err != nil {
		t.Errorf("DataShardCount>1 with Cluster set: unexpected error: %v", err)
	}
}

// TestDataShardCount_RejectsAuxCatalogFeatures pins Validate's explicit
// scope carve-out: each of the four opt-in features that introduce their
// own separately-named table + materialized view is rejected alongside
// DataShardCount>1 (issue #3077 wires the local/Distributed split for the
// base signal tables only — see Config.DataShardCount's doc).
func TestDataShardCount_RejectsAuxCatalogFeatures(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"DeltaPrefixEnabled", Config{DeltaPrefixEnabled: true}},
		{"DownsampleTierEnabled", Config{DownsampleTierEnabled: true}},
		{"LokiLabelCatalogEnabled", Config{LokiLabelCatalogEnabled: true}},
		{"TempoTagCatalogEnabled", Config{TempoTagCatalogEnabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Database = "otel"
			cfg.Cluster = "bwc_cluster"
			cfg.DataShardCount = 2
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error combining DataShardCount>1 with %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("error does not name %s: %v", tc.name, err)
			}
		})
	}

	// Every OTHER curated feature (a plain ALTER on the base/local table,
	// introducing no separately-named table) is NOT rejected.
	allowed := Config{
		Database:                 "otel",
		Cluster:                  "bwc_cluster",
		DataShardCount:           2,
		ColumnStatisticsEnabled:  true,
		TraceIDProjectionEnabled: true,
		TextIndexEnabled:         true,
	}
	if err := allowed.Validate(); err != nil {
		t.Errorf("DataShardCount>1 with ALTER-only curated features: unexpected error: %v", err)
	}
}

// TestDataShardCount_Metrics pins the Metrics signal's local+Distributed
// shape: all five metrics tables render under "_local" names (with the
// curated projection/temporality-index ALTERs following them, targeting the
// SAME local names), followed by five Distributed wrapper CREATEs under the
// ORIGINAL table names.
func TestDataShardCount_Metrics(t *testing.T) {
	cfg := Config{Database: "otel", Cluster: "bwc_cluster", DataShardCount: 2}.withDefaults()
	stmts, err := renderSignal(cfg, Metrics)
	if err != nil {
		t.Fatalf("renderSignal(Metrics): %v", err)
	}

	originals := []string{
		defaultMetricsGaugeTable, defaultMetricsSumTable, defaultMetricsHistogramTable,
		defaultMetricsExpHistogramTable, defaultMetricsSummaryTable,
	}

	// The first 5 statements are the LOCAL CREATE TABLEs.
	for i, base := range originals {
		want := base + dataShardLocalSuffix
		if !strings.Contains(stmts[i], "CREATE TABLE") || !strings.Contains(stmts[i], want) {
			t.Errorf("metrics[%d]: expected local CREATE TABLE for %s, got:\n%s", i, want, stmts[i])
		}
		if strings.Contains(stmts[i], "Distributed") {
			t.Errorf("metrics[%d]: local CREATE must not carry a Distributed engine:\n%s", i, stmts[i])
		}
	}

	// Every ALTER in between targets a "_local" table.
	var wrapperStmts []string
	for _, s := range stmts[len(originals):] {
		if strings.HasPrefix(s, "ALTER TABLE") {
			foundLocal := false
			for _, base := range originals {
				if strings.Contains(s, base+dataShardLocalSuffix) {
					foundLocal = true
				}
				if strings.Contains(s, "`"+base+"`") && !strings.Contains(s, base+dataShardLocalSuffix) {
					t.Errorf("ALTER targets the ORIGINAL (non-local) table name, want local: %s", s)
				}
			}
			if !foundLocal {
				t.Errorf("ALTER does not target any known local table: %s", s)
			}
			continue
		}
		wrapperStmts = append(wrapperStmts, s)
	}

	// The trailing statements are the five Distributed wrappers, one per
	// original table name, each pointed at its own local table.
	if len(wrapperStmts) != len(originals) {
		t.Fatalf("got %d trailing non-ALTER statements, want %d Distributed wrappers:\n%v", len(wrapperStmts), len(originals), wrapperStmts)
	}
	for i, base := range originals {
		s := wrapperStmts[i]
		wantCreate := "CREATE TABLE IF NOT EXISTS otel." + base + " ON CLUSTER `bwc_cluster` AS otel." + base + dataShardLocalSuffix
		if !strings.HasPrefix(s, wantCreate) {
			t.Errorf("wrapper[%d] = %q; want prefix %q", i, s, wantCreate)
		}
		wantEngine := "ENGINE = Distributed('bwc_cluster', 'otel', '" + base + dataShardLocalSuffix + "', rand())"
		if !strings.HasSuffix(s, wantEngine) {
			t.Errorf("wrapper[%d] = %q; want suffix %q", i, s, wantEngine)
		}
	}
}

// TestDataShardCount_Logs pins the Logs signal: the local CREATE plus its
// (unconditional) curated codec ALTER — issue #2768's Body codec applies
// regardless of any *Enabled flag — both targeting the local table, followed
// by one Distributed wrapper under the original name.
func TestDataShardCount_Logs(t *testing.T) {
	cfg := Config{Database: "otel", Cluster: "bwc_cluster", DataShardCount: 3}.withDefaults()
	stmts, err := renderSignal(cfg, Logs)
	if err != nil {
		t.Fatalf("renderSignal(Logs): %v", err)
	}
	if len(stmts) != 3 {
		t.Fatalf("got %d statements, want 3 (local CREATE + codec ALTER + Distributed wrapper): %v", len(stmts), stmts)
	}
	local := defaultLogsTable + dataShardLocalSuffix
	if !strings.Contains(stmts[0], local) {
		t.Errorf("logs[0] (CREATE) does not target the local table: %s", stmts[0])
	}
	if !strings.HasPrefix(stmts[1], "ALTER TABLE") || !strings.Contains(stmts[1], local) {
		t.Errorf("logs[1] (codec ALTER) does not target the local table: %s", stmts[1])
	}
	wantWrapper := "CREATE TABLE IF NOT EXISTS otel." + defaultLogsTable + " ON CLUSTER `bwc_cluster` AS otel." + local +
		" ENGINE = Distributed('bwc_cluster', 'otel', '" + local + "', rand())"
	if stmts[2] != wantWrapper {
		t.Errorf("logs[2] = %q; want %q", stmts[2], wantWrapper)
	}
}

// TestDataShardCount_Traces pins the Traces signal's DERIVED trace_id_ts
// naming: suffixing Tables.Traces alone (via dataShardLocalConfig) is
// enough for the upstream template to render the lookup table AND its MV
// under the correctly-suffixed name with no separate field to touch, and
// both the spans table and the lookup table get their own Distributed
// wrapper.
func TestDataShardCount_Traces(t *testing.T) {
	cfg := Config{Database: "otel", Cluster: "bwc_cluster", DataShardCount: 2}.withDefaults()
	stmts, err := renderSignal(cfg, Traces)
	if err != nil {
		t.Fatalf("renderSignal(Traces): %v", err)
	}
	// spans local, ts-lookup local, ts-lookup MV, the (unconditional) curated
	// Duration codec ALTER (issue #2768, targets the spans local table), then
	// 2 Distributed wrappers.
	if len(stmts) != 6 {
		t.Fatalf("got %d statements, want 6: %v", len(stmts), stmts)
	}
	localSpans := defaultTracesTable + dataShardLocalSuffix
	localTs := localSpans + traceIDTsTableSuffix
	if !strings.Contains(stmts[0], localSpans) {
		t.Errorf("traces[0] (spans) does not target %s: %s", localSpans, stmts[0])
	}
	if !strings.Contains(stmts[1], localTs) {
		t.Errorf("traces[1] (ts lookup) does not target %s: %s", localTs, stmts[1])
	}
	if !strings.Contains(stmts[2], "MATERIALIZED VIEW") || !strings.Contains(stmts[2], localTs) {
		t.Errorf("traces[2] (ts lookup MV) does not target %s: %s", localTs, stmts[2])
	}
	if !strings.HasPrefix(stmts[3], "ALTER TABLE") || !strings.Contains(stmts[3], localSpans) {
		t.Errorf("traces[3] (codec ALTER) does not target %s: %s", localSpans, stmts[3])
	}

	wantSpansWrapper := "CREATE TABLE IF NOT EXISTS otel." + defaultTracesTable + " ON CLUSTER `bwc_cluster` AS otel." + localSpans +
		" ENGINE = Distributed('bwc_cluster', 'otel', '" + localSpans + "', rand())"
	if stmts[4] != wantSpansWrapper {
		t.Errorf("traces[4] = %q; want %q", stmts[4], wantSpansWrapper)
	}
	origTs := defaultTracesTable + traceIDTsTableSuffix
	wantTsWrapper := "CREATE TABLE IF NOT EXISTS otel." + origTs + " ON CLUSTER `bwc_cluster` AS otel." + localTs +
		" ENGINE = Distributed('bwc_cluster', 'otel', '" + localTs + "', rand())"
	if stmts[5] != wantTsWrapper {
		t.Errorf("traces[5] = %q; want %q", stmts[5], wantTsWrapper)
	}
}

// TestDataShardCount_ReplicatedCombination pins the combination the design
// explicitly calls out as needing a real test (cerberus issue #3077's
// acceptance criteria): schema.replicated.enabled=true (DatabaseEngine.
// Replicated) together with DataShardCount>1, sharing the SAME
// {shard}/{replica} macro slot. The local tables must render the BARE
// ReplicatedMergeTree engine (no explicit args — a Replicated database
// rejects them with code 36, see TestRenderSignal_ReplicatedDatabaseDefaultsToReplicatedMergeTree),
// exactly as they would with DataShardCount<=1; DataShardCount changes only
// which table NAME that engine attaches to plus the trailing Distributed
// wrapper, never the engine-selection logic.
func TestDataShardCount_ReplicatedCombination(t *testing.T) {
	cfg := Config{
		Database: "otel",
		Cluster:  "bwc_cluster",
		DatabaseEngine: DatabaseEngine{
			Replicated:        true,
			ReplicatedZooPath: "/clickhouse/databases/otel/{shard}/{replica}",
		},
		DataShardCount: 2,
	}.withDefaults()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	for _, sig := range All {
		stmts, err := renderSignal(cfg, sig)
		if err != nil {
			t.Fatalf("renderSignal(%s): %v", sig, err)
		}
		for i, s := range stmts {
			if strings.HasPrefix(s, "ALTER TABLE") || strings.Contains(s, "MATERIALIZED VIEW") || strings.Contains(s, "Distributed") {
				continue
			}
			if !strings.Contains(s, "ReplicatedMergeTree") {
				t.Errorf("%s[%d]: want bare ReplicatedMergeTree on the local table:\n%s", sig, i, s)
			}
			if strings.Contains(s, "ReplicatedMergeTree(") {
				t.Errorf("%s[%d]: ReplicatedMergeTree must take NO args under a Replicated database:\n%s", sig, i, s)
			}
		}
	}
}

// TestDataShardCount_CustomShardingKey pins DataShardingKey overriding the
// rand() default — the sharding-key Frag renders verbatim as the
// Distributed engine's fourth argument.
func TestDataShardCount_CustomShardingKey(t *testing.T) {
	cfg := Config{
		Database:        "otel",
		Cluster:         "bwc_cluster",
		DataShardCount:  2,
		DataShardingKey: chsql.Call("cityHash64", chsql.Col("TraceId")),
	}.withDefaults()
	stmts, err := renderSignal(cfg, Traces)
	if err != nil {
		t.Fatalf("renderSignal(Traces): %v", err)
	}
	last := stmts[len(stmts)-1]
	if !strings.Contains(last, "cityHash64(`TraceId`)") {
		t.Errorf("custom sharding key not rendered: %s", last)
	}
	if strings.Contains(last, "rand()") {
		t.Errorf("default rand() must not render alongside a custom sharding key: %s", last)
	}
}

// TestDataShardCount_MatchesApply mirrors TestRenderAll_MatchesApply under
// DataShardCount>1: the offline RenderAll preview must return exactly what
// ApplyWithConfig executes, local+Distributed statements included.
func TestDataShardCount_MatchesApply(t *testing.T) {
	cfg := Config{Database: "otel", Cluster: "bwc_cluster", DataShardCount: 2}

	rc := &recordingConn{}
	if err := ApplyWithConfig(context.Background(), rc, cfg, All); err != nil {
		t.Fatalf("ApplyWithConfig: %v", err)
	}
	rendered, err := RenderAll(cfg, All)
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	if len(rendered) != len(rc.execs) {
		t.Fatalf("RenderAll returned %d statements, ApplyWithConfig executed %d", len(rendered), len(rc.execs))
	}
	for i := range rendered {
		if rendered[i] != rc.execs[i] {
			t.Errorf("statement %d differs:\n  render: %s\n  apply:  %s", i, rendered[i], rc.execs[i])
		}
	}
}
