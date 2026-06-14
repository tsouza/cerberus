package logql

import (
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// materializedColumnFor reports whether `label` names a ResourceAttributes
// map key the OTel ClickHouse Exporter MATERIALIZEs into a dedicated
// top-level LowCardinality(String) column on the logs table (one of the 8
// k8s.* / deployment.environment.name keys). When it does, the second
// return is the materialized column name — a bare ColumnRef against it is
// byte-for-byte equivalent to `ResourceAttributes[<key>]` (the column's
// MATERIALIZED expression IS that map access, including the empty-string
// default a missing key yields) but avoids decompressing the wide Map.
//
// Both the dotted OTel-semantic-convention form (`k8s.namespace.name`,
// the actual map key) and the underscored Grafana form
// (`k8s_namespace_name`, the identifier Grafana panels + the LogQL
// grammar surface) resolve — the underscored form is normalised to dots
// before the lookup, mirroring resourceFallbackColumn's `service_name`
// alias. A custom-schema user whose otel_logs has no materialized columns
// leaves schema.Logs.MaterializedResourceColumns nil and opts out: the
// table is empty so this never fires and the lowering stays map-only.
func materializedColumnFor(label string, s schema.Logs) (string, bool) {
	if label == "" || len(s.MaterializedResourceColumns) == 0 {
		return "", false
	}
	if col, ok := s.MaterializedResourceColumns[label]; ok {
		return col, true
	}
	// Grafana panels + the LogQL grammar spell the dotted OTel key with
	// underscores (`k8s_namespace_name`). The map is keyed by the dotted
	// form (the literal ResourceAttributes key), so normalise before the
	// second lookup.
	if dotted := strings.ReplaceAll(label, "_", "."); dotted != label {
		if col, ok := s.MaterializedResourceColumns[dotted]; ok {
			return col, true
		}
	}
	return "", false
}

// topLevelLogColumnFor reports whether `label` names a top-level OTel-CH
// scalar column on the logs table (rather than a key inside the
// ResourceAttributes / LogAttributes maps). When it does, the second
// return is the underlying column name (`SeverityText`, `ServiceName`,
// ...) that the lowering should reference directly via `ColumnRef`.
//
// Why this exists: cerberus's default OTel-CH schema dedicates several
// fields (severity, service name, scope, trace correlation, event name)
// to their own top-level columns alongside the generic
// ResourceAttributes Map. Users (and Grafana panels) reach for those
// fields by their canonical column names (`sum by (SeverityText) (...)`,
// `sum by (ServiceName) (...)`) — the LogQL grammar accepts any label
// identifier, so the parser doesn't distinguish "a stream attribute"
// from "a top-level column". Before this helper, `levelAwareGroupKey`
// resolved every label as `ResourceAttributes[<label>]`, which silently
// returned the empty string for top-level columns and collapsed every
// matching query into a single `{<label>:""}` series — the bug task #218
// reported (every Log volume / Log rate by-clause dashboard panel
// broken).
//
// The recognised set mirrors the schema.Logs fields that name a
// scalar (non-Map) column. Map-typed columns (LogAttributes,
// ResourceAttributes, ScopeAttributes) are deliberately excluded
// because users group by keys inside those maps, not by the map as a
// whole. The set is closed against the schema: a custom-schema user
// who renames `SeverityColumn` to `level_text` automatically gets
// `level_text` as the recognised top-level label here too, because
// the resolution reads from the schema fields rather than a static
// allow-list of names.
//
// Both the inner range-aggregation identity wrap ([withDetectedLevel]
// and friends) and the inner range-aggregation's own `by/without`
// resolution ([levelAwareRangeGroupKey]) consult this helper so the
// two grouping layers agree on which labels surface from top-level
// columns vs which fall back to the ResourceAttributes map.
func topLevelLogColumnFor(label string, s schema.Logs) (string, bool) {
	if label == "" {
		return "", false
	}
	// Each case matches `label` against a schema field only when the
	// field itself is non-empty — a custom schema that blanks a column
	// out (e.g. no dedicated EventName column) must not collapse
	// every empty-string label lookup into a spurious match.
	candidates := []string{
		s.SeverityColumn,
		s.SeverityNumberColumn,
		s.ServiceNameColumn,
		s.ScopeNameColumn,
		s.ScopeVersionColumn,
		s.EventNameColumn,
		s.TraceIDColumn,
		s.SpanIDColumn,
		s.TraceFlagsColumn,
	}
	for _, col := range candidates {
		if col != "" && col == label {
			return col, true
		}
	}
	return "", false
}

// topLevelColumnsReferencedBy returns the subset of `labels` that name
// top-level OTel-CH scalar columns on the schema. Order is preserved
// and duplicates are dropped so the downstream identity wrap emits a
// deterministic map shape. Used by [withDetectedLevel] to inflate the
// augmented identity map with exactly the top-level columns an outer
// `by(...)` clause references — see [lowerCtx.OuterByLabels].
func topLevelColumnsReferencedBy(labels []string, s schema.Logs) []string {
	if len(labels) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(labels))
	out := make([]string, 0, len(labels))
	for _, lbl := range labels {
		col, ok := topLevelLogColumnFor(lbl, s)
		if !ok || seen[col] {
			continue
		}
		seen[col] = true
		out = append(out, col)
	}
	return out
}

// topLevelColumnRef returns a chplan ColumnRef pointing at the
// top-level OTel-CH column named `col`. Used by both the inner range
// aggregation's `by/without` resolution and the augmented-identity
// wrap so the two paths emit identical column references.
func topLevelColumnRef(col string) chplan.Expr {
	return &chplan.ColumnRef{Name: col}
}

// structuredOuterByKeys returns the subset of an enclosing vector
// aggregation's by-clause labels that are NEITHER a top-level OTel-CH
// scalar column (handled by [topLevelColumnsReferencedBy]) NOR the
// synthesized `detected_level` family (handled by [withDetectedLevel]
// directly). These are the labels that resolve from the
// structured-metadata (LogAttributes) / stream (ResourceAttributes)
// maps and must be inflated into the inner range aggregation's
// synthesized identity map so the outer aggregation can read them back
// after the RangeWindow (see [withDetectedLevelAndColumns]). Order is
// preserved and duplicates dropped for a deterministic map shape.
func structuredOuterByKeys(labels []string, s schema.Logs) []string {
	if len(labels) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(labels))
	out := make([]string, 0, len(labels))
	for _, lbl := range labels {
		if lbl == "" || seen[lbl] {
			continue
		}
		if _, ok := topLevelLogColumnFor(lbl, s); ok {
			continue
		}
		if isDetectedLevelGroupingLabel(lbl) {
			continue
		}
		seen[lbl] = true
		out = append(out, lbl)
	}
	return out
}
