package ddl

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRenderSignal_AllTablesCarryIfNotExists is the rendering-layer
// equivalent of the Apply-twice integration test: every CREATE
// statement under every signal must carry an IF NOT EXISTS clause so a
// second Apply against an already-populated CH is a no-op. The
// integration test asserts the end-to-end behavior; this test pins the
// contract at the template-render boundary so a future template bump
// can't silently drop the clause.
func TestRenderSignal_AllTablesCarryIfNotExists(t *testing.T) {
	cfg := Config{}.withDefaults()
	for _, sig := range All {
		stmts, err := renderSignal(cfg, sig)
		if err != nil {
			t.Fatalf("renderSignal(%s): %v", sig, err)
		}
		for i, stmt := range stmts {
			if !strings.Contains(stmt, "IF NOT EXISTS") {
				t.Errorf("%s[%d]: missing IF NOT EXISTS — re-apply would fail:\n%s", sig, i, stmt)
			}
		}
	}
}

// TestRenderCreateDatabase pins the database-bootstrap statement: it must
// carry IF NOT EXISTS (idempotent re-apply), name the resolved database, and
// render the ON CLUSTER clause only when a cluster is configured. This is the
// render-layer guard for the cold-cluster bug where a missing
// CREATE DATABASE left every CREATE TABLE failing with "Database otel does not
// exist".
func TestRenderCreateDatabase(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantSub []string
		notSub  []string
	}{
		{
			name:    "default database",
			cfg:     Config{}.withDefaults(),
			wantSub: []string{"CREATE DATABASE IF NOT EXISTS default"},
			notSub:  []string{"ON CLUSTER"},
		},
		{
			name:    "override database",
			cfg:     Config{Database: "otel"}.withDefaults(),
			wantSub: []string{"CREATE DATABASE IF NOT EXISTS otel"},
			notSub:  []string{"ON CLUSTER"},
		},
		{
			name:    "on cluster",
			cfg:     Config{Database: "otel", Cluster: "prod"}.withDefaults(),
			wantSub: []string{"CREATE DATABASE IF NOT EXISTS otel", "ON CLUSTER `prod`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderCreateDatabase(tc.cfg)
			for _, sub := range tc.wantSub {
				if !strings.Contains(got, sub) {
					t.Errorf("renderCreateDatabase = %q; want substring %q", got, sub)
				}
			}
			for _, sub := range tc.notSub {
				if strings.Contains(got, sub) {
					t.Errorf("renderCreateDatabase = %q; should not contain %q", got, sub)
				}
			}
		})
	}
}

// TestRenderCreateDatabase_ReplicatedEngine pins the Replicated database
// engine path: the CREATE DATABASE statement carries
// `ENGINE = Replicated(<path>, <shard>, <replica>)`, the shard/replica
// default to the server macros when unset, and an explicit override is
// honoured. This is the squid-style clustered shape where the Replicated
// database auto-replicates DDL (the tables themselves get an explicit
// ReplicatedMergeTree engine to replicate their DATA).
func TestRenderCreateDatabase_ReplicatedEngine(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "macro defaults",
			cfg: Config{
				Database: "otel",
				DatabaseEngine: DatabaseEngine{
					Replicated:        true,
					ReplicatedZooPath: "/clickhouse/databases/otel",
				},
			}.withDefaults(),
			want: "CREATE DATABASE IF NOT EXISTS otel ENGINE = Replicated('/clickhouse/databases/otel', '{shard}', '{replica}')",
		},
		{
			name: "explicit shard and replica",
			cfg: Config{
				Database: "otel",
				DatabaseEngine: DatabaseEngine{
					Replicated:        true,
					ReplicatedZooPath: "/clickhouse/databases/otel",
					ReplicatedShard:   "shard0",
					ReplicatedReplica: "clickhouse-shard0-0",
				},
			}.withDefaults(),
			want: "CREATE DATABASE IF NOT EXISTS otel ENGINE = Replicated('/clickhouse/databases/otel', 'shard0', 'clickhouse-shard0-0')",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderCreateDatabase(tc.cfg); got != tc.want {
				t.Errorf("renderCreateDatabase:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestWithDefaults_ReplicatedMacroDefaults pins that withDefaults fills the
// shard/replica macros only when the Replicated engine is enabled, and
// leaves an Atomic (zero-value) config untouched.
func TestWithDefaults_ReplicatedMacroDefaults(t *testing.T) {
	atomic := Config{Database: "otel"}.withDefaults()
	if atomic.DatabaseEngine.Replicated {
		t.Fatalf("zero-value DatabaseEngine must stay non-replicated")
	}
	if atomic.DatabaseEngine.ReplicatedShard != "" || atomic.DatabaseEngine.ReplicatedReplica != "" {
		t.Errorf("Atomic config must not get macro defaults, got shard=%q replica=%q",
			atomic.DatabaseEngine.ReplicatedShard, atomic.DatabaseEngine.ReplicatedReplica)
	}

	repl := Config{
		Database:       "otel",
		DatabaseEngine: DatabaseEngine{Replicated: true, ReplicatedZooPath: "/p"},
	}.withDefaults()
	if repl.DatabaseEngine.ReplicatedShard != "{shard}" || repl.DatabaseEngine.ReplicatedReplica != "{replica}" {
		t.Errorf("Replicated config must default the macros, got shard=%q replica=%q",
			repl.DatabaseEngine.ReplicatedShard, repl.DatabaseEngine.ReplicatedReplica)
	}
}

// TestApplyWithConfig_ReplicatedRequiresZooPath pins the validation: a
// Replicated database engine with no ZooKeeper/Keeper path is a
// misconfiguration that fails fast (before touching the nil conn), not a
// statement that renders `Replicated(”, ...)`. The validation is eager —
// it fires regardless of which signals are requested, INCLUDING the
// empty-signals path, so a bad config can't hide behind a zero-signal call.
func TestApplyWithConfig_ReplicatedRequiresZooPath(t *testing.T) {
	cfg := Config{
		Database:       "otel",
		DatabaseEngine: DatabaseEngine{Replicated: true}, // no ReplicatedZooPath
	}
	signalSets := map[string][]Signal{
		"all signals":   All,
		"empty signals": {},
		"nil signals":   nil,
	}
	for name, signals := range signalSets {
		t.Run(name, func(t *testing.T) {
			err := ApplyWithConfig(context.TODO(), nil, cfg, signals)
			if err == nil {
				t.Fatal("Replicated engine with empty zoo path must error")
			}
			if !strings.Contains(err.Error(), "ReplicatedZooPath") {
				t.Errorf("error should name the missing field, got: %v", err)
			}
		})
	}
}

// TestRenderCreateDatabase_BackticksEscaped pins backtick handling end to
// end through renderCreateDatabase: the cluster name is backtick-quoted
// with embedded backticks doubled (via the typed OnCluster constructor),
// inside the full CREATE DATABASE statement — not just the OnCluster
// fragment in isolation.
func TestRenderCreateDatabase_BackticksEscaped(t *testing.T) {
	cfg := Config{Database: "otel", Cluster: "a`b"}.withDefaults()
	got := renderCreateDatabase(cfg)
	want := "CREATE DATABASE IF NOT EXISTS otel ON CLUSTER `a``b`"
	if got != want {
		t.Errorf("renderCreateDatabase:\n got: %s\nwant: %s", got, want)
	}
}

// TestRenderSignal_TTLZeroWithReplicatedEngine renders every signal with no
// TTL and a Replicated database engine: the table statements must carry no
// TTL clause (the nil TTL frag coalesces to an empty slot), while the
// CREATE DATABASE statement carries the Replicated engine clause. Pins that
// the two orthogonal axes (TTL off, Replicated DB on) compose cleanly.
func TestRenderSignal_TTLZeroWithReplicatedEngine(t *testing.T) {
	cfg := Config{
		Database: "otel",
		DatabaseEngine: DatabaseEngine{
			Replicated:        true,
			ReplicatedZooPath: "/clickhouse/databases/otel",
		},
	}.withDefaults()

	dbStmt := renderCreateDatabase(cfg)
	if !strings.Contains(dbStmt, "ENGINE = Replicated('/clickhouse/databases/otel', '{shard}', '{replica}')") {
		t.Errorf("CREATE DATABASE missing Replicated engine clause:\n%s", dbStmt)
	}

	for _, sig := range All {
		stmts, err := renderSignal(cfg, sig)
		if err != nil {
			t.Fatalf("renderSignal(%s): %v", sig, err)
		}
		for i, stmt := range stmts {
			if strings.Contains(stmt, "TTL toDateTime") {
				t.Errorf("%s[%d]: TTL=0 must emit no TTL clause:\n%s", sig, i, stmt)
			}
		}
	}
}

// TestRenderSignal_LogsOnlySubset emulates a deployment that only wants
// the logs signal. The render layer must produce the single logs
// statement and nothing else — Apply iterates per-signal so any
// "metrics tables leak when only Logs requested" regression surfaces
// here.
func TestRenderSignal_LogsOnlySubset(t *testing.T) {
	cfg := Config{}.withDefaults()
	stmts, err := renderSignal(cfg, Logs)
	if err != nil {
		t.Fatalf("renderSignal(Logs): %v", err)
	}
	if len(stmts) != 1 {
		t.Fatalf("logs subset: got %d statements; want 1", len(stmts))
	}
	if !strings.Contains(stmts[0], "otel_logs") {
		t.Errorf("logs subset[0]: missing otel_logs table name")
	}
	if strings.Contains(stmts[0], "otel_metrics") || strings.Contains(stmts[0], "otel_traces") {
		t.Errorf("logs subset[0]: leaked unrelated tables:\n%s", stmts[0])
	}
}

// TestRenderSignal_TracesOnlySubset confirms the three traces statements
// (spans table, lookup table, MV) render together and nothing else.
// Order matters because the MV references the lookup table.
func TestRenderSignal_TracesOnlySubset(t *testing.T) {
	cfg := Config{}.withDefaults()
	stmts, err := renderSignal(cfg, Traces)
	if err != nil {
		t.Fatalf("renderSignal(Traces): %v", err)
	}
	if len(stmts) != 3 {
		t.Fatalf("traces subset: got %d statements; want 3", len(stmts))
	}
	// Statement order is load-bearing: the MV (stmts[2]) selects FROM
	// the spans table (stmts[0]) and writes INTO the lookup table
	// (stmts[1]). Reverse order would error on CH.
	if !strings.Contains(stmts[0], "CREATE TABLE IF NOT EXISTS") {
		t.Errorf("traces[0] should be the spans table CREATE")
	}
	if !strings.Contains(stmts[1], "otel_traces_trace_id_ts") || strings.Contains(stmts[1], "MATERIALIZED VIEW") {
		t.Errorf("traces[1] should be the lookup table:\n%s", stmts[1])
	}
	if !strings.Contains(stmts[2], "CREATE MATERIALIZED VIEW IF NOT EXISTS") {
		t.Errorf("traces[2] should be the MV CREATE")
	}
}

// TestRenderSignal_MetricsOnlySubset confirms the five metrics statements
// render without leaking logs or traces tables.
func TestRenderSignal_MetricsOnlySubset(t *testing.T) {
	cfg := Config{}.withDefaults()
	stmts, err := renderSignal(cfg, Metrics)
	if err != nil {
		t.Fatalf("renderSignal(Metrics): %v", err)
	}
	if len(stmts) != 5 {
		t.Fatalf("metrics subset: got %d statements; want 5", len(stmts))
	}
	for i, stmt := range stmts {
		if strings.Contains(stmt, "otel_logs") || strings.Contains(stmt, "otel_traces") {
			t.Errorf("metrics[%d] leaked an unrelated table:\n%s", i, stmt)
		}
	}
}

// TestRenderSignal_CustomDatabasePropagates verifies that an override
// flows through *every* statement under *every* signal — this is the
// contract main.go relies on when an operator sets
// CERBERUS_CH_DATABASE to a non-default value.
func TestRenderSignal_CustomDatabasePropagates(t *testing.T) {
	cfg := Config{Database: "tenant_alpha"}.withDefaults()
	for _, sig := range All {
		stmts, _ := renderSignal(cfg, sig)
		for i, stmt := range stmts {
			if !strings.Contains(stmt, "tenant_alpha") {
				t.Errorf("%s[%d]: custom database not rendered:\n%s", sig, i, stmt)
			}
			if strings.Contains(stmt, `"default"`) && !strings.Contains(stmt, "tenant_alpha") {
				t.Errorf("%s[%d]: leaked default DB literal:\n%s", sig, i, stmt)
			}
		}
	}
}

// TestRenderSignal_CustomTablesPropagate verifies per-table-name
// overrides reach the rendered DDL for every signal. This pins the
// schema-rename contract: an operator who wants their tables called
// e.g. `acme_logs` instead of `otel_logs` can do so without forking the
// upstream templates.
func TestRenderSignal_CustomTablesPropagate(t *testing.T) {
	cfg := Config{
		Tables: Tables{
			Logs:                "acme_logs",
			Traces:              "acme_traces",
			MetricsGauge:        "acme_gauge",
			MetricsSum:          "acme_sum",
			MetricsHistogram:    "acme_histogram",
			MetricsExpHistogram: "acme_exp_histogram",
			MetricsSummary:      "acme_summary",
		},
	}.withDefaults()

	// Logs: single statement.
	logs, _ := renderSignal(cfg, Logs)
	if !strings.Contains(logs[0], "acme_logs") {
		t.Errorf("logs override leaked: %s", logs[0])
	}
	// Metrics: 5 statements, each with its own custom name.
	metrics, _ := renderSignal(cfg, Metrics)
	wantMetrics := []string{"acme_gauge", "acme_sum", "acme_histogram", "acme_exp_histogram", "acme_summary"}
	for i, w := range wantMetrics {
		if !strings.Contains(metrics[i], w) {
			t.Errorf("metrics[%d]: missing custom name %q in:\n%s", i, w, metrics[i])
		}
	}
	// Traces: 3 statements, all referencing acme_traces in some
	// shape (the lookup uses the base name as a prefix).
	traces, _ := renderSignal(cfg, Traces)
	for i, stmt := range traces {
		if !strings.Contains(stmt, "acme_traces") {
			t.Errorf("traces[%d]: missing custom base name:\n%s", i, stmt)
		}
	}
}

// TestRenderSignal_TablesEmptyFallBackToDefaults pins withDefaults's
// per-field semantics: leaving any Tables field empty must fall back to
// the upstream name. Half-overridden configs (a common deployment style
// where only a few names diverge) must not silently zero the others.
func TestRenderSignal_TablesEmptyFallBackToDefaults(t *testing.T) {
	cfg := Config{Tables: Tables{Logs: "shiny_logs"}}.withDefaults()
	logs, _ := renderSignal(cfg, Logs)
	if !strings.Contains(logs[0], "shiny_logs") {
		t.Errorf("logs override missing: %s", logs[0])
	}
	metrics, _ := renderSignal(cfg, Metrics)
	// Metrics names left empty in the override must still surface as
	// the upstream defaults.
	for _, want := range []string{
		"otel_metrics_gauge",
		"otel_metrics_sum",
		"otel_metrics_histogram",
		"otel_metrics_exp_histogram",
		"otel_metrics_summary",
	} {
		found := false
		for _, stmt := range metrics {
			if strings.Contains(stmt, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("metrics: default %q leaked into a non-default name", want)
		}
	}
}

// TestRenderSignal_ConcurrentRendersDoNotRace fires many parallel
// renderSignal calls. The package keeps no mutable global state (every
// withDefaults call returns a copy) so racing through the function
// must not produce stale strings or panics. Run with `-race` to make
// the assertion teeth.
func TestRenderSignal_ConcurrentRendersDoNotRace(t *testing.T) {
	const N = 32
	var wg sync.WaitGroup
	cfg := Config{Database: "ddl_concurrent"}.withDefaults()
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, sig := range All {
				stmts, err := renderSignal(cfg, sig)
				if err != nil {
					t.Errorf("renderSignal(%s): %v", sig, err)
					return
				}
				for _, stmt := range stmts {
					if !strings.Contains(stmt, "ddl_concurrent") {
						t.Errorf("%s: lost custom DB in concurrent render:\n%s", sig, stmt)
					}
				}
			}
		}()
	}
	wg.Wait()
}

// TestRenderSignal_TTLZeroSkipsTTLClause: the documented "no TTL" path
// produces statements without a TTL keyword. Cerberus's default leaves
// retention to the operator.
func TestRenderSignal_TTLZeroSkipsTTLClause(t *testing.T) {
	cfg := Config{TTL: TTL{}}.withDefaults()
	for _, sig := range All {
		stmts, _ := renderSignal(cfg, sig)
		for i, stmt := range stmts {
			// The traces MV doesn't carry a TTL clause but contains
			// other "TTL" substrings; check we don't see "TTL toDate..."
			// which is what the rendered TTL expression looks like.
			if strings.Contains(stmt, "TTL toDateTime") {
				t.Errorf("%s[%d]: zero TTL rendered an expression:\n%s", sig, i, stmt)
			}
		}
	}
}

// TestRenderSignal_TTLAppliesCorrectTimeFieldPerSignal: the time field
// the TTL expression uses must match what the upstream exporter expects
// — Logs and Traces use toDateTime(Timestamp) (logs moved off the
// removed TimestampTime column in upstream v0.150.0), Metrics use
// toDateTime(TimeUnix). A swap would silently render valid SQL against
// the wrong column.
func TestRenderSignal_TTLAppliesCorrectTimeFieldPerSignal(t *testing.T) {
	cfg := Config{TTL: TTL{Metrics: 24 * time.Hour, Logs: 24 * time.Hour, Traces: 24 * time.Hour}}.withDefaults()
	logs, _ := renderSignal(cfg, Logs)
	if !strings.Contains(logs[0], "TTL toDateTime(Timestamp) + toIntervalDay(1)") {
		t.Errorf("logs TTL: missing toDateTime(Timestamp) field:\n%s", logs[0])
	}
	if strings.Contains(logs[0], "TimestampTime") {
		t.Errorf("logs TTL: TimestampTime column no longer exists upstream:\n%s", logs[0])
	}
	metrics, _ := renderSignal(cfg, Metrics)
	for i, stmt := range metrics {
		if !strings.Contains(stmt, "TTL toDateTime(TimeUnix)") {
			t.Errorf("metrics[%d] TTL: missing toDateTime(TimeUnix):\n%s", i, stmt)
		}
	}
	traces, _ := renderSignal(cfg, Traces)
	if !strings.Contains(traces[0], "TTL toDateTime(Timestamp)") {
		t.Errorf("traces[0] TTL: missing toDateTime(Timestamp):\n%s", traces[0])
	}
	if !strings.Contains(traces[1], "TTL toDateTime(Start)") {
		t.Errorf("traces[1] TTL: missing toDateTime(Start):\n%s", traces[1])
	}
}

// TestRenderSignal_ClusterClauseRenderedAndQuoted documents the
// quoting contract: cluster names land in an ON CLUSTER clause with the
// name backtick-quoted (embedded backticks doubled), matching upstream's
// Config.clusterString, so special characters (rare in practice, but
// possible) don't escape the template.
func TestRenderSignal_ClusterClauseRenderedAndQuoted(t *testing.T) {
	cfg := Config{Cluster: "ch-prod"}.withDefaults()
	for _, sig := range All {
		stmts, _ := renderSignal(cfg, sig)
		for i, stmt := range stmts {
			if !strings.Contains(stmt, "ON CLUSTER `ch-prod`") {
				t.Errorf("%s[%d]: cluster clause missing or unquoted:\n%s", sig, i, stmt)
			}
		}
	}
}

// TestApply_Idempotent_NoOpWhenSignalsEmpty: invoking Apply with an
// empty signal slice is a no-op that returns nil. Mirrors the contract
// the cmd/cerberus wiring depends on when the caller's signal selector
// resolves to nothing.
func TestApply_Idempotent_NoOpWhenSignalsEmpty(t *testing.T) {
	// Apply iterates signals and only calls conn.Exec inside the loop;
	// with no signals, conn must never be touched, so a nil conn is
	// safe.
	if err := Apply(context.TODO(), nil, nil); err != nil {
		t.Errorf("Apply with no signals: %v; want nil (no-op)", err)
	}
	if err := ApplyWithConfig(context.TODO(), nil, Config{}, []Signal{}); err != nil {
		t.Errorf("ApplyWithConfig with no signals: %v; want nil (no-op)", err)
	}
}

// TestSignalString_StableValuesForDashboards pins the human-readable
// signal names. Cerberus surfaces these in startup logs and error
// messages — a rename here would silently move dashboards.
func TestSignalString_StableValuesForDashboards(t *testing.T) {
	got := []string{Metrics.String(), Logs.String(), Traces.String()}
	want := []string{"metrics", "logs", "traces"}
	for i, s := range got {
		if s != want[i] {
			t.Errorf("Signal[%d].String() = %q; want %q", i, s, want[i])
		}
	}
}

// TestRenderSignal_DefaultEngineMergeTree documents the cerberus default
// (single-node MergeTree). Operators running ReplicatedMergeTree will
// override but we pin the default so cerberus stays "drop in" for a
// fresh CH single-node deploy.
func TestRenderSignal_DefaultEngineMergeTree(t *testing.T) {
	cfg := Config{}.withDefaults()
	for _, sig := range All {
		stmts, _ := renderSignal(cfg, sig)
		// Only table CREATEs care about the engine; the MV statement
		// for traces does not include an ENGINE = clause. Skip lines
		// without the substring to avoid false positives.
		for i, stmt := range stmts {
			if !strings.Contains(stmt, "ENGINE = ") {
				continue
			}
			if !strings.Contains(stmt, "MergeTree()") {
				t.Errorf("%s[%d]: default engine missing:\n%s", sig, i, stmt)
			}
		}
	}
}
