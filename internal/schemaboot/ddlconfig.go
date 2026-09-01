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

// mapSerializationVersionSetting / mapSerializationVersionWithBuckets are the
// ClickHouse MergeTree setting chopt's map_bucketed_serialization feature
// (cerberus issue #2774) stamps onto new logs/traces tables — see
// internal/chopt.FeatureMapBucketedSerialization's doc comment for the full
// mechanism and the reasoning behind scoping it to those two signals only.
const (
	mapSerializationVersionSetting     = "map_serialization_version"
	mapSerializationVersionWithBuckets = "with_buckets"
)

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
	logsSettings, tracesSettings, err := mapBucketedSettings(cfg.SchemaMapBucketedSerialization, settings)
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
		// ColumnTTL (cerberus issue #2769) threads straight from
		// SchemaProvisioning's own LogsBodyTTL / TracesEventsLinksTTL —
		// unlike TTL/Tiering above, these carry no global-plus-override
		// inherit rule (there is exactly one column each targets, so
		// there is nothing to inherit from): a zero value is simply "no
		// column TTL", the same posture DeltaPrefixEnabled's plain
		// config-bool gate uses rather than a chopt verdict, because this
		// capability (like DeltaPrefixEnabled's) has no ClickHouse
		// version floor to probe.
		ColumnTTL: ddl.ColumnTTL{
			LogsBody:          p.LogsBodyTTL,
			TracesEventsLinks: p.TracesEventsLinksTTL,
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
		Settings:       settings,
		LogsSettings:   logsSettings,
		TracesSettings: tracesSettings,
		// DeltaPrefixEnabled (cerberus issue #2389) is a second, independent
		// gate on top of AutoCreateSchema — see SchemaProvisioning's doc
		// comment. The table + column names come from cfg.Schema (the SAME
		// resolved schema struct every other table name above is threaded
		// from), the enable bit from SchemaProvisioning (the SAME struct
		// every other DDL-shaping knob on this Config comes from).
		DeltaPrefixEnabled:      p.DeltaPrefixEnabled,
		DeltaPrefixBucketColumn: cfg.Schema.DeltaPrefixBucketColumn,
		DeltaPrefixSumColumn:    cfg.Schema.DeltaPrefixSumColumn,
		// ColumnStatisticsEnabled (cerberus issue #2766) is the resolved
		// chopt column_statistics verdict, back-filled by cmd/cerberus's boot
		// resolver (or, for the offline `migrate schema` preview,
		// chopt.ExplicitlyRequested) — the same threading
		// SchemaMapBucketedSerialization above uses for the logs/traces
		// SETTINGS tail.
		ColumnStatisticsEnabled: cfg.SchemaColumnStatistics,
		// TraceIDProjectionEnabled (cerberus issue #2767) is the resolved
		// chopt trace_id_projection verdict, back-filled by cmd/cerberus's
		// boot resolver (or, for the offline `migrate schema` preview,
		// chopt.ExplicitlyRequested) — the SAME threading
		// ColumnStatisticsEnabled above uses for its own chopt verdict.
		TraceIDProjectionEnabled: cfg.SchemaTraceIDProjection,
		// TextIndexEnabled (cerberus issue #2773) is the resolved chopt
		// full_text_index verdict, back-filled by cmd/cerberus's boot
		// resolver (or, for the offline `migrate schema` preview,
		// chopt.ExplicitlyRequested) — the SAME threading
		// TraceIDProjectionEnabled above uses for its own chopt verdict.
		TextIndexEnabled: cfg.SchemaFullTextIndex,
		// LokiLabelCatalogEnabled (cerberus issue #2770) is the resolved
		// chopt loki_catalog_mv verdict, back-filled by cmd/cerberus's boot
		// resolver (or, for the offline `migrate schema` preview,
		// chopt.ExplicitlyRequested) — the SAME threading
		// TraceIDProjectionEnabled above uses for its own chopt verdict.
		LokiLabelCatalogEnabled: cfg.SchemaLokiCatalogMV,
		// TempoTagCatalogEnabled (cerberus issue #2771) is the resolved
		// chopt tempo_tag_catalog_mv verdict, back-filled by cmd/cerberus's
		// boot resolver (or, for the offline `migrate schema` preview,
		// chopt.ExplicitlyRequested) — the SAME threading
		// LokiLabelCatalogEnabled above uses for its own chopt verdict.
		TempoTagCatalogEnabled: cfg.SchemaTempoTagCatalogMV,
		// DownsampleTierEnabled (cerberus issue #2751) is the resolved chopt
		// downsample_tier verdict, back-filled by cmd/cerberus's boot
		// resolver (or, for the offline `migrate schema` preview,
		// chopt.ExplicitlyRequested) — the SAME threading
		// TraceIDProjectionEnabled above uses for its own chopt verdict. The
		// table/column names are the fixed schema.DownsampleTier* constants
		// (see that package's doc for why, unlike DeltaPrefixTable, they are
		// not threaded from cfg.Schema at all).
		DownsampleTierEnabled: cfg.SchemaDownsampleTier,
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

// mapBucketedSettings resolves the ddl.Config.LogsSettings / TracesSettings
// chopt's map_bucketed_serialization feature (cerberus issue #2774) drives.
// enabled is cfg.SchemaMapBucketedSerialization — the resolved verdict
// back-filled by cmd/cerberus's boot resolver (or, for the offline `migrate
// schema` preview, chopt.ExplicitlyRequested). false returns (nil, nil): the
// generic Settings tail is untouched and every table's DDL stays
// byte-identical to today, the same backward-compat contract every other
// Config field defaults to.
//
// resolvedSettings is schemaSettings' already-resolved generic Settings
// slice (the only way to hand-set map_serialization_version today is
// CERBERUS_SCHEMA_SETTINGS — there is no per-key shorthand the way
// StoragePolicy is for storage_policy), checked here purely because it is
// the slice ddl.Config.Settings actually ends up carrying. Both the generic
// tail and this feature's tail land on the SAME logs/traces SETTINGS clause
// (ddl.appendSettings concatenates Settings then the per-table extra — see
// its doc comment), and ClickHouse rejects a SETTINGS clause that repeats a
// key, so a manually configured map_serialization_version alongside the
// feature is a fail-fast misconfiguration here rather than a
// DDL-execution-time error at boot — mirroring the storage_policy dedup
// guard immediately above.
func mapBucketedSettings(enabled bool, resolvedSettings []schema.KV) (logs, traces []schema.KV, err error) {
	if !enabled {
		return nil, nil, nil
	}
	for _, kv := range resolvedSettings {
		if kv.Key == mapSerializationVersionSetting {
			return nil, nil, fmt.Errorf(
				"schema: %s set via both CERBERUS_SCHEMA_SETTINGS and the map_bucketed_serialization ch_opt feature — set it in exactly one",
				mapSerializationVersionSetting,
			)
		}
	}
	kv := []schema.KV{{Key: mapSerializationVersionSetting, Value: mapSerializationVersionWithBuckets}}
	return kv, kv, nil
}
