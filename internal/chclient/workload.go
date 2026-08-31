package chclient

import "context"

// SettingWorkload is the ClickHouse per-query setting that assigns a
// dispatched query to a named `WORKLOAD` for CPU-slot / IO-byte scheduling
// (`CREATE WORKLOAD` / `CREATE RESOURCE`, ClickHouse's server-side workload
// scheduling — see docs/operations.md#workload-scheduling). cerberus never
// creates or alters WORKLOAD/RESOURCE objects itself: they are server
// objects an operator provisions against a cluster cerberus, in the common
// bring-your-own-ClickHouse deployment, does not own. This setting only
// NAMES which already-provisioned workload cerberus's own query traffic
// should ride, so its own CPU/IO share can be bounded separately from the
// ingest/merge traffic sharing the same node.
//
// Unlike SettingUseQueryCache, ClickHouse does NOT reject an unknown
// workload NAME by default (`throw_on_unknown_workload=0`): a query tagged
// with a workload that does not exist runs with unlimited, unscheduled
// access, silently equivalent to not stamping the setting at all. What CAN
// be rejected is the setting NAME itself, on a server too old to know about
// workload scheduling, or a hardened profile that pins/forbids it — exactly
// what ProbeQueryWorkloadCapability's boot canary checks for.
const SettingWorkload = "workload"

// WithWorkloadSetting returns a ctx that signals the data-plane query
// methods to add SettingWorkload=name to the per-request ClickHouse settings
// map. It mirrors WithTSGridSetting / WithResultCacheSetting: one writer
// into the generalised WithQuerySetting carrier, so a query already carrying
// other plan-shape-gated settings picks up `workload` on the same map
// rather than one wrap clobbering another.
func WithWorkloadSetting(ctx context.Context, name string) context.Context {
	return WithQuerySetting(ctx, SettingWorkload, name)
}
