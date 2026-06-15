package main

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestBootstrapClickHouseConfig pins that the bootstrap connection rebinds to
// ClickHouse's always-present `default` database (so CREATE DATABASE works
// even when the configured target database doesn't exist yet) while leaving
// the rest of the connection config untouched.
func TestBootstrapClickHouseConfig(t *testing.T) {
	in := chclient.Config{Addr: "ch:9000", Database: "otel", Username: "u", Password: "p"}
	got := bootstrapClickHouseConfig(in)
	if got.Database != "default" {
		t.Errorf("bootstrap Database = %q; want default", got.Database)
	}
	if got.Addr != "ch:9000" || got.Username != "u" || got.Password != "p" {
		t.Errorf("bootstrap config changed non-database fields: %+v", got)
	}
	if in.Database != "otel" {
		t.Errorf("input config mutated: %+v", in)
	}
}

// TestSchemaApplyConfig_PerSignalTTLFallback pins the per-signal TTL
// resolution schemaApplyConfig performs: a non-zero per-signal override
// wins, and a zero per-signal value inherits the global CERBERUS_SCHEMA_TTL.
func TestSchemaApplyConfig_PerSignalTTLFallback(t *testing.T) {
	cfg := config.Config{
		SchemaProvisioning: config.SchemaProvisioning{
			TTL:        90 * 24 * time.Hour, // global default
			TTLLogs:    7 * 24 * time.Hour,  // logs override
			TTLTraces:  14 * 24 * time.Hour, // traces override
			TTLMetrics: 0,                   // metrics inherit the global
		},
	}
	got := schemaApplyConfig(cfg)
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

// TestSchemaApplyConfig_TableNamesThreaded pins that auto-create uses the
// SAME resolved table names the query heads read (cfg.Schema / Logs /
// Traces), so a CERBERUS_SCHEMA_*_TABLE override creates and queries the
// same table rather than silently diverging onto the upstream defaults.
func TestSchemaApplyConfig_TableNamesThreaded(t *testing.T) {
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
	got := schemaApplyConfig(cfg)
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

// TestSchemaApplyConfig_ReplicatedThreaded pins the Replicated database
// engine knobs flow through to the ddl Config.
func TestSchemaApplyConfig_ReplicatedThreaded(t *testing.T) {
	cfg := config.Config{
		SchemaProvisioning: config.SchemaProvisioning{
			Cluster:                   "", // mutually exclusive with replicated
			DatabaseReplicated:        true,
			DatabaseReplicatedPath:    "/clickhouse/databases/otel",
			DatabaseReplicatedShard:   "shard0",
			DatabaseReplicatedReplica: "replica0",
		},
	}
	got := schemaApplyConfig(cfg)
	if !got.DatabaseEngine.Replicated ||
		got.DatabaseEngine.ReplicatedZooPath != "/clickhouse/databases/otel" ||
		got.DatabaseEngine.ReplicatedShard != "shard0" ||
		got.DatabaseEngine.ReplicatedReplica != "replica0" {
		t.Errorf("replicated engine not threaded: %+v", got.DatabaseEngine)
	}
}

// TestSchemaApplyConfig_ReplicatedTablePathThreaded pins the ReplicatedMergeTree
// table-engine knobs flow through to the ddl Config so a Replicated database
// gets explicit ReplicatedMergeTree tables (a Replicated database does not
// auto-convert MergeTree).
func TestSchemaApplyConfig_ReplicatedTablePathThreaded(t *testing.T) {
	cfg := config.Config{
		SchemaProvisioning: config.SchemaProvisioning{
			DatabaseReplicated:     true,
			DatabaseReplicatedPath: "/clickhouse/databases/otel",
			TableReplicatedPath:    "/clickhouse/custom/{shard}/{table}",
			TableReplicatedReplica: "rep-{replica}",
		},
	}
	got := schemaApplyConfig(cfg)
	if got.ReplicatedTablePath != "/clickhouse/custom/{shard}/{table}" ||
		got.ReplicatedTableReplica != "rep-{replica}" {
		t.Errorf("table-replicated knobs not threaded: path=%q replica=%q",
			got.ReplicatedTablePath, got.ReplicatedTableReplica)
	}
}
