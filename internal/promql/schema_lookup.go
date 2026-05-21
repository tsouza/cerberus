package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// schemaTopLevelColumn returns the dedicated top-level OTel-CH column
// that mirrors a PromQL label name, or "" when the label has no
// top-level column and should fall back to the Attributes-map lookup.
//
// The OTel-CH default metrics schema hoists `service.name` out of the
// Attributes map into a dedicated `ServiceName LowCardinality(String)`
// column. PromQL queries that reach for it under the Prom-grammar
// underscored form (`{service_name="cerberus"}`,
// `sum by (service_name) (...)`) — or the OTel-canonical dotted form
// (`{"service.name"="cerberus"}`) — would otherwise miss every
// OTel-collector-routed row because the value lives ONLY in the
// top-level column, leaving `Attributes['service.name']` /
// `Attributes['service_name']` empty.
//
// Both spellings route to the same `s.ServiceNameColumn` so the wire
// behaviour is symmetric across producers (the Prom-tooling-side may
// have re-underscored the label before the matcher reached us; the
// OTel-side may have kept the dotted form). This mirrors the LogQL
// side's [internal/logql.resourceFallbackColumn] (PR #669 / task #217)
// and the exemplars handler's `ServiceName != ""` precedence in
// [internal/api/prom/exemplars.go::groupExemplars].
//
// A custom-schema user who clears [schema.Metrics.ServiceNameColumn]
// opts out — the helper returns "" so the lowering stays Attributes-
// map-only. The mapping table is intentionally narrow (only
// service.name today); generalise to other top-level columns
// (service_namespace, service_instance_id, scope_name, ...) when those
// bugs surface — don't over-engineer the first cut.
func schemaTopLevelColumn(s schema.Metrics, labelName string) string {
	switch labelName {
	case "service_name", "service.name":
		return s.ServiceNameColumn
	}
	return ""
}

// promqlTopLevelColumnsReferencedBy returns the set of dedicated CH
// column names that the labels in `labels` route to via
// [schemaTopLevelColumn]. Order is preserved against the first
// occurrence; duplicates are dropped so the augmenting Project that
// consumes this list emits a deterministic map shape.
//
// Used by [augmentAttributesForOuterBy] to inflate the inner LWR /
// RangeWindow input's Attributes map with exactly the top-level columns
// the outer aggregation's by-clause references. Mirrors the LogQL
// [internal/logql.topLevelColumnsReferencedBy] from PR #666 / task #218.
func promqlTopLevelColumnsReferencedBy(labels []string, s schema.Metrics) []string {
	if len(labels) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(labels))
	out := make([]string, 0, len(labels))
	for _, lbl := range labels {
		col := schemaTopLevelColumn(s, lbl)
		if col == "" || seen[col] {
			continue
		}
		seen[col] = true
		out = append(out, col)
	}
	return out
}

// promqlTopLevelKeysForOuterBy returns the list of Prom-grammar label
// names that the inner augmenting Project should synthesise into
// Attributes. The names are normalised to the underscored form
// (`service_name`) so the outer aggregate's `Attributes[<label>]`
// lookup — which iterates over [internal/api/format.PromLabelToOTelCandidates] —
// hits the synthesised key on the underscored candidate. Order
// mirrors `outerByLabels` (first-seen wins) with duplicates dropped.
//
// Each returned tuple is `(promLabel, topLevelColumn)`. Both spellings
// of `service.name` collapse to `service_name` here so the wire
// response carries the Prom-canonical form regardless of which
// spelling the user typed in the by-clause.
func promqlTopLevelKeysForOuterBy(labels []string, s schema.Metrics) [][2]string {
	if len(labels) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(labels))
	out := make([][2]string, 0, len(labels))
	for _, lbl := range labels {
		col := schemaTopLevelColumn(s, lbl)
		if col == "" {
			continue
		}
		key := promCanonicalTopLevelLabel(lbl)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, [2]string{key, col})
	}
	return out
}

// promCanonicalTopLevelLabel returns the Prom-canonical underscored
// spelling of a label that routes to a top-level OTel-CH column.
// `service.name` → `service_name`. Used by
// [promqlTopLevelKeysForOuterBy] to normalise the synthesised
// Attributes-map key so the outer aggregate's
// `Attributes['service_name']` lookup hits regardless of which
// spelling the user wrote.
func promCanonicalTopLevelLabel(label string) string {
	switch label {
	case "service.name", "service_name":
		return "service_name"
	}
	return label
}

// augmentAttributesForOuterBy returns a chplan expression that wraps
// the per-row Attributes map with one synthesised key per top-level
// OTel-CH column referenced by `outerByLabels`. The shape is
//
//	mapConcat(
//	    Attributes,
//	    mapFilter((k, v) -> v != '',
//	        map('<promLabel0>', toString(<col0>),
//	            '<promLabel1>', toString(<col1>),
//	            ...)))
//
// `toString` is a no-op for `String`-typed columns (CH elides it at
// the wire) but coerces non-String top-level columns into the
// Map(String, String) value slot. `mapFilter(v != ”)` drops empty-
// column rows so a row with `ServiceName=”` doesn't gain a spurious
// `{service_name:”}` key — matching Prom's "absent label" semantics.
//
// `mapConcat` is later-key-wins, so an explicit Attributes binding
// (a producer that wrote both the top-level column AND the underscored
// map key) is overwritten by the top-level column. That's the
// intended precedence: the dedicated column is the OTel-CH-canonical
// storage shape and the map entry is the fallback.
//
// Returns nil when `outerByLabels` contains no top-level-routed
// labels — callers fold a nil augmentation into "no Project wrap"
// rather than emitting a degenerate identity map.
func augmentAttributesForOuterBy(s schema.Metrics, outerByLabels []string) chplan.Expr {
	pairs := promqlTopLevelKeysForOuterBy(outerByLabels, s)
	if len(pairs) == 0 {
		return nil
	}
	args := make([]chplan.Expr, 0, len(pairs)*2)
	for _, p := range pairs {
		args = append(
			args,
			&chplan.LitString{V: p[0]},
			&chplan.FuncCall{
				Name: "toString",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: p[1]}},
			},
		)
	}
	synth := &chplan.FuncCall{Name: "map", Args: args}
	filtered := &chplan.FuncCall{
		Name: "mapFilter",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{"k", "v"},
				Body: &chplan.Binary{
					Op:    chplan.OpNe,
					Left:  &chplan.BareIdent{Name: "v"},
					Right: &chplan.LitString{V: ""},
				},
			},
			synth,
		},
	}
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}, filtered},
	}
}
