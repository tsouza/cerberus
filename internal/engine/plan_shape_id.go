package engine

import (
	"strconv"
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
)

// shapeIDPrefix namespaces the cerberus shape id inside log_comment so an
// operator scanning system.query_log can tell a cerberus-stamped comment
// apart from any other tool's log_comment at a glance, and so a
// `log_comment LIKE 'cerb:%'` filter selects exactly cerberus queries.
const shapeIDPrefix = "cerb:"

// planShapeID returns a COMPACT, literal-free identifier for plan's shape,
// for stamping into ClickHouse log_comment. It captures the emit-root node
// kind plus a few key structural modifiers (aggregate group-key arity,
// presence of a range window / limit / join / union) and DELIBERATELY omits
// every literal: no metric names, no label values, no timestamps, no group-by
// column names. Two queries with the same plan shape but different literals
// hash to the same id, so operators can cluster system.query_log rows by
// log_comment (alongside normalized_query_hash) without the id itself leaking
// query contents.
//
// The form is `cerb:<root>[;<modifier>...]`, e.g.
// `cerb:project;agg=3;rw` for a Project over a 3-key Aggregate over a
// RangeWindow. Returns "" for a nil plan.
func planShapeID(plan chplan.Node) string {
	if plan == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(shapeIDPrefix)
	b.WriteString(nodeKind(plan))
	for _, m := range shapeModifiers(plan) {
		b.WriteByte(';')
		b.WriteString(m)
	}
	return b.String()
}

// shapeModifiers returns the ordered, literal-free structural modifiers for
// plan. The order is fixed so the same shape always produces the same id.
func shapeModifiers(plan chplan.Node) []string {
	var (
		mods        []string
		aggKeys     = -1
		hasRange    bool
		hasLimit    bool
		hasJoin     bool
		hasUnion    bool
		hasNative   bool
		hasResample bool
	)
	chplan.Walk(plan, func(n chplan.Node) bool {
		switch v := n.(type) {
		case *chplan.Aggregate:
			// Arity only (a count) — never the key names, so no literal leaks.
			if aggKeys < 0 {
				aggKeys = len(v.GroupBy)
			}
		case *chplan.RangeWindow:
			hasRange = true
		case *chplan.RangeWindowNative:
			hasNative = true
		case *chplan.RangeWindowResample:
			hasResample = true
		case *chplan.Limit:
			hasLimit = true
		case *chplan.Scan:
			if len(v.UnionTables) > 0 {
				hasUnion = true
			}
		case *chplan.CrossJoin, *chplan.StructuralJoin, *chplan.VectorJoin, *chplan.InfoJoin:
			hasJoin = true
		}
		return true
	})
	if aggKeys >= 0 {
		mods = append(mods, "agg="+strconv.Itoa(aggKeys))
	}
	if hasRange {
		mods = append(mods, "rw")
	}
	if hasNative {
		mods = append(mods, "rwn")
	}
	if hasResample {
		mods = append(mods, "rwr")
	}
	if hasJoin {
		mods = append(mods, "join")
	}
	if hasUnion {
		mods = append(mods, "union")
	}
	if hasLimit {
		mods = append(mods, "limit")
	}
	return mods
}

// nodeKind returns the short, stable kind token for the emit-root node. The
// tokens are lowercase and literal-free; an unrecognised node kind falls back
// to "node" so the id stays well-formed.
func nodeKind(n chplan.Node) string {
	switch n.(type) {
	case *chplan.Scan:
		return "scan"
	case *chplan.Filter:
		return "filter"
	case *chplan.Project:
		return "project"
	case *chplan.Aggregate:
		return "agg"
	case *chplan.RangeWindow:
		return "rw"
	case *chplan.RangeWindowNative:
		return "rwn"
	case *chplan.RangeWindowResample:
		return "rwr"
	case *chplan.Limit:
		return "limit"
	case *chplan.OrderBy:
		return "orderby"
	case *chplan.UnionAll:
		return "union"
	default:
		return "node"
	}
}
