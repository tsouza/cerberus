package tempo

import (
	"context"
	"fmt"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
)

// This file implements the optional `q=<TraceQL>` narrowing filter on the
// tag discovery routes — both the tag-NAME pair (`/api/search/tags`,
// `/api/v2/search/tags`) and the tag-VALUE pair
// (`/api/search/tag/{name}/values`, `/api/v2/search/tag/{name}/values`) —
// and owns the one respect in which each pair's two halves differ: `q`
// belongs to V2 and not to V1.
//
// Grafana's tag autocomplete sends `q` as soon as the user has typed any
// part of a query: the point is to offer the keys that exist on the spans
// the query already selects rather than every key in the window. Without
// it cerberus answered the same unfiltered key set for every `q`, so the
// autocomplete list was wider than the data it was completing against
// (#1820).
//
// The value routes are where that gap bites hardest, and they were left
// out of #1820's fix (#1932). They back the VALUE half of the same
// dropdown — the completion the user is mid-way through typing — so the
// unfiltered answer there is both the largest set and the least relevant
// one: every value the key takes anywhere in the window, rather than the
// values it takes on the spans the query already selects.
//
// That is a V2 parameter on both pairs. On the name side, upstream
// Tempo's API reference lists `q` under "Search tags V2" only, and the V1
// name route drops it on the floor rather than
// rejecting it: modules/livestore/live_store.go's SearchTags forwards
// nothing but req.Scope into instance.SearchTagsV2, so the empty query
// yields no condition groups and every block answers with the unfiltered
// b.SearchTags. The same call also zeroes Start/End, and includeBlock
// treats a zero window as "every block". A V1 request therefore gets the
// whole window's key set back, with a 200, however narrow — or however
// malformed — its `q` was. Matching that is why V1 here neither parses nor
// validates the parameter.
//
// The narrowing is a predicate over the SPAN ROW, which is what makes one
// filter serve every discovery lookup. Each tag-name scope bucket already
// runs `SELECT DISTINCT <keys-of-column> FROM <spans> WHERE <window>`, and
// each tag-value lookup runs the same shape over one key's values, so `q`
// contributes one more conjunct to that WHERE in both cases. On the
// dynamic-attribute value lookup — whose outer SELECT sees only the
// exploded value column — the conjunct joins the INNER query, the one
// that still has the span row in scope. Nothing else about any lookup
// changes, so a request without `q` renders byte-identical SQL.

// TagsRoute names which of the four tag-discovery routes a lookup
// answers. The routes differ in exactly one respect — whether `q` is
// part of their contract — and TagsRoute.narrowingQuery is the single
// place that difference is expressed, so the HTTP and gRPC surfaces
// cannot drift on it, and neither can the name and value halves.
// Exported because the gRPC tag services select the route too.
type TagsRoute uint8

const (
	// TagsRouteV1 is `/api/search/tags` and its gRPC twin SearchTags,
	// whose contract carries no narrowing query.
	TagsRouteV1 TagsRoute = iota
	// TagsRouteV2 is `/api/v2/search/tags` and its gRPC twin
	// SearchTagsV2, the route that documents and honours `q`.
	TagsRouteV2
	// TagValuesRouteV1 is `/api/search/tag/{name}/values` and its gRPC
	// twin SearchTagValues. Like TagsRouteV1 it carries no narrowing
	// query.
	TagValuesRouteV1
	// TagValuesRouteV2 is `/api/v2/search/tag/{name}/values` and its
	// gRPC twin SearchTagValuesV2 — the value-side route that documents
	// and honours `q`.
	TagValuesRouteV2
)

// narrowingQuery returns the TraceQL query this route filters its
// lookups by, given whatever the client sent. On the two V2 routes that
// is the client's query verbatim; on the two V1 routes it is always
// empty, because `q` is not part of the V1 contract and upstream ignores
// it there.
//
// The value side is asymmetric for the same reason the name side is, and
// upstream's own code is where that shows: `q` is parsed for both value
// routes by one shared parser (pkg/api/search_tags.go's
// parseSearchTagValuesRequest — its enforceTraceQL flag validates the
// TAG NAME, not the query) and even re-emitted onto the sub-requests, so
// it physically reaches the queriers on V1. It then dies at every leaf.
// The V1 executors take a bare tag name and have nowhere to put a
// filter — modules/livestore/instance_search.go's SearchTagValues calls
// block.SearchTagValues(ctx, tagName, …), and the backend-block leg in
// modules/querier/querier.go reaches tempodb's block.SearchTagValues the
// same way — while only the V2 executors run
// traceql.ExtractConditionGroups(req.Query, …) and take the filtered
// ExecuteTagValues path. Upstream's API reference documents `q` under
// "Search tag values V2" only, and matching that is why V1 here never
// reaches the parser.
func (rt TagsRoute) narrowingQuery(raw string) string {
	if rt == TagsRouteV2 || rt == TagValuesRouteV2 {
		return raw
	}
	return ""
}

// tagQueryFilter turns the raw `q` query parameter into the extra WHERE
// conjunct a discovery lookup runs under — the per-scope key lookups on
// the name routes, the per-key value lookups on the value routes. An
// empty `q` — and any `q` at all on a V1 route, which does not take
// one — yields a nil Frag, and a nil Frag adds no clause, so those
// requests are unchanged down to the
// rendered SQL. Routing the parameter through TagsRoute here rather than at
// the call sites is what keeps a V1 caller from ever reaching the parser:
// upstream answers a malformed V1 `q` with a 200 and the unfiltered key
// set, and a route that parsed it would answer 400 instead.
//
// V2 tag discovery deliberately follows Tempo's backwards-compatible
// leniency: incomplete matchers retain their valid conjuncts, while a query
// that still cannot parse, lower, or reduce to one span-row predicate falls
// back to the unfiltered set. Grafana sends those half-typed queries during
// autocomplete, and upstream treats an extraction failure as no filter rather
// than a client error.
func (h *Handler) tagQueryFilter(ctx context.Context, route TagsRoute, raw string, start, end time.Time) (chsql.Frag, error) {
	raw = route.narrowingQuery(raw)
	if raw == "" {
		return nil, nil
	}
	ctx = traceql_lower.WithSearchWindow(ctx, start, end)
	expr, err := parseLenientExpr(ctx, raw)
	if err != nil {
		return nil, nil
	}
	plan, err := traceql_lower.Lower(ctx, expr, h.Schema)
	if err != nil {
		return nil, nil
	}
	pred, ok := spanRowPredicate(plan)
	if !ok {
		return nil, nil
	}
	if literal, ok := pred.(*chplan.LitBool); ok && literal.V {
		return nil, nil
	}
	return spanPredicateFrag(pred)
}

// spanRowPredicate extracts the span-row predicate from a lowered plain
// search plan. The plain-search lowering is `Filter(Scan)`, optionally
// under a `Project` when the query carries a `select(...)` clause — the
// projection names which columns come back, which a key lookup does not
// care about, so it is peeled.
//
// Anything else reports false: its matching spans are not describable by
// a predicate over one row.
func spanRowPredicate(plan chplan.Node) (chplan.Expr, bool) {
	for {
		switch n := plan.(type) {
		case *chplan.Project:
			plan = n.Input
		case *chplan.Filter:
			if _, ok := n.Input.(*chplan.Scan); !ok {
				return nil, false
			}
			return n.Predicate, true
		default:
			return nil, false
		}
	}
}

// spanPredicateFrag adapts a lowered chplan.Expr to the typed chsql.Frag a
// QueryBuilder slot takes, so the predicate composes with the window
// conjuncts through the same API rather than being rendered to a string
// and spliced.
//
// The render happens twice on purpose. QueryBuilder.Build drops the
// builder's first-error state (subquerySQL, its error-propagating
// counterpart, is unexported), so a predicate the emitter cannot render
// would otherwise reach ClickHouse as truncated SQL. Rendering it once
// into a throwaway Builder turns that into an error the handler can answer
// with, and guarantees the Frag below cannot be the one that fails.
func spanPredicateFrag(pred chplan.Expr) (chsql.Frag, error) {
	if err := chsql.NewBuilder().Expr(pred); err != nil {
		return nil, fmt.Errorf("%w: %w", errLowerStage, err)
	}
	return func(b *chsql.Builder) { _ = b.Expr(pred) }, nil
}
