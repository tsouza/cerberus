package tempo

import (
	"context"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// --- Tag-catalog fast path (cerberus issue #2771) ---
//
// The Tempo sibling of internal/api/loki's detected_labels catalog fast
// path (cerberus issue #2770): tagsCatalogEligible / tagValuesCatalogEligible
// gate when a /search/tags or /search/tag/{name}/values request may be
// served from the tempo_tag_catalog refreshable-MV catalog
// (internal/schema/ddl's renderTempoTagCatalogTable / View) instead of the
// live per-scope attribute-map scan, and tagsFromCatalog /
// tagValuesFromCatalog run that read, returning ok=true only on a genuine
// hit. Both mirror internal/api/loki/detected_labels.go's
// labelCatalogEligible / detectedLabelsFromCatalog exactly: a query error,
// an UNKNOWN_TABLE (feature off or DDL not yet applied), or an empty
// result all degrade to ok=false, which sends the caller straight through
// to the SAME live path every other request already takes — that path is
// untouched and permanent, never a transitional shim.
//
// ELIGIBILITY is narrower than Loki's own selector-less rule, because a
// tag-discovery response here is a LIST of keys/values (what the catalog
// approximates over its own trailing window and top-N cap), not a
// cardinality COUNT (which tolerates a window mismatch — see
// FeatureLokiCatalogMV's doc). Eligible only when ALL of:
//
//   - the request carries no `q=<TraceQL>` narrowing filter (the resolved
//     chsql.Frag from tagQueryFilter is nil) — the catalog has no way to
//     answer "values on spans matching this predicate" without evaluating
//     the predicate per row, exactly the scan this feature exists to
//     avoid. Confirmed, not assumed: search_tags_filter.go's
//     tagQueryFilter only ever returns a non-nil Frag when `q` is both
//     present AND lowers to a real (non-trivially-true) span-row
//     predicate, so this check alone also covers the "q=" and
//     unparseable-q cases, which resolve to a nil filter already.
//   - the request is windowless — no `start`/`end` query parameters at
//     all, captured BEFORE BoundDiscoveryWindow defaults them to
//     [now-DefaultSearchLookback, now]. The catalog's own aggregation
//     window (internal/schema/ddl's tempoTagCatalogWindowHours) is
//     DELIBERATELY sized to match DefaultSearchLookback exactly, so a
//     true datasource-open-probe request and the catalog's maintained
//     window describe the SAME trailing hour; an explicit wide
//     historical window is left on the live path rather than silently
//     answered from a narrower window than the caller asked for.
//   - (tags only) the requested `?scope=` is one the catalog covers —
//     "none" (every covered bucket), "resource", "span", "event", or
//     "link" (cerberus issue #2850 widened the catalog to the latter
//     two — see internal/schema/ddl's SCOPE COVERAGE doc for the
//     measured cost that justified it). Every other scope value
//     (instrumentation/intrinsic/trace) stays live unconditionally.
//     "none" additionally requires scopeAttrsConfigured == false — see
//     that parameter's doc below.
//   - (tag values only) the resolved tag name is NOT an intrinsic —
//     intrinsics read a dedicated column directly (cheap already, never
//     an attribute-map explosion), so the catalog has nothing to offer
//     there.

// tagsCatalogEligible reports whether a /search/tags (or
// /api/v2/search/tags) request may be served from the catalog.
//
// scopeAttrsConfigured is h.Schema.ScopeAttributesColumn != "": whether
// this deployment's schema carries an instrumentation-scope attribute
// map. The catalog never covers that bucket (see internal/schema/ddl's
// SCOPE COVERAGE doc), so a `scope=none` catalog hit is a faithful
// substitute for the live "none" answer (collectAttributeTagScopes /
// attributeTagScopes) ONLY when the schema has no instrumentation bucket
// for the live path to include either. On a custom schema that DOES
// populate ScopeAttributesColumn, a catalog-served "none" would silently
// omit that bucket, so "none" stays off the fast path there — every
// default-schema deployment (scopeAttrsConfigured == false) is
// unaffected. A scoped request ("resource"/"span"/"event"/"link") never
// touches the instrumentation bucket in the first place, so this
// parameter only gates "none".
func tagsCatalogEligible(scope string, filter chsql.Frag, windowless, scopeAttrsConfigured bool) bool {
	if filter != nil || !windowless {
		return false
	}
	switch scope {
	case tagScopeResource, tagScopeSpan, tagScopeEvent, tagScopeLink:
		return true
	case tagScopeNone:
		return !scopeAttrsConfigured
	default:
		return false
	}
}

// tagValuesCatalogEligible reports whether a
// /search/tag/{name}/values (or V2) request may be served from the
// catalog.
func tagValuesCatalogEligible(resolved resolvedTagName, filter chsql.Frag, windowless bool) bool {
	if filter != nil || !windowless {
		return false
	}
	return !resolved.IsIntrinsic
}

// catalogScopesFor maps the `?scope=` query parameter to the catalog
// Scope value(s) a /search/tags catalog read should fetch: every covered
// bucket for "none" (the default / unscoped request), one bucket for an
// explicit "resource", "span", "event", or "link". Only called after
// tagsCatalogEligible has already rejected every other scope value.
func catalogScopesFor(scope string) []string {
	switch scope {
	case tagScopeResource:
		return []string{schema.TagCatalogScopeResource}
	case tagScopeSpan:
		return []string{schema.TagCatalogScopeSpan}
	case tagScopeEvent:
		return []string{schema.TagCatalogScopeEvent}
	case tagScopeLink:
		return []string{schema.TagCatalogScopeLink}
	default: // tagScopeNone
		return []string{
			schema.TagCatalogScopeResource, schema.TagCatalogScopeSpan,
			schema.TagCatalogScopeEvent, schema.TagCatalogScopeLink,
		}
	}
}

// catalogScopeForMapScope maps a resolved tag-VALUES lookup's attrMapScope
// to the single catalog Scope value to filter by, or ok=false for
// attrMapScopeAny — the auto-scope form, which unions the resource AND
// span buckets ONLY (never event/link — those require an explicit
// event./link. prefix, see resolveTagName) via the explicit
// TagCatalogScopeResource/Span IN-list buildTagCatalogValuesSQL applies
// when ok is false.
func catalogScopeForMapScope(ms attrMapScope) (string, bool) {
	switch ms {
	case attrMapScopeResource:
		return schema.TagCatalogScopeResource, true
	case attrMapScopeSpan:
		return schema.TagCatalogScopeSpan, true
	case attrMapScopeEvent:
		return schema.TagCatalogScopeEvent, true
	case attrMapScopeLink:
		return schema.TagCatalogScopeLink, true
	default:
		return "", false
	}
}

// tagsFromCatalog attempts the catalog read for every scope bucket the
// requested `?scope=` wants (see catalogScopesFor). ok=true only when at
// least one bucket returned at least one key; a query error on ANY
// requested bucket degrades the WHOLE attempt to ok=false (mirroring
// Loki's binary catalog-hit-or-full-fallback contract) rather than
// serving a partial mix of catalog and live data.
func (h *Handler) tagsFromCatalog(ctx context.Context, scope string) ([]TagScope, bool) {
	var out []TagScope
	for _, catScope := range catalogScopesFor(scope) {
		sqlStr, args := buildTagCatalogKeysSQL(catScope)
		keys, err := h.Client.QueryStrings(ctx, sqlStr, args...)
		if err != nil {
			h.Logger.Debug("cerberus tempo tag-catalog key lookup failed; falling back to live scan",
				"scope", catScope, "err", err)
			return nil, false
		}
		if keys = sortedUnique(keys); len(keys) == 0 {
			continue
		}
		out = append(out, TagScope{Name: catScope, Tags: keys})
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// tagValuesFromCatalog attempts the catalog read for a resolved dynamic
// attribute tag name (never called for an intrinsic — see
// tagValuesCatalogEligible). ok=true only on a genuine hit — at least one
// value came back.
func (h *Handler) tagValuesFromCatalog(ctx context.Context, resolved resolvedTagName) ([]string, bool) {
	sqlStr, args := buildTagCatalogValuesSQL(resolved.Key, resolved.MapScope)
	values, err := h.Client.QueryStrings(ctx, sqlStr, args...)
	if err != nil {
		h.Logger.Debug("cerberus tempo tag-value catalog lookup failed; falling back to live scan",
			"key", resolved.Key, "err", err)
		return nil, false
	}
	if values = sortedUnique(values); len(values) == 0 {
		return nil, false
	}
	return values, true
}

// buildTagCatalogKeysSQL renders:
//
//	SELECT `TagKey` FROM `tempo_tag_catalog` WHERE `Scope` = ? GROUP BY `TagKey`
//
// GROUP BY is required even though (Scope, TagKey) is the table's whole
// ORDER BY, because AggregatingMergeTree only guarantees per-key states
// are EVENTUALLY merged by background merges, not that every part has
// already merged into one row per key at read time — mirrors
// internal/api/loki's buildLabelCatalogSQL doc comment.
func buildTagCatalogKeysSQL(scope string) (string, []any) {
	sb := chsql.NewQuery().
		Select(chsql.Col(schema.TagCatalogKeyColumn)).
		From(chsql.Col(schema.TagCatalogTable)).
		Where(chsql.Eq(chsql.Col(schema.TagCatalogScopeColumn), chsql.Lit(scope))).
		GroupBy(chsql.Col(schema.TagCatalogKeyColumn))
	return sb.Build()
}

// buildTagCatalogValuesSQL renders:
//
//	SELECT arrayJoin(topKMerge(50)(`TopValuesState`)) AS `value`
//	FROM `tempo_tag_catalog` WHERE `TagKey` = ? [AND `Scope` = ?]
//
// topKMerge finalises the per-(scope, key) topKState sketch
// internal/schema/ddl.renderTempoTagCatalogView's refresh maintains into
// one top-N array; arrayJoin unrolls that array into one row per value,
// the shape chclient.QueryStrings (a single-string-column scan) expects.
//
// A single-scope mapScope (resource./span./event./link. forms) adds an
// exact Scope predicate; the auto-scope "any" form (catalogScopeForMapScope's
// ok=false) adds an explicit `Scope IN ('resource', 'span')` predicate
// instead of omitting the Scope filter — auto-scope has only ever meant
// "resource or span" (see resolveTagName: event./link. require an
// explicit prefix, so attr.Scope only resolves to attrMapScopeAny for a
// bare or dotted identifier), and since cerberus issue #2850 the catalog
// ALSO carries event/link rows, so an unfiltered read here would widen
// auto-scope's merge to include them too — a genuine behaviour change
// the explicit IN prevents. The result is the catalog's analogue of
// attrValueArrayJoinFrag's live-path union of SpanAttributes and
// ResourceAttributes ONLY, pre-aggregated instead of per-row.
func buildTagCatalogValuesSQL(key string, mapScope attrMapScope) (string, []any) {
	sb := chsql.NewQuery().
		Select(chsql.As(chsql.Call("arrayJoin", tagCatalogTopValuesMergeFrag(chsql.Col(schema.TagCatalogTopValuesStateColumn))), "value")).
		From(chsql.Col(schema.TagCatalogTable)).
		Where(chsql.Eq(chsql.Col(schema.TagCatalogKeyColumn), chsql.Lit(key)))
	if catScope, ok := catalogScopeForMapScope(mapScope); ok {
		sb.Where(chsql.Eq(chsql.Col(schema.TagCatalogScopeColumn), chsql.Lit(catScope)))
	} else {
		sb.Where(chsql.In(
			chsql.Col(schema.TagCatalogScopeColumn),
			chsql.Lit(schema.TagCatalogScopeResource), chsql.Lit(schema.TagCatalogScopeSpan),
		))
	}
	return sb.Build()
}

// tagCatalogTopValuesMergeFrag renders `topKMerge(N)(<col>)` — CH's
// parameterised-aggregate shape (`name(params...)(args...)`) via
// Builder.ParamAgg, adapting the typed chsql.Frag operands to the plain
// func(*chsql.Builder) slices ParamAgg takes (Frag's underlying type is
// already func(*Builder), so each element assignment is a type
// conversion, not a rendering step). N is schema.TagCatalogTopValuesLimit
// — see that constant's doc for why this read side and the DDL write
// side share it rather than each carrying its own literal.
func tagCatalogTopValuesMergeFrag(col chsql.Frag) chsql.Frag {
	limit := chsql.InlineLit(int64(schema.TagCatalogTopValuesLimit))
	return func(b *chsql.Builder) {
		b.ParamAgg("topKMerge", []func(b *chsql.Builder){limit}, []func(b *chsql.Builder){col})
	}
}
