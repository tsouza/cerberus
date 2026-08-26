// Package schemaboot maps cerberus's runtime config into the typed
// internal/schema/ddl Config used to create ClickHouse tables. It is the single
// place that translation lives, shared by the server's auto-create startup hook
// (cmd/cerberus) and the offline migration preview tool (cmd/cerberus), so the
// schema the tool previews is byte-identical to the schema the server applies.
package schemaboot

import (
	"fmt"
	"time"

	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// resolveSignalDuration resolves a per-signal duration knob: a non-zero
// per-signal override wins, otherwise the signal inherits the global default
// (which is itself 0 = the feature is off unless the operator sets it). It
// serves both retention (CERBERUS_SCHEMA_TTL{,_METRICS,_LOGS,_TRACES}) and
// storage tiering (CERBERUS_SCHEMA_TIER_AFTER{,_METRICS,_LOGS,_TRACES}), which
// carry the same global-plus-override shape.
func resolveSignalDuration(global, override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return global
}

// storagePolicySetting is the MergeTree setting key the StoragePolicy shorthand
// folds into the SETTINGS tail. Pinned first so the emitted DDL is
// deterministic regardless of any further Settings entries.
const storagePolicySetting = "storage_policy"

// MetricsRetention resolves the effective metrics-table retention TTL from
// cfg's SchemaProvisioning knobs — CERBERUS_SCHEMA_TTL_METRICS overrides
// CERBERUS_SCHEMA_TTL, the same inherit-or-override rule DDLConfig applies
// to every per-signal TTL/tiering knob via resolveSignalDuration. Exported
// so an operational tool that needs ONLY the effective metrics retention —
// not the full auto-create DDL shape DDLConfig builds (which also demands
// Validate() pass for tiering knobs unrelated to the caller's concern) —
// doesn't have to duplicate the inherit rule: cmd/cerberus's
// delta-prefix-backfill / delta-prefix-verify verbs use this to detect
// when a backfilled day is already outside the DeltaPrefixTable's own TTL
// as of the check's run time, a structurally unrecoverable state distinct
// from a real completeness gap (cerberus issue #2652).
func MetricsRetention(cfg config.Config) time.Duration {
	return resolveSignalDuration(cfg.SchemaProvisioning.TTL, cfg.SchemaProvisioning.TTLMetrics)
}

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
	signalTTL := func(override time.Duration) time.Duration {
		return resolveSignalDuration(p.TTL, override)
	}
	tierAfter := func(override time.Duration) time.Duration {
		return resolveSignalDuration(p.TierAfter, override)
	}
	settings, err := schemaSettings(p)
	if err != nil {
		return ddl.Config{}, err
	}
	out := ddl.Config{
		Database: cfg.ClickHouse.Database,
		Cluster:  p.Cluster,
		Engine:   p.TableEngine,
		TTL: ddl.TTL{
			Metrics: MetricsRetention(cfg),
			Logs:    signalTTL(p.TTLLogs),
			Traces:  signalTTL(p.TTLTraces),
		},
		Tiering: ddl.Tiering{
			Volume:  p.TierVolume,
			Metrics: tierAfter(p.TierAfterMetrics),
			Logs:    tierAfter(p.TierAfterLogs),
			Traces:  tierAfter(p.TierAfterTraces),
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
			MetricsDeltaPrefix:  cfg.Schema.DeltaPrefixTable,
		},
		Settings: settings,
		// DeltaPrefixEnabled (cerberus issue #2389) is a second, independent
		// gate on top of AutoCreateSchema — see SchemaProvisioning's doc
		// comment. The table + column names come from cfg.Schema (the SAME
		// resolved schema struct every other table name above is threaded
		// from), the enable bit from SchemaProvisioning (the SAME struct
		// every other DDL-shaping knob on this Config comes from).
		DeltaPrefixEnabled:      p.DeltaPrefixEnabled,
		DeltaPrefixBucketColumn: cfg.Schema.DeltaPrefixBucketColumn,
		DeltaPrefixSumColumn:    cfg.Schema.DeltaPrefixSumColumn,
	}
	// Validate here rather than only inside ddl.ApplyWithConfig: DDLConfig runs
	// on EVERY boot (the auto-create hook is a separate flag), so an inert
	// tiering combination — an age with no volume, a volume with no age, a move
	// that never beats the delete-TTL — fails fast with a precise message
	// instead of being accepted and silently doing nothing.
	if err := out.Validate(); err != nil {
		return ddl.Config{}, err
	}
	return out, nil
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
