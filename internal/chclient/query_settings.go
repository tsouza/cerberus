package chclient

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// perQuerySettingsKeyType keys the per-request ClickHouse settings map on a
// context. It is the generalised successor to the single-flag tsGridSettingKey:
// instead of one bool meaning "add SettingExperimentalTSGridAggregate=1", the
// engine can stamp ANY (name, value) the post-optimize plan shape calls for,
// and querySettings merges the whole map into the per-query settings.
type perQuerySettingsKeyType struct{}

var perQuerySettingsKey = perQuerySettingsKeyType{}

// WithQuerySetting returns a ctx carrying name=value in the per-request
// ClickHouse settings map, on top of any settings already attached to ctx.
// It is the single reusable hook the engine uses to attach plan-shape-gated
// settings (the experimental ts-grid gate, optimize_aggregation_in_order, ...)
// to exactly the query they apply to — never globally, so a setting that
// would error on an unrelated query (or an older ClickHouse) never rides one.
//
// The carrier is a context value, not a Client field, so it is per-request:
// two concurrent requests with different plan shapes cannot cross-contaminate
// each other's settings. Each call copies the existing map (copy-on-write) so
// a derived ctx never mutates a parent ctx's map.
func WithQuerySetting(ctx context.Context, name string, value any) context.Context {
	prev := querySettingsFromContext(ctx)
	next := make(clickhouse.Settings, len(prev)+1)
	for k, v := range prev {
		next[k] = v
	}
	next[name] = value
	return context.WithValue(ctx, perQuerySettingsKey, next)
}

// querySettingsFromContext returns the per-request settings map attached to
// ctx by WithQuerySetting, or nil when none was attached. The returned map
// must be treated as read-only by callers (it may be shared with derived
// contexts); WithQuerySetting copies before writing.
func querySettingsFromContext(ctx context.Context) clickhouse.Settings {
	s, _ := ctx.Value(perQuerySettingsKey).(clickhouse.Settings)
	return s
}

// QuerySettingsFromContext returns a COPY of the per-request ClickHouse
// settings attached to ctx by WithQuerySetting, or nil when none was
// attached. It is the public read side of the carrier: callers in other
// packages (the engine's rule code, tests asserting a plan-shape rule
// stamped the expected setting) can inspect what will ride a query without
// reaching into chclient's private context key. The copy keeps the internal
// map immutable from the caller's side.
func QuerySettingsFromContext(ctx context.Context) clickhouse.Settings {
	s := querySettingsFromContext(ctx)
	if s == nil {
		return nil
	}
	out := make(clickhouse.Settings, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}
