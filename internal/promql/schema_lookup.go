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

// promqlTopLevelKeys returns every `(promLabel, topLevelColumn)` pair the
// selector Project must synthesise into Attributes for schema s. The
// names are normalised to the underscored form (`service_name`) so both
// a wire response reading the map directly and an outer aggregate's
// `Attributes[<label>]` lookup — which iterates over
// [internal/api/format.PromLabelToOTelCandidates] — hit the synthesised
// key on the underscored candidate.
//
// The set is derived from the schema, NOT from the query: a dedicated
// column is part of the series' identity whether or not the query names
// it. Deriving it from an enclosing by-clause is what made a bare
// selector silently drop the label — see [dedicatedResourceKeys], which
// removes the same keys from the ResourceAttributes arm on the promise
// that this path surfaces them.
//
// Pairs whose column is unconfigured in s are omitted, so a custom
// schema that clears [schema.Metrics.ServiceNameColumn] opts out and the
// ResourceAttributes arm becomes the only path again.
func promqlTopLevelKeys(s schema.Metrics) [][2]string {
	out := make([][2]string, 0, len(dedicatedResourceKeys))
	for _, d := range dedicatedResourceKeys {
		col := d.column(s)
		if col == "" {
			continue
		}
		out = append(out, [2]string{promCanonicalTopLevelLabel(d.otelKey), col})
	}
	return out
}

// promCanonicalTopLevelLabel returns the Prom-canonical underscored
// spelling of a label that routes to a top-level OTel-CH column.
// `service.name` → `service_name`. Used by [promqlTopLevelKeys] to
// normalise the synthesised Attributes-map key so the outer aggregate's
// `Attributes['service_name']` lookup hits regardless of which
// spelling the user wrote.
func promCanonicalTopLevelLabel(label string) string {
	switch label {
	case "service.name", "service_name":
		return "service_name"
	}
	return label
}

// augmentAttributesForTopLevelExpr returns a chplan expression that wraps
// the supplied base Attributes expression with one synthesised key per
// dedicated top-level OTel-CH column configured in s. The shape is
//
//	mapConcat(
//	    mapFilter((k, v) -> k NOT IN ('<promLabel0>', ...), <base>),
//	    mapFilter((k, v) -> v != '',
//	        map('<promLabel0>', toString(<col0>),
//	            '<promLabel1>', toString(<col1>),
//	            ...)))
//
// The base expression is supplied by the caller so the synthesised
// top-level-column overlay composes ON TOP of an already-merged map (e.g.
// the resource-attribute merge from [mergeResourceAttributesExpr]) rather
// than the raw column.
//
// `toString` is a no-op for `String`-typed columns (CH elides it at
// the wire) but coerces non-String top-level columns into the
// Map(String, String) value slot. `mapFilter(v != ”)` drops empty-
// column rows so a row with `ServiceName=”` doesn't gain a spurious
// `{service_name:”}` key — matching Prom's "absent label" semantics.
//
// ClickHouse's `mapConcat` does NOT collapse duplicate keys — it
// concatenates both sides' key/value arrays, leaving both entries in the
// resulting Map. "Later-key-wins" holds only for a *subscript* read
// (`m['k']`); cerberus's series identity is the whole map, so relying on
// mapConcat ordering alone would let two datapoints that should fold into
// one series (dedicated column wins, #232) stay split, and would let the
// wire-rendered label ([internal/chclient]'s `buildLabelMap`, which
// collapses last-wins after `mapSort`) contradict the grouping key that
// saw the first-occurrence value (#2467). So the overlaid keys are
// stripped out of `base` BEFORE the concat, making the dedicated column
// win STRUCTURALLY rather than by mapConcat ordering — the exact shape
// [internal/chsql/info_join.go]'s `infoExtrasFrag` already uses for the
// mirror-image hazard (there the base wins structurally; here the overlay
// does).
//
// Returns nil when s configures no dedicated top-level column — callers
// fold a nil augmentation into "no Project wrap" rather than emitting a
// degenerate identity map.
func augmentAttributesForTopLevelExpr(s schema.Metrics, base chplan.Expr) chplan.Expr {
	pairs := promqlTopLevelKeys(s)
	if len(pairs) == 0 {
		return nil
	}
	args := make([]chplan.Expr, 0, len(pairs)*2)
	excluded := make([]chplan.Expr, 0, len(pairs))
	for _, p := range pairs {
		args = append(
			args,
			&chplan.LitString{V: p[0]},
			&chplan.FuncCall{
				Fn:   chplan.FnToString,
				Args: []chplan.Expr{&chplan.ColumnRef{Name: p[1]}},
			},
		)
		excluded = append(excluded, &chplan.LitString{V: p[0]})
	}
	synth := &chplan.FuncCall{Fn: chplan.FnMap, Args: args}
	filtered := &chplan.FuncCall{
		Fn: chplan.FnMapFilter,
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
	baseWithoutOverlaidKeys := &chplan.FuncCall{
		Fn: chplan.FnMapFilter,
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{"k", "v"},
				Body: &chplan.InList{
					Left:    &chplan.BareIdent{Name: "k"},
					List:    excluded,
					Negated: true,
				},
			},
			base,
		},
	}
	return &chplan.FuncCall{
		Fn:   chplan.FnMapMerge,
		Args: []chplan.Expr{baseWithoutOverlaidKeys, filtered},
	}
}
