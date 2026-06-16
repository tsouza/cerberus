package promql

import (
	"sort"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// promLabelSanitizePattern is the SQL-side mirror of
// [format.OTelToPromLabel]'s character-class rewrite: every byte outside
// the Prometheus label-name grammar `[a-zA-Z0-9_]` is replaced with `_`
// so OTel ResourceAttributes keys (`k8s.namespace.name`,
// `deployment.environment.name`) surface as Prom-legal label names
// (`k8s_namespace_name`, `deployment_environment_name`). Used by both the
// read-path projection ([mergeResourceAttributesExpr]) and never inline
// elsewhere — keep it in lock-step with the Go normaliser in
// internal/api/format/otelname.go.
//
// The leading-digit `_` prefix [format.OTelToPromLabel] applies is not
// mirrored here: OTel resource-attribute keys never begin with a digit,
// and adding the guard would bloat every selector's SQL with an
// `if(match(k,'^[0-9]'), …)` per key. If a leading-digit key ever
// surfaces it sanitizes its non-leading bytes correctly; only the missing
// prefix differs, which is below the wire-visibility threshold for the
// resource keys this feature targets.
const promLabelSanitizePattern = "[^a-zA-Z0-9_]"

// resourceAttributesActive reports whether the resource-attribute arm is
// enabled for schema s: the schema must name a ResourceAttributes column.
// A custom schema that clears ResourceAttributesColumn opts out entirely,
// keeping every existing fixture byte-identical.
func resourceAttributesActive(s schema.Metrics) bool {
	return s.ResourceAttributesColumn != ""
}

// dedicatedResourceKey pairs an OTel resource-attribute key that the
// OTel-CH exporter also materialises into a dedicated top-level column
// with the schema field naming that column. When the dedicated column is
// configured the resource-attribute arm MUST NOT promote the same key
// again — the dedicated path (e.g. [schemaTopLevelColumn] →
// [augmentAttributesForOuterByExpr]) already surfaces it — otherwise the key
// double-promotes and the projected series diverges from reference
// Prometheus on every row (the value lands once via the dedicated column
// and once via the ResourceAttributes map).
type dedicatedResourceKey struct {
	otelKey string
	column  func(schema.Metrics) string
}

// dedicatedResourceKeys is the data-driven registry of OTel resource keys
// backed by a dedicated top-level column. It mirrors the mapping
// [schemaTopLevelColumn] resolves: today only service.name →
// ServiceNameColumn. Extend this slice (not an inline literal) when a new
// resource key gains a dedicated column.
var dedicatedResourceKeys = []dedicatedResourceKey{
	{otelKey: "service.name", column: func(s schema.Metrics) string { return s.ServiceNameColumn }},
}

// excludedResourceKeys returns the set of OTel resource keys that the
// resource arm must skip because a dedicated column already backs them —
// each in BOTH its original dotted form (service.name) and its sanitized
// Prom-wire form (service_name), so an IN/NOT-IN filter or a candidate-
// chain check catches whichever spelling the data or matcher carries.
// Keys whose dedicated column is unconfigured in s are not excluded (the
// resource arm is then the only path that can surface them).
func excludedResourceKeys(s schema.Metrics) map[string]struct{} {
	out := make(map[string]struct{}, len(dedicatedResourceKeys)*2)
	for _, d := range dedicatedResourceKeys {
		if d.column(s) == "" {
			continue
		}
		out[d.otelKey] = struct{}{}
		out[format.OTelToPromLabel(d.otelKey)] = struct{}{}
	}
	return out
}

// DedicatedResourceLabelExcluded reports whether promLabel addresses an
// OTel resource key already backed by a dedicated top-level column in
// schema s (e.g. service.name → ServiceName). The metadata surface
// (/labels, /label/<name>/values) and the matcher path both consult it so
// the dedicated key is never surfaced via the resource arm — the
// dedicated ServiceName path owns it. A label is excluded when any of its
// dot<->underscore candidates names an excluded dedicated key.
func DedicatedResourceLabelExcluded(s schema.Metrics, promLabel string) bool {
	excluded := excludedResourceKeys(s)
	if len(excluded) == 0 {
		return false
	}
	for _, cand := range format.PromLabelToOTelCandidates(promLabel) {
		if _, ok := excluded[cand]; ok {
			return true
		}
	}
	return false
}

// resourceLabelAllowed reports whether a matcher / projection should
// consult ResourceAttributes for the Prom label promLabel. An empty
// allowlist promotes every key (the default). When the allowlist is
// non-empty, promLabel is allowed iff any of its dot<->underscore
// candidates (the same set [attributeLookup] resolves against) names an
// allowlisted OTel dotted key — so an operator listing `k8s.namespace.name`
// allows the `{k8s_namespace_name=…}` matcher.
//
// A label backed by a dedicated top-level column (service.name →
// ServiceName) is NEVER allowed here regardless of allowlist: the
// dedicated path owns it, and promoting it via the resource arm too would
// double-promote and diverge from reference Prometheus.
func resourceLabelAllowed(s schema.Metrics, promLabel string) bool {
	if DedicatedResourceLabelExcluded(s, promLabel) {
		return false
	}
	if len(s.PromResourceLabels) == 0 {
		return true
	}
	allow := make(map[string]struct{}, len(s.PromResourceLabels))
	for _, k := range s.PromResourceLabels {
		allow[k] = struct{}{}
	}
	for _, cand := range format.PromLabelToOTelCandidates(promLabel) {
		if _, ok := allow[cand]; ok {
			return true
		}
	}
	return false
}

// sanitizeMapKeysExpr returns a chplan expression that rewrites every key of
// the given source map to its Prom-legal sanitized form. The shape is:
//
//	mapFromArrays(
//	    arrayMap(k -> replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_'), mapKeys(<src>)),
//	    mapValues(<src>))
//
// All composition is typed chplan.FuncCall / Lambda nodes — no raw SQL. The
// source map is evaluated twice (mapKeys + mapValues); it must be a pure
// column ref or filter expression with no side effects (resourceSourceMap and
// the bare Attributes ColumnRef both qualify).
func sanitizeMapKeysExpr(src chplan.Expr) chplan.Expr {
	sanitizedKeys := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{"k"},
				Body: &chplan.FuncCall{
					Name: "replaceRegexpAll",
					Args: []chplan.Expr{
						&chplan.BareIdent{Name: "k"},
						&chplan.InlineString{V: promLabelSanitizePattern},
						&chplan.InlineString{V: "_"},
					},
				},
			},
			&chplan.FuncCall{Name: "mapKeys", Args: []chplan.Expr{src}},
		},
	}
	return &chplan.FuncCall{
		Name: "mapFromArrays",
		Args: []chplan.Expr{
			sanitizedKeys,
			&chplan.FuncCall{Name: "mapValues", Args: []chplan.Expr{src}},
		},
	}
}

// sanitizeResourceKeysExpr returns the sanitized, allowlist-filtered
// ResourceAttributes map, optionally pre-filtered to the configured allowlist.
// When the allowlist is non-empty the source map is narrowed first:
//
//	mapFromArrays(
//	    arrayMap(k -> replaceRegexpAll(k, …), mapKeys(mapFilter((k,v)->k IN (?,…), <RA>))),
//	    mapValues(mapFilter((k,v)->k IN (?,…), <RA>)))
//
// The allowlist filter matches the ORIGINAL dotted key (so operators list
// the OTel key, not the sanitized form), matching the matcher-side
// [resourceLabelAllowed] gate.
func sanitizeResourceKeysExpr(s schema.Metrics) chplan.Expr {
	return sanitizeMapKeysExpr(resourceSourceMap(s))
}

// resourceSourceMap returns the ResourceAttributes column ref, ALWAYS
// excluding the dedicated-column-backed keys (service.name →
// ServiceName) and additionally narrowed to the configured allowlist when
// one is set. The two predicates AND together inside a single mapFilter:
//
//	mapFilter((k, v) -> k NOT IN ('service.name', 'service_name')
//	                    [AND k IN ('<key0>', …)], ResourceAttributes)
//
// The IN/NOT-IN lists match the ORIGINAL dotted key (and, for the
// dedicated exclusion, the sanitized form too, since rows may store
// either spelling). When no allowlist is set and no dedicated column is
// configured the function returns the bare column ref so the default
// (promote-all) path stays minimal — byte-identical to legacy fixtures.
func resourceSourceMap(s schema.Metrics) chplan.Expr {
	ra := &chplan.ColumnRef{Name: s.ResourceAttributesColumn}

	var pred chplan.Expr
	if excluded := excludedResourceKeys(s); len(excluded) > 0 {
		keys := make([]string, 0, len(excluded))
		for k := range excluded {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		list := make([]chplan.Expr, 0, len(keys))
		for _, k := range keys {
			list = append(list, &chplan.LitString{V: k})
		}
		pred = &chplan.InList{
			Left:    &chplan.BareIdent{Name: "k"},
			List:    list,
			Negated: true,
		}
	}

	if len(s.PromResourceLabels) > 0 {
		list := make([]chplan.Expr, 0, len(s.PromResourceLabels))
		for _, k := range s.PromResourceLabels {
			list = append(list, &chplan.LitString{V: k})
		}
		in := &chplan.InList{
			Left: &chplan.BareIdent{Name: "k"},
			List: list,
		}
		if pred == nil {
			pred = in
		} else {
			pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: in}
		}
	}

	if pred == nil {
		return ra
	}
	return &chplan.FuncCall{
		Name: "mapFilter",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{"k", "v"},
				Body:   pred,
			},
			ra,
		},
	}
}

// mergeResourceAttributesExpr returns the base merged-label expression for
// the read path:
//
//	mapUpdate(sanitize(allowlist-filtered ResourceAttributes), sanitize(Attributes))
//
// `mapUpdate(a, b)` is ClickHouse's later-wins map merge keeping a's type,
// so per-datapoint Attributes (b) override ResourceAttributes (a) on a key
// collision — the Prom precedence the PRECEDENCE CONTRACT pins. BOTH maps'
// keys are sanitized (dot→underscore) before the merge so a key present in
// both — e.g. `k8s.namespace.name` — collides on its sanitized wire spelling
// (`k8s_namespace_name`) and Attributes wins, matching the candidate-aware
// matcher path. Sanitizing only the RA side would let a dotted Attributes key
// dodge the collision: RA would win the projected value AND a phantom
// Prom-illegal dotted label would leak, while the matcher made Attributes win
// — the series would filter on one value and display another. The cost is a
// changed wire spelling for any DOTTED metric-Attributes key (now underscore
// form, the more Prom-correct spelling; rare in practice). When the schema
// clears ResourceAttributesColumn the function returns the bare Attributes
// ColumnRef so existing fixtures stay byte-identical.
func mergeResourceAttributesExpr(s schema.Metrics) chplan.Expr {
	attrs := &chplan.ColumnRef{Name: s.AttributesColumn}
	if !resourceAttributesActive(s) {
		return attrs
	}
	return &chplan.FuncCall{
		Name: "mapUpdate",
		Args: []chplan.Expr{sanitizeResourceKeysExpr(s), sanitizeMapKeysExpr(attrs)},
	}
}

// isBareAttributesRef reports whether expr is exactly the bare Attributes
// ColumnRef — used to skip the selector Project (and the matching pred
// sink) when the resource merge is a no-op (no ResourceAttributesColumn)
// and there is no outer-by overlay, keeping legacy fixtures byte-identical.
func isBareAttributesRef(expr chplan.Expr, s schema.Metrics) bool {
	ref, ok := expr.(*chplan.ColumnRef)
	return ok && ref.Name == s.AttributesColumn && ref.Qualifier == ""
}

// resourceMatcherFallback returns the ResourceAttributes-side lookup arm
// for a non-service, non-__name__ matcher, or nil when the resource arm is
// disabled (schema cleared the column, or the label is not allowlisted).
// The returned expr is the resource-side candidate-chain lookup wrapped in
// nullIf so an absent/empty resource value yields NULL — the caller folds
// it into the Attributes-wins coalesce.
func resourceMatcherFallback(s schema.Metrics, promLabel string) chplan.Expr {
	if !resourceAttributesActive(s) || !resourceLabelAllowed(s, promLabel) {
		return nil
	}
	return &chplan.FuncCall{
		Name: "nullIf",
		Args: []chplan.Expr{
			attributeLookup(s.ResourceAttributesColumn, promLabel),
			&chplan.LitString{V: ""},
		},
	}
}
