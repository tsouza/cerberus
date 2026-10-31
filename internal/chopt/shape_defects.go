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
	// SettingSince is the first release that accepts the setting the gate
	// stamps. An older server rejects the whole query (UNKNOWN_SETTING, code
	// 115), so the gate can only be stamped on a fleet whose every node is at
	// or above it; such an old node also predates the defect.
	SettingSince Version
}

// DefectGate is how the queries of a defect's shape are handled.
type DefectGate uint8

const (
	// DefectAbsent: no known node carries the defect; nothing is stamped.
	DefectAbsent DefectGate = iota
	// DefectGated: the gate's setting is stamped on the defect's shape.
	DefectGated
	// DefectRefused: a node carries the defect and another is too old to
	// accept the gate's setting, so no query of the shape can be both sent
	// the setting and answered by every node; the shape is refused with an
	// error rather than answered wrong on the affected node.
	DefectRefused
)

// Gate setting floors, verified against real servers by test/chserver's
// setting-floor probe.
var (
	// QueryConditionCacheSettingSince is the first release accepting
	// use_query_condition_cache, the release the cache shipped in.
	QueryConditionCacheSettingSince = Version{Major: 25, Minor: 3}
	// ProjectionFilteringSettingSince is the first release accepting
	// optimize_use_projection_filtering.
	ProjectionFilteringSettingSince = Version{Major: 25, Minor: 6}
)

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
		ID:           ShapeDefectConversionConditionCache,
		SettingSince: QueryConditionCacheSettingSince,
		Builds: []BuildRange{{
			From: Version{Major: 25, Minor: 3}, Until: Version{Major: 26, Minor: 9},
			Defect: "ClickHouse#119168: conversion-function verdicts shared across sessions with different conversion settings",
		}},
	},
	{
		ID:           ShapeDefectNegatedConjunctionProjection,
		SettingSince: ProjectionFilteringSettingSince,
		Builds: []BuildRange{{
			From: Version{Major: 25, Minor: 11}, Until: Version{Major: 26, Minor: 10},
			Defect: "ClickHouse#120286: projection index filters rows a negated conjunction keeps",
		}},
	},
	{
		ID:           ShapeDefectTraceIDProjectionRead,
		SettingSince: ProjectionFilteringSettingSince,
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

// shapeDefectsOf returns every shape defect server carries, gated: a server
// inside a defect's range is at or above its SettingSince
// (TestShapeDefects_RangesStartAtOrAboveTheirSetting).
func shapeDefectsOf(server Version) map[string]DefectGate {
	out := map[string]DefectGate{}
	for _, d := range shapeDefects {
		if KnownShapeDefect(d.ID, server) {
			out[d.ID] = DefectGated
		}
	}
	return out
}

// FleetDefectGate decides how one defect is handled across a fleet probe
// pass, given whether each reached build carries it (affected), the first
// release accepting its gate's setting, whether the pass reached every node,
// and the decision in force. One affected node puts the defect in force, as a
// query of its shape can land there; it is gated when every reached node
// accepts the setting and refused when one does not, because the one driver
// pool picks the node per connection and cannot send the setting to some
// nodes only. A pass that could not reach every node and saw no affected
// build keeps the decision in force, refusing it if a reached node cannot take
// the setting; a complete pass with no affected build clears it.
func FleetDefectGate(versions []Version, complete bool, affected func(Version) bool, settingSince Version, current DefectGate) DefectGate {
	carried, lacking := false, false
	for _, v := range versions {
		carried = carried || affected(v)
		lacking = lacking || v.Less(settingSince)
	}
	switch {
	case !carried && (complete || current == DefectAbsent):
		return DefectAbsent
	case lacking:
		return DefectRefused
	case carried:
		return DefectGated
	default:
		return current
	}
}
