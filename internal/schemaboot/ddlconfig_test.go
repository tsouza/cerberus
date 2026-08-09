package schemaboot_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	"github.com/tsouza/cerberus/internal/schemaboot"
)

// TestDDLConfig_PerSignalTTLFallback pins the per-signal TTL resolution: a
// non-zero per-signal override wins, and a zero per-signal value inherits the
// global CERBERUS_SCHEMA_TTL.
func TestDDLConfig_PerSignalTTLFallback(t *testing.T) {
	cfg := config.Config{
		SchemaProvisioning: config.SchemaProvisioning{
			TTL:        90 * 24 * time.Hour, // global default
			TTLLogs:    7 * 24 * time.Hour,  // logs override
			TTLTraces:  14 * 24 * time.Hour, // traces override
			TTLMetrics: 0,                   // metrics inherit the global
		},
	}
	got, err := schemaboot.DDLConfig(cfg)
	if err != nil {
		t.Fatalf("DDLConfig: %v", err)
	}
	if got.TTL.Metrics != 90*24*time.Hour {
		t.Errorf("metrics TTL = %v; want global 90d (inherited)", got.TTL.Metrics)
	}
	if got.TTL.Logs != 7*24*time.Hour {
		t.Errorf("logs TTL = %v; want 7d (override)", got.TTL.Logs)
	}
	if got.TTL.Traces != 14*24*time.Hour {
		t.Errorf("traces TTL = %v; want 14d (override)", got.TTL.Traces)
	}
}

// TestDDLConfig_TableNamesThreaded pins that auto-create uses the SAME resolved
// table names the query heads read (cfg.Schema / Logs / Traces), so a
// CERBERUS_SCHEMA_*_TABLE override creates and queries the same table rather
// than silently diverging onto the upstream defaults.
func TestDDLConfig_TableNamesThreaded(t *testing.T) {
	cfg := config.Config{
		Schema: schema.Metrics{
			GaugeTable:        "m_gauge",
			SumTable:          "m_sum",
			HistogramTable:    "m_hist",
			ExpHistogramTable: "m_exp",
			SummaryTable:      "m_summary",
		},
		Logs:   schema.Logs{LogsTable: "my_logs"},
		Traces: schema.Traces{SpansTable: "my_spans"},
	}
	got, err := schemaboot.DDLConfig(cfg)
	if err != nil {
		t.Fatalf("DDLConfig: %v", err)
	}
	if got.Tables.MetricsGauge != "m_gauge" || got.Tables.MetricsSum != "m_sum" ||
		got.Tables.MetricsHistogram != "m_hist" || got.Tables.MetricsExpHistogram != "m_exp" ||
		got.Tables.MetricsSummary != "m_summary" {
		t.Errorf("metrics table names not threaded: %+v", got.Tables)
	}
	if got.Tables.Logs != "my_logs" {
		t.Errorf("logs table = %q; want my_logs", got.Tables.Logs)
	}
	if got.Tables.Traces != "my_spans" {
		t.Errorf("traces table = %q; want my_spans", got.Tables.Traces)
	}
}

// TestDDLConfig_ReplicatedThreaded pins the Replicated database engine knobs
// flow through to the ddl Config.
func TestDDLConfig_ReplicatedThreaded(t *testing.T) {
	cfg := config.Config{
		SchemaProvisioning: config.SchemaProvisioning{
			Cluster:                   "", // mutually exclusive with replicated
			DatabaseReplicated:        true,
			DatabaseReplicatedPath:    "/clickhouse/databases/otel",
			DatabaseReplicatedShard:   "shard0",
			DatabaseReplicatedReplica: "replica0",
		},
	}
	got, err := schemaboot.DDLConfig(cfg)
	if err != nil {
		t.Fatalf("DDLConfig: %v", err)
	}
	if !got.DatabaseEngine.Replicated ||
		got.DatabaseEngine.ReplicatedZooPath != "/clickhouse/databases/otel" ||
		got.DatabaseEngine.ReplicatedShard != "shard0" ||
		got.DatabaseEngine.ReplicatedReplica != "replica0" {
		t.Errorf("replicated engine not threaded: %+v", got.DatabaseEngine)
	}
}

// TestDDLConfig_StoragePolicyPinnedFirst pins the StoragePolicy shorthand: it
// folds into Settings ahead of the generic settings list, so `storage_policy`
// always precedes any CERBERUS_SCHEMA_SETTINGS entries deterministically.
func TestDDLConfig_StoragePolicyPinnedFirst(t *testing.T) {
	cfg := config.Config{
		SchemaProvisioning: config.SchemaProvisioning{
			StoragePolicy: "s3_tiered",
			Settings: []schema.KV{
				{Key: "min_bytes_for_wide_part", Value: int64(0)},
			},
		},
	}
	got, err := schemaboot.DDLConfig(cfg)
	if err != nil {
		t.Fatalf("DDLConfig: %v", err)
	}
	if len(got.Settings) != 2 {
		t.Fatalf("want 2 settings, got %d: %+v", len(got.Settings), got.Settings)
	}
	if got.Settings[0].Key != "storage_policy" || got.Settings[0].Value != "s3_tiered" {
		t.Errorf("storage_policy not pinned first: %+v", got.Settings)
	}
	if got.Settings[1].Key != "min_bytes_for_wide_part" {
		t.Errorf("generic setting not preserved after storage_policy: %+v", got.Settings)
	}
}

// TestDDLConfig_PerSignalTieringFallback pins that the tiering ages resolve
// exactly like retention does — a non-zero per-signal override wins, a zero one
// inherits the global CERBERUS_SCHEMA_TIER_AFTER — and that the destination
// volume threads through to the ddl config that emits the TO VOLUME clause.
func TestDDLConfig_PerSignalTieringFallback(t *testing.T) {
	const day = 24 * time.Hour
	cfg := config.Config{
		SchemaProvisioning: config.SchemaProvisioning{
			TTL:              90 * day,
			StoragePolicy:    "s3_tiered",
			TierVolume:       "cold",
			TierAfter:        30 * day, // global default
			TierAfterLogs:    3 * day,  // logs override
			TierAfterTraces:  7 * day,  // traces override
			TierAfterMetrics: 0,        // metrics inherit the global
		},
	}
	got, err := schemaboot.DDLConfig(cfg)
	if err != nil {
		t.Fatalf("DDLConfig: %v", err)
	}
	if got.Tiering.Volume != "cold" {
		t.Errorf("tiering volume = %q; want cold", got.Tiering.Volume)
	}
	if got.Tiering.Metrics != 30*day {
		t.Errorf("metrics tiering age = %v; want global 30d (inherited)", got.Tiering.Metrics)
	}
	if got.Tiering.Logs != 3*day {
		t.Errorf("logs tiering age = %v; want 3d (override)", got.Tiering.Logs)
	}
	if got.Tiering.Traces != 7*day {
		t.Errorf("traces tiering age = %v; want 7d (override)", got.Tiering.Traces)
	}
}

// TestDDLConfig_InertTieringRejected pins that an accepted-but-inert tiering
// configuration fails at boot rather than being taken and silently doing
// nothing. DDLConfig runs on EVERY boot, so this is where an operator finds
// out — not on the storage bill months later.
func TestDDLConfig_InertTieringRejected(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		name string
		p    config.SchemaProvisioning
	}{
		{
			name: "age_without_volume",
			p:    config.SchemaProvisioning{TTL: 30 * day, TierAfter: 3 * day},
		},
		{
			name: "volume_without_age",
			p:    config.SchemaProvisioning{TTL: 30 * day, TierVolume: "cold"},
		},
		{
			name: "move_not_shorter_than_retention",
			p:    config.SchemaProvisioning{TTL: 30 * day, TierVolume: "cold", TierAfter: 30 * day},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := schemaboot.DDLConfig(config.Config{SchemaProvisioning: tc.p}); err == nil {
				t.Fatal("want a rejection for the inert tiering config, got nil")
			}
		})
	}
}

// TestDDLConfig_NoTieringIsValid pins that the default (no tiering) config —
// what every existing deployment carries — still maps cleanly.
func TestDDLConfig_NoTieringIsValid(t *testing.T) {
	cfg := config.Config{
		SchemaProvisioning: config.SchemaProvisioning{TTL: 30 * 24 * time.Hour},
	}
	got, err := schemaboot.DDLConfig(cfg)
	if err != nil {
		t.Fatalf("DDLConfig: %v", err)
	}
	if got.Tiering != (ddl.Tiering{}) {
		t.Errorf("no tiering configured, want the zero Tiering, got %+v", got.Tiering)
	}
}

// TestDDLConfig_StoragePolicyDualSourceRejected pins the fail-fast: setting
// storage_policy via BOTH the shorthand and a Settings key is an error — there
// is exactly one way to set it.
func TestDDLConfig_StoragePolicyDualSourceRejected(t *testing.T) {
	cfg := config.Config{
		SchemaProvisioning: config.SchemaProvisioning{
			StoragePolicy: "s3_tiered",
			Settings: []schema.KV{
				{Key: "storage_policy", Value: "other"},
			},
		},
	}
	if _, err := schemaboot.DDLConfig(cfg); err == nil {
		t.Fatal("want error for storage_policy set via both shorthand and Settings, got nil")
	}
}

// TestDDLConfig_SettingsOnlyNoStoragePolicy pins that a bare Settings list (no
// shorthand) threads through unchanged — no spurious storage_policy.
func TestDDLConfig_SettingsOnlyNoStoragePolicy(t *testing.T) {
	cfg := config.Config{
		SchemaProvisioning: config.SchemaProvisioning{
			Settings: []schema.KV{
				{Key: "min_bytes_for_wide_part", Value: int64(0)},
			},
		},
	}
	got, err := schemaboot.DDLConfig(cfg)
	if err != nil {
		t.Fatalf("DDLConfig: %v", err)
	}
	if len(got.Settings) != 1 || got.Settings[0].Key != "min_bytes_for_wide_part" {
		t.Errorf("settings not threaded as-is: %+v", got.Settings)
	}
}
