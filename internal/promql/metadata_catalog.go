package promql

import (
	"context"
	"sort"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/promql/parser"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// MetadataValueColumn is the alias the label-VALUES catalog lowering
// projects its single output column under; MetadataNameColumn is the
// label-NAMES equivalent. The HTTP layer's outer `SELECT DISTINCT …`
// wrapper references these names, so they are part of the contract
// between [LowerMetadataLabelValues] / [LowerMetadataLabelNames] and
// internal/api/prom.
const (
	MetadataValueColumn = "value"
	MetadataNameColumn  = "name"
)

// catalogKind distinguishes the two single-column metadata catalogs.
type catalogKind uint8

const (
	// catalogLabelValues projects the value of ONE known label per matched row.
	catalogLabelValues catalogKind = iota
	// catalogLabelNames projects the label NAMES each matched row carries.
	catalogLabelNames
)

// metadataCatalog is the plan-time description of what a match[]-driven
// catalog endpoint wants out of a lowering. It is threaded on [lowerCtx]
// so the selector seam knows to leave the raw identity columns alone
// (see [selectorAttributesExpr]) and the metadata wrap knows to project
// the single catalog column instead of the Sample quadruple (see
// [wrapMetadataCatalog]).
type metadataCatalog struct {
	kind catalogKind
	// label is the Prom label whose values are wanted; unused for
	// catalogLabelNames.
	label string
}

// alias returns the output column name for the catalog projection.
func (c *metadataCatalog) alias() string {
	if c.kind == catalogLabelNames {
		return MetadataNameColumn
	}
	return MetadataValueColumn
}

// LowerMetadataLabelValues lowers one match[] selector into a plan that
// projects EXACTLY ONE column: the value `label` takes on each matched
// row, over the closed [start,end] metadata window.
//
// The requested label is known at plan time, so the projection is
// resolved at the leaf — a single map subscript (or dedicated-column
// coalesce) on the scanned row. Nothing above the scan has to
// materialise, rename, or group by the row's whole attribute map to
// answer "which values does this one label take", which is what a
// generic Sample-shaped metadata lowering would force: rebuilding the
// merged label map per row is O(rows × attributes) work and O(rows ×
// attributes) group state for an O(distinct values) answer.
//
// The value expression is [rawLabelValueExpr] — the SAME resolution the
// matcher path uses for `{<label>="…"}`. Listing and matching therefore
// agree by construction: every value this endpoint advertises is a value
// a selector on that label actually matches.
func LowerMetadataLabelValues(ctx context.Context, expr parser.Expr, s schema.Metrics, start, end time.Time, label string) (chplan.Node, error) {
	return lowerMetadataCatalog(ctx, expr, s, start, end, &metadataCatalog{kind: catalogLabelValues, label: label})
}

// LowerMetadataLabelNames lowers one match[] selector into a plan that
// projects EXACTLY ONE column: the label names each matched row carries,
// fanned out with arrayJoin, over the closed [start,end] metadata window.
//
// Keys are projected in their STORAGE spelling; the caller folds them
// through the Prom label-name grammar (format.NormalizeLabelNames) as it
// already does for every other name source. Sanitising in Go instead of
// in SQL removes a per-key, per-row regex from the scan and lets the
// projection read the stored maps' key arrays directly instead of
// rebuilding a renamed map per row.
func LowerMetadataLabelNames(ctx context.Context, expr parser.Expr, s schema.Metrics, start, end time.Time) (chplan.Node, error) {
	return lowerMetadataCatalog(ctx, expr, s, start, end, &metadataCatalog{kind: catalogLabelNames})
}

// lowerMetadataCatalog is the shared entry point behind the two catalog
// lowerings. It mirrors [LowerMetadataRange] — same full-range metadata
// window semantics, same bare-selector contract — and differs only in
// the projection the metadata seam emits.
func lowerMetadataCatalog(
	ctx context.Context, expr parser.Expr, s schema.Metrics,
	start, end time.Time, cat *metadataCatalog,
) (chplan.Node, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanLower, trace.WithAttributes(cerbtrace.AttrQL.String("promql")))
	defer span.End()
	plan, err := lower(expr, s, lowerCtx{
		start:             start,
		end:               end,
		metadataFullRange: true,
		catalog:           cat,
		lowerers:          RangeLowerers{}.withDefaults(),
	})
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(cerbtrace.AttrPlanNodeCount.Int(cerbtrace.CountNodes(plan)))
	return plan, nil
}

// wrapMetadataCatalog is the catalog-mode counterpart of
// [wrapMetadataFullRange]. It applies the same closed [start,end] window
// (ANDed onto the selector's matcher predicate, which catalog mode
// deliberately leaves un-sunk so both land on one Filter directly above
// the scan) and then projects the single catalog column.
//
// What it does NOT emit is the whole point:
//
//   - no `GROUP BY (MetricName, Attributes)` — the map-valued grouping
//     key a per-series collapse needs is pure overhead when the answer
//     is a flat set of strings; the caller's outer `SELECT DISTINCT`
//     dedupes from a hash set keyed on one column instead;
//   - no `any(TimeUnix)` / `any(Value)` — neither column appears in a
//     label-names or label-values response, so nothing downstream reads
//     the aggregates a Sample-shaped collapse would compute;
//   - no renamed-attribute-map projection — see [selectorAttributesExpr].
func wrapMetadataCatalog(
	scan chplan.Node, pred chplan.Expr, start, end time.Time,
	s schema.Metrics, cat *metadataCatalog,
) chplan.Node {
	input := scan
	if combined := metadataWindowPredicate(pred, start, end, s); combined != nil {
		input = &chplan.Filter{Input: scan, Predicate: combined}
	}
	return &chplan.Project{
		Input: input,
		Projections: []chplan.Projection{
			{Expr: catalogProjectionExpr(s, cat), Alias: cat.alias()},
		},
	}
}

// catalogProjectionExpr builds the single expression a catalog plan
// projects.
func catalogProjectionExpr(s schema.Metrics, cat *metadataCatalog) chplan.Expr {
	if cat.kind == catalogLabelNames {
		return catalogLabelNamesExpr(s)
	}
	return catalogLabelValuesExpr(s, cat.label)
}

// catalogLabelNamesExpr fans a row's label names out to one row each:
//
//	arrayJoin(arrayConcat(mapKeys(Attributes), mapKeys(<resource source map>)))
//
// The resource side reuses [resourceSourceMap] so the allowlist filter
// and the dedicated-column exclusion (service.name → ServiceName) that
// govern the read-path projection govern the name catalog identically. A
// key present in both maps is emitted twice and folded by the caller's
// dedupe.
func catalogLabelNamesExpr(s schema.Metrics) chplan.Expr {
	keys := chplan.Expr(&chplan.FuncCall{
		Name: "mapKeys",
		Args: []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	})
	if resourceAttributesActive(s) {
		keys = &chplan.FuncCall{
			Name: "arrayConcat",
			Args: []chplan.Expr{
				keys,
				&chplan.FuncCall{Name: "mapKeys", Args: []chplan.Expr{resourceSourceMap(s)}},
			},
		}
	}
	return &chplan.FuncCall{Name: "arrayJoin", Args: []chplan.Expr{keys}}
}

// catalogLabelValuesExpr resolves one Prom label to its per-row value.
//
// `__name__` reads the MetricName column — on the classic-histogram
// companion arms that column carries the arm's synthesised Prom-visible
// name, so the catalog advertises the spelling a selector can query.
// Every other label goes through [rawLabelValueExpr], the matcher path's
// own resolution.
func catalogLabelValuesExpr(s schema.Metrics, label string) chplan.Expr {
	if label == model.MetricNameLabel {
		return &chplan.ColumnRef{Name: s.MetricNameColumn}
	}
	return rawLabelValueExpr(s, label)
}

// catalogAttributesProjections returns the label-carrying projections a
// selector-shape Project must expose.
//
// Outside catalog mode that is the single merged/renamed Attributes
// column the read path contracts on. In catalog mode the merge is
// skipped and the raw sources are passed through instead, because the
// catalog projection above resolves ONE label out of them
// ([catalogLabelValuesExpr]) and would otherwise have to subscript a map
// this Project had just rebuilt row by row. The dedicated top-level
// column travels with them: a label backed by one (service.name →
// ServiceName) resolves against that column, and a Project that dropped
// it would put the column out of scope for every shape that unions arms
// together.
//
// attrs is the Attributes expression to project in EITHER mode — callers
// that source it from the raw column pass [selectorAttributesSource], and
// the classic-histogram bucket fan-out passes its `le`-augmented map.
func catalogAttributesProjections(cat *metadataCatalog, s schema.Metrics, attrs chplan.Expr) []chplan.Projection {
	out := []chplan.Projection{{Expr: attrs, Alias: s.AttributesColumn}}
	if cat == nil {
		return out
	}
	if resourceAttributesActive(s) {
		out = append(out, chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
			Alias: s.ResourceAttributesColumn,
		})
	}
	for _, col := range dedicatedLabelColumns(s) {
		out = append(out, chplan.Projection{Expr: &chplan.ColumnRef{Name: col}, Alias: col})
	}
	return out
}

// selectorAttributesSource returns the Attributes expression a
// selector-shape Project reads a metric table with: the merged read-path
// map with the dedicated-column overlay folded in normally, the raw
// column in catalog mode (where the merge would be built per row only to
// be subscripted once).
//
// The overlay MUST be applied here, not above a UnionAll of these
// projections: the arms collapse to the canonical Sample quadruple, so
// the raw ServiceName column is out of scope for anything stacked on top
// (see [lowerCtx.attributesPreMerged]).
func selectorAttributesSource(cat *metadataCatalog, s schema.Metrics) chplan.Expr {
	if cat != nil {
		return &chplan.ColumnRef{Name: s.AttributesColumn}
	}
	merged := mergeResourceAttributesExpr(s)
	if overlay := augmentAttributesForTopLevelExpr(s, merged); overlay != nil {
		return canonicalAttributesExpr(overlay)
	}
	return canonicalAttributesExpr(merged)
}

// selectorLabelProjections is [catalogAttributesProjections] over
// [selectorAttributesSource] — the shape every selector Project that
// reads a metric table directly uses.
func selectorLabelProjections(cat *metadataCatalog, s schema.Metrics) []chplan.Projection {
	return catalogAttributesProjections(cat, s, selectorAttributesSource(cat, s))
}

// dedicatedLabelColumns lists the configured top-level columns that back
// a Prom label, in a stable order. It reads the same [promqlTopLevelKeys]
// registry the read-path overlay does, so the metadata surface and the
// projected series can never disagree about which columns exist.
func dedicatedLabelColumns(s schema.Metrics) []string {
	pairs := promqlTopLevelKeys(s)
	if len(pairs) == 0 {
		return nil
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p[1])
	}
	return out
}

// sanitizePromLabelChars is the Go mirror of the SQL-side
// [promLabelSanitizePattern] rewrite: every byte outside
// `[a-zA-Z0-9_]` becomes `_`. It deliberately does NOT add the
// leading-underscore prefix format.OTelToPromLabel applies to
// digit-leading names — the SQL projection it mirrors does not either
// (see promLabelSanitizePattern's docstring).
func sanitizePromLabelChars(k string) string {
	b := make([]byte, len(k))
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			b[i] = c
		default:
			b[i] = '_'
		}
	}
	return string(b)
}

// configuredResourceKeysFor inverts the label sanitisation over the
// CONFIGURED resource-attribute allowlist: it returns every allowlisted
// OTel key whose sanitised spelling is promLabel.
//
// The sanitisation `k -> replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_')` is
// many-to-one, so it cannot be inverted over an open key set — but the
// allowlist IS a closed, plan-time-known set, and over a closed set the
// inverse is exact and computable in Go. Two distinct source keys that
// differ only in separator (`a.b` and `a-b`) collide onto one label and
// BOTH belong in the result; returning the set rather than a single key
// is what keeps the resolution complete.
//
// Order follows the configured allowlist so a deployment's own ordering
// decides precedence between colliding keys; duplicates drop. An empty
// allowlist (the promote-all default) has no closed key set to invert,
// so the result is empty and the dot/underscore candidate chain in
// [attributeLookup] remains the only resource-side resolution.
func configuredResourceKeysFor(s schema.Metrics, promLabel string) []string {
	if len(s.PromResourceLabels) == 0 || s.ResourceAttributesColumn == "" {
		return nil
	}
	excluded := excludedResourceKeys(s)
	// Keys the dot/underscore candidate chain already reaches are skipped —
	// they would only duplicate an arm [attributeLookup] emits.
	covered := make(map[string]struct{})
	for _, c := range attributeLookupKeys(promLabel) {
		covered[c] = struct{}{}
	}
	seen := make(map[string]struct{}, len(s.PromResourceLabels))
	out := make([]string, 0, len(s.PromResourceLabels))
	for _, k := range s.PromResourceLabels {
		if sanitizePromLabelChars(k) != promLabel && format.OTelToPromLabel(k) != promLabel {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		if _, drop := excluded[k]; drop {
			continue
		}
		if _, dup := covered[k]; dup {
			continue
		}
		out = append(out, k)
	}
	return out
}

// sanitizedResourceKeyPairs returns the allowlisted resource keys paired
// with their sanitised Prom spelling, sorted by key for a deterministic
// emit. It is the plan-time lookup table [sanitizeMapKeysExpr] uses in
// place of a per-key regex when the key set is closed.
func sanitizedResourceKeyPairs(s schema.Metrics) (keys, sanitized []string) {
	seen := make(map[string]struct{}, len(s.PromResourceLabels))
	uniq := make([]string, 0, len(s.PromResourceLabels))
	for _, k := range s.PromResourceLabels {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		uniq = append(uniq, k)
	}
	sort.Strings(uniq)
	sanitized = make([]string, 0, len(uniq))
	for _, k := range uniq {
		sanitized = append(sanitized, sanitizePromLabelChars(k))
	}
	return uniq, sanitized
}
