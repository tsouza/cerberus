package tempo

import (
	"context"
	"fmt"

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
//
// attrMapScopeInstrumentation stays off the fast path unconditionally
// (cerberus issue #3010): the catalog MV never carries an
// instrumentation-scope arm at all (internal/schema/ddl's SCOPE
// COVERAGE doc — unlike tagsCatalogEligible's `scope=none`, which is
// merely narrowed when the schema configures ScopeAttributesColumn, an
// explicit instrumentation-scope tag-VALUES request has no partial-
// coverage case to preserve; the catalog has nothing for it whether or
// not the schema carries a column). This exclusion is a cheap fast-fail
// that skips the catalog attempt (and its SQL round trip) entirely; it is
// no longer the SOLE guard against a wrong catalog read for this scope —
// catalogScopeForMapScope (cerberus issue #3019) independently refuses to
// build SQL for attrMapScopeInstrumentation too, so removing this
// exclusion would cost a wasted round trip, not a wrong answer. Belt AND
// suspenders, not a mask over a gap: see catalogScopeForMapScope's doc for
// the full history.
func tagValuesCatalogEligible(resolved resolvedTagName, filter chsql.Frag, windowless bool) bool {
	if filter != nil || !windowless {
		return false
	}
	if resolved.MapScope == attrMapScopeInstrumentation {
		return false
	}
	return !resolved.IsIntrinsic
}

// allCatalogCoveredTagScopes is the canonical enumeration of every `?scope=`
// query-param value (see the tagScope* constants in search_tags.go) the tag
// catalog actually carries an arm for — cerberus issue #3019's counterpart
// to allAttrMapScopes for this package's OTHER scope vocabulary (the
// request-facing scope string, not the internal attrMapScope enum
// buildAttributeValuesSQL dispatches on). "none" is deliberately not a
// member: it is the umbrella that catalogScopesFor expands to every entry
// here, not a catalog arm of its own. tagsCatalogEligible and
// catalogScopesFor are both checked against it —
// TestCatalogScopesFor_CoversEveryCatalogScope and
// TestTagsCatalogEligible_CoversEveryCatalogScope in tag_catalog_test.go.
var allCatalogCoveredTagScopes = []string{
	tagScopeResource,
	tagScopeSpan,
	tagScopeEvent,
	tagScopeLink,
}

// catalogScopesFor maps the `?scope=` query parameter to the catalog
// Scope value(s) a /search/tags catalog read should fetch: every covered
// bucket for "none" (the default / unscoped request), one bucket for an
// explicit "resource", "span", "event", or "link". Only called after
// tagsCatalogEligible has already rejected every other scope value, so the
// panic default below is a real invariant, not a defensive nicety: it fires
// only if that upstream guarantee is ever broken, naming the unexpected
// scope instead of this function silently guessing "none" the way an
// unlabelled `default:` used to (cerberus issue #3019).
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
	case tagScopeNone:
		return []string{
			schema.TagCatalogScopeResource, schema.TagCatalogScopeSpan,
			schema.TagCatalogScopeEvent, schema.TagCatalogScopeLink,
		}
	default:
		panic(fmt.Sprintf("cerberus: catalogScopesFor: unhandled scope %q — tagsCatalogEligible "+
			"should have rejected it before this function was ever called; extend this switch in "+
			"tag_catalog.go if a new catalog-covered scope was added", scope))
	}
}

// catalogScopeMode is catalogScopeForMapScope's classification of how (or
// whether) buildTagCatalogValuesSQL should filter the catalog's Scope
// column for one attrMapScope value.
type catalogScopeMode int

const (
	// catalogScopeSingle: filter to exactly the Scope value
	// catalogScopeForMapScope also returns.
	catalogScopeSingle catalogScopeMode = iota
	// catalogScopeUnion: attrMapScopeAny — filter to
	// `Scope IN ('resource', 'span')`. Auto-scope has only ever meant
	// "resource or span" (see resolveTagName: event./link. require an
	// explicit prefix), so an explicit IN-list is required rather than
	// omitting the Scope filter — cerberus issue #2850 widened the catalog
	// to also carry event/link rows, which an unfiltered read would then
	// wrongly merge in.
	catalogScopeUnion
	// catalogScopeUncovered: the catalog carries NO arm for this
	// attrMapScope at all (attrMapScopeInstrumentation —
	// internal/schema/ddl's SCOPE COVERAGE doc). buildTagCatalogValuesSQL
	// refuses to build SQL at all for this mode.
	catalogScopeUncovered
)

// catalogScopeForMapScope maps a resolved tag-VALUES lookup's attrMapScope
// to how buildTagCatalogValuesSQL should filter the catalog's Scope column.
// Exhaustive over allAttrMapScopes by construction (cerberus issue #3019;
// TestCatalogScopeForMapScope_CoversEveryScope pins the case set against
// that canonical list) — the panic default fires for an attrMapScope this
// function does not recognise, rather than an unlabelled `default:` that
// used to mean "treat as attrMapScopeAny" for BOTH the real Any case and
// any value nobody had made a decision for yet.
//
// attrMapScopeInstrumentation reports catalogScopeUncovered: this function
// is now the SOLE authority for that decision. buildTagCatalogValuesSQL
// asks it and refuses the catalog read itself on catalogScopeUncovered,
// rather than depending on tagValuesCatalogEligible having already excluded
// the scope upstream. Before this change, two independently-incomplete
// switches (this one's unlabelled default, and tagValuesCatalogEligible's
// instrumentation check) composed correctly only because of THAT call
// order — nothing enforced it, and catalogScopeForMapScope on its own would
// have silently mis-served an instrumentation-scope request as
// attrMapScopeAny's resource+span union had the caller ever changed. See
// tagValuesCatalogEligible's own doc: it keeps its exclusion too, now as a
// cheap fast-fail that skips the round trip, not as the only safeguard.
func catalogScopeForMapScope(ms attrMapScope) (scope string, mode catalogScopeMode) {
	switch ms {
	case attrMapScopeResource:
		return schema.TagCatalogScopeResource, catalogScopeSingle
	case attrMapScopeSpan:
		return schema.TagCatalogScopeSpan, catalogScopeSingle
	case attrMapScopeEvent:
		return schema.TagCatalogScopeEvent, catalogScopeSingle
	case attrMapScopeLink:
		return schema.TagCatalogScopeLink, catalogScopeSingle
	case attrMapScopeAny:
		return "", catalogScopeUnion
	case attrMapScopeInstrumentation:
		return "", catalogScopeUncovered
	default:
		panic(fmt.Sprintf("cerberus: catalogScopeForMapScope: unhandled attrMapScope %d — extend "+
			"the switch in tag_catalog.go", int(ms)))
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
// value came back. Also false, before any query even runs, when
// buildTagCatalogValuesSQL itself refuses the scope (catalogScopeUncovered
// — see catalogScopeForMapScope's doc).
func (h *Handler) tagValuesFromCatalog(ctx context.Context, resolved resolvedTagName) ([]string, bool) {
	sqlStr, args, ok := buildTagCatalogValuesSQL(resolved.Key, resolved.MapScope)
	if !ok {
		return nil, false
	}
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
// exact Scope predicate (catalogScopeSingle); the auto-scope "any" form
// (catalogScopeUnion) adds an explicit `Scope IN ('resource', 'span')`
// predicate instead of omitting the Scope filter — auto-scope has only
// ever meant "resource or span" (see resolveTagName: event./link. require
// an explicit prefix, so attr.Scope only resolves to attrMapScopeAny for a
// bare or dotted identifier), and since cerberus issue #2850 the catalog
// ALSO carries event/link rows, so an unfiltered read here would widen
// auto-scope's merge to include them too — a genuine behaviour change the
// explicit IN prevents. The result is the catalog's analogue of
// attrValueArrayJoinFrag's live-path union of SpanAttributes and
// ResourceAttributes ONLY, pre-aggregated instead of per-row.
//
// ok is false only for catalogScopeUncovered (attrMapScopeInstrumentation,
// cerberus issue #3019) — the catalog has no arm to filter by, so no SQL
// this function could build would be meaningful. The returned string/args
// are the zero value in that case; the caller (tagValuesFromCatalog) must
// not run them, and doesn't.
func buildTagCatalogValuesSQL(key string, mapScope attrMapScope) (string, []any, bool) {
	catScope, mode := catalogScopeForMapScope(mapScope)
	if mode == catalogScopeUncovered {
		return "", nil, false
	}
	sb := chsql.NewQuery().
		Select(chsql.As(chsql.Call("arrayJoin", tagCatalogTopValuesMergeFrag(chsql.Col(schema.TagCatalogTopValuesStateColumn))), "value")).
		From(chsql.Col(schema.TagCatalogTable)).
		Where(chsql.Eq(chsql.Col(schema.TagCatalogKeyColumn), chsql.Lit(key)))
	switch mode {
	case catalogScopeSingle:
		sb.Where(chsql.Eq(chsql.Col(schema.TagCatalogScopeColumn), chsql.Lit(catScope)))
	case catalogScopeUnion:
		sb.Where(chsql.In(
			chsql.Col(schema.TagCatalogScopeColumn),
			chsql.Lit(schema.TagCatalogScopeResource), chsql.Lit(schema.TagCatalogScopeSpan),
		))
	}
	sqlStr, args := sb.Build()
	return sqlStr, args, true
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
