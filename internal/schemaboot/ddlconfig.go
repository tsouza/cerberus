// Package schemaboot maps cerberus's runtime config into the typed
// internal/schema/ddl Config used to create ClickHouse tables. It is the single
// place that translation lives, shared by the server's auto-create startup hook
// (cmd/cerberus) and the offline migration preview tool (cmd/migrate), so the
// schema the tool previews is byte-identical to the schema the server applies.
package schemaboot

import (
	"fmt"
	"time"

	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// storagePolicySetting is the MergeTree setting key the StoragePolicy shorthand
// folds into the SETTINGS tail. Pinned first so the emitted DDL is
// deterministic regardless of any further Settings entries.
const storagePolicySetting = "storage_policy"

// DDLConfig maps the runtime config into the typed internal/schema/ddl Config
// the auto-create hook applies. The database name comes from the ClickHouse
// connection config; the cluster / table-engine / TTL / Replicated
// database-engine knobs come from CERBERUS_SCHEMA_* (SchemaProvisioning); and
// the per-signal TABLE NAMES are threaded from the SAME resolved schema structs
// the query heads read (cfg.Schema / cfg.Logs / cfg.Traces), so a
// CERBERUS_SCHEMA_*_TABLE override creates and queries the same table instead of
// silently diverging.
func DDLConfig(cfg config.Config) (ddl.Config, error) {
	p := cfg.SchemaProvisioning
	// Per-signal TTL: a non-zero per-signal override wins; otherwise the signal
	// inherits the global CERBERUS_SCHEMA_TTL default (which is itself 0 = no
	// retention unless the operator sets it).
	signalTTL := func(override time.Duration) time.Duration {
		if override > 0 {
			return override
		}
		return p.TTL
	}
	settings, err := schemaSettings(p)
	if err != nil {
		return ddl.Config{}, err
	}
	return ddl.Config{
		Database: cfg.ClickHouse.Database,
		Cluster:  p.Cluster,
		Engine:   p.TableEngine,
		TTL: ddl.TTL{
			Metrics: signalTTL(p.TTLMetrics),
			Logs:    signalTTL(p.TTLLogs),
			Traces:  signalTTL(p.TTLTraces),
		},
		DatabaseEngine: ddl.DatabaseEngine{
			Replicated:        p.DatabaseReplicated,
			ReplicatedZooPath: p.DatabaseReplicatedPath,
			ReplicatedShard:   p.DatabaseReplicatedShard,
			ReplicatedReplica: p.DatabaseReplicatedReplica,
		},
		Tables: ddl.Tables{
			Logs:                cfg.Logs.LogsTable,
			Traces:              cfg.Traces.SpansTable,
			MetricsGauge:        cfg.Schema.GaugeTable,
			MetricsSum:          cfg.Schema.SumTable,
			MetricsHistogram:    cfg.Schema.HistogramTable,
			MetricsExpHistogram: cfg.Schema.ExpHistogramTable,
			MetricsSummary:      cfg.Schema.SummaryTable,
		},
		Settings: settings,
	}, nil
}

// schemaSettings resolves the auto-create-table SETTINGS tail from the
// provisioning config: the StoragePolicy shorthand (when set) is folded in
// PINNED FIRST, ahead of the generic Settings list, so `storage_policy` always
// precedes the long-tail settings deterministically. Setting StoragePolicy AND
// also carrying a `storage_policy` key in Settings is a fail-fast error — there
// is exactly one way to set it.
func schemaSettings(p config.SchemaProvisioning) ([]schema.KV, error) {
	if p.StoragePolicy == "" {
		return p.Settings, nil
	}
	for _, kv := range p.Settings {
		if kv.Key == storagePolicySetting {
			return nil, fmt.Errorf(
				"schema: storage_policy set via both CERBERUS_SCHEMA_STORAGE_POLICY and CERBERUS_SCHEMA_SETTINGS — set it in exactly one",
			)
		}
	}
	out := make([]schema.KV, 0, len(p.Settings)+1)
	out = append(out, schema.KV{Key: storagePolicySetting, Value: p.StoragePolicy})
	out = append(out, p.Settings...)
	return out, nil
}
