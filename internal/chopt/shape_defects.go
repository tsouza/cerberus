package chopt

import "slices"

// ShapeDefect is a ClickHouse wrong-result defect that only a recognisable
// query shape triggers, on the builds its ranges hold. Unlike a Feature's
// UnsafeBuilds, it does not withhold any feature: on an affected build the
// engine stamps the setting that switches the defective mechanism off on the
// queries of that shape alone, and every other query keeps the mechanism.
// internal/engine owns the shape predicate and the setting; this table owns
// the builds.
type ShapeDefect struct {
	ID     string
	Builds []BuildRange
	// Setting is the setting the gate stamps (a GateSettings entry).
	Setting string
}

// GateSetting is a setting cerberus stamps to switch a defective ClickHouse
// mechanism off, with the first release that accepts it. A server below Since
// does not ignore the setting: it rejects the whole query with UNKNOWN_SETTING
// (code 115). Such a server also predates every defect the setting avoids
// (TestShapeDefects_RangesStartAtOrAboveTheirSetting), so on a fleet mixing
// it with an affected node it never answers wrong; it answers that error.
type GateSetting struct {
	Name  string
	Since Version
	// Defects names the ClickHouse wrong-result defects the setting is
	// stamped against, for the error a server below Since answers.
	Defects []string
}

// Gate setting names, the ClickHouse settings themselves.
const (
	SettingUseQueryConditionCache         = "use_query_condition_cache"
	SettingOptimizeUseProjectionFiltering = "optimize_use_projection_filtering"
)

// gateSettings is the table, verified against real servers by test/chserver's
// setting-floor probe.
var gateSettings = []GateSetting{
	{
		Name:    SettingUseQueryConditionCache,
		Since:   Version{Major: 25, Minor: 3},
		Defects: []string{"ClickHouse#105686", "ClickHouse#107145", "ClickHouse#119168"},
	},
	{
		Name:    SettingOptimizeUseProjectionFiltering,
		Since:   Version{Major: 25, Minor: 6},
		Defects: []string{"ClickHouse#120286", "the 25.11 projection-index read"},
	},
}

// GateSettings returns a copy of the gate-setting table.
func GateSettings() []GateSetting {
	out := make([]GateSetting, len(gateSettings))
	for i, g := range gateSettings {
		g.Defects = slices.Clone(g.Defects)
		out[i] = g
	}
	return out
}

// GateSettingByName returns the gate setting called name.
func GateSettingByName(name string) (GateSetting, bool) {
	for _, g := range GateSettings() {
		if g.Name == name {
			return g, true
		}
	}
	return GateSetting{}, false
}

// Shape defect ids, read by internal/choptwire to wire internal/engine's
// per-query gates.
const (
	// ShapeDefectConversionConditionCache is ClickHouse#119168: a conversion
	// function captures the conversion settings of the session that built it
	// (precise_float_parsing among them), but the query condition cache keyed
	// its filter without them, so a session that parses a string to a float
	// differently shares the cached granule verdict. The shape is a WHERE /
	// PREWHERE predicate that parses a String to a float. Present since the
	// cache shipped in 25.3 on every line through 26.8, the 26.3 and 26.8 LTS
	// lines included; no 26.9 release reproduces it, and the upstream fix
	// (26.10) was not backported.
	ShapeDefectConversionConditionCache = "conversion_condition_cache"

	// ShapeDefectNegatedConjunctionProjection is ClickHouse#120286: the
	// projection index's filter analysis weakened a conjunct it cannot compute
	// from the projection to true even under a negation, so `NOT (a AND b)`
	// with b outside the projection became `NOT a` (or `NOT true`) and the
	// projection filtered out rows the predicate keeps. The shape is a
	// TraceId-keyed read whose filter negates a conjunction with a conjunct
	// reading a column other than TraceId. Every 25.11 through 26.9 release
	// is affected; the fix merged for 26.10 and was not backported.
	ShapeDefectNegatedConjunctionProjection = "negated_conjunction_projection"

	// ShapeDefectTraceIDProjectionRead is the 25.11 line's projection-index
	// read losing rows outright: a TraceId-keyed read filtered through
	// proj_trace_id returns a subset of the matching rows whenever it reads
	// any column the projection does not store (10 of 60 spans; a bare
	// count() is correct), with one part or several and with one thread or
	// many. 25.12.11.4 no longer does it. Every TraceId-keyed read cerberus
	// emits reads such a column, so the shape is every TraceId-keyed read.
	ShapeDefectTraceIDProjectionRead = "trace_id_projection_read"
)

// shapeDefects is the seeded table. Each Until is the first published release
// the cerberus-shaped reproduction came back clean on, or the first release of
// the next line when no backport was released. test/chserver's
// condition-cache and projection-filtering tests pin the boundaries; the
// per-build surveys are in docs/clickhouse-optimizations.background.md.
var shapeDefects = []ShapeDefect{
	{
		ID:      ShapeDefectConversionConditionCache,
		Setting: SettingUseQueryConditionCache,
		Builds: []BuildRange{{
			From: Version{Major: 25, Minor: 3}, Until: Version{Major: 26, Minor: 9},
			Defect: "ClickHouse#119168: conversion-function verdicts shared across sessions with different conversion settings",
		}},
	},
	{
		ID:      ShapeDefectNegatedConjunctionProjection,
		Setting: SettingOptimizeUseProjectionFiltering,
		Builds: []BuildRange{{
			From: Version{Major: 25, Minor: 11}, Until: Version{Major: 26, Minor: 10},
			Defect: "ClickHouse#120286: projection index filters rows a negated conjunction keeps",
		}},
	},
	{
		ID:      ShapeDefectTraceIDProjectionRead,
		Setting: SettingOptimizeUseProjectionFiltering,
		Builds: []BuildRange{{
			From: Version{Major: 25, Minor: 11}, Until: Version{Major: 25, Minor: 12, Patch: 11, Build: 4},
			Defect: "25.11 projection-index read drops rows of a TraceId lookup that reads a column outside the projection",
		}},
	},
}

// ShapeDefects returns a copy of the shape-defect table.
func ShapeDefects() []ShapeDefect {
	out := make([]ShapeDefect, len(shapeDefects))
	for i, d := range shapeDefects {
		d.Builds = slices.Clone(d.Builds)
		out[i] = d
	}
	return out
}

// KnownShapeDefect reports whether server falls inside one of shape defect
// id's ranges.
func KnownShapeDefect(id string, server Version) bool {
	for _, d := range shapeDefects {
		if d.ID != id {
			continue
		}
		for _, r := range d.Builds {
			if r.Contains(server) {
				return true
			}
		}
	}
	return false
}

// shapeDefectsOf returns the ids of every shape defect server carries.
func shapeDefectsOf(server Version) map[string]struct{} {
	out := map[string]struct{}{}
	for _, d := range shapeDefects {
		if KnownShapeDefect(d.ID, server) {
			out[d.ID] = struct{}{}
		}
	}
	return out
}
