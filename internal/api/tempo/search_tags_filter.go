package tempo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
)

// This file implements the optional `q=<TraceQL>` narrowing filter on the
// tag-NAME discovery routes, and owns the one respect in which those two
// routes differ: `q` belongs to V2 (`/api/v2/search/tags`) and not to V1
// (`/api/search/tags`).
//
// Grafana's tag autocomplete sends `q` as soon as the user has typed any
// part of a query: the point is to offer the keys that exist on the spans
// the query already selects rather than every key in the window. Without
// it cerberus answered the same unfiltered key set for every `q`, so the
// autocomplete list was wider than the data it was completing against
// (#1820).
//
// That is a V2 parameter. Upstream Tempo's API reference lists `q` under
// "Search tags V2" only, and its V1 route drops it on the floor rather
// than rejecting it: modules/livestore/live_store.go's SearchTags forwards
// nothing but req.Scope into instance.SearchTagsV2, so the empty query
// yields no condition groups and every block answers with the unfiltered
// b.SearchTags. The same call also zeroes Start/End, and includeBlock
// treats a zero window as "every block". A V1 request therefore gets the
// whole window's key set back, with a 200, however narrow — or however
// malformed — its `q` was. Matching that is why V1 here neither parses nor
// validates the parameter.
//
// The narrowing is a predicate over the SPAN ROW, which is what makes it
// composable with the per-scope key projections: each scope bucket already
// runs `SELECT DISTINCT <keys-of-column> FROM <spans> WHERE <window>`, and
// `q` contributes one more conjunct to that WHERE. Nothing else about the
// lookup changes, so a request without `q` renders byte-identical SQL.

// TagsRoute names which of the two tag-name routes a lookup answers.
// The routes differ in exactly one respect — whether `q` is part of
// their contract — and TagsRoute.narrowingQuery is the single place that
// difference is expressed, so the HTTP and gRPC surfaces cannot drift on
// it. Exported because the gRPC tag services select the route too.
type TagsRoute uint8

const (
	// TagsRouteV1 is `/api/search/tags` and its gRPC twin SearchTags,
	// whose contract carries no narrowing query.
	TagsRouteV1 TagsRoute = iota
	// TagsRouteV2 is `/api/v2/search/tags` and its gRPC twin
	// SearchTagsV2, the route that documents and honours `q`.
	TagsRouteV2
)

// narrowingQuery returns the TraceQL query this route filters its key
// lookups by, given whatever the client sent. On V2 that is the client's
// query verbatim; on V1 it is always empty, because `q` is not part of
// the V1 contract and upstream ignores it there.
func (rt TagsRoute) narrowingQuery(raw string) string {
	if rt == TagsRouteV2 {
		return raw
	}
	return ""
}

// errTagQueryShape is the sentinel for "this TraceQL query is well-formed
// but does not reduce to a predicate over a single span row". Structural
// (`{...} >> {...}`), multi-spanset and metrics-pipeline queries lower to
// plan shapes whose matching spans are defined by a join or an aggregate,
// not by a row predicate, so there is no conjunct to push into the key
// lookups. Answering the unfiltered key set for those would be a silently
// wrong answer — wider than the query asked for — so they are rejected.
var errTagQueryShape = errors.New("traceql: tag-name filtering needs a query that selects spans by a row predicate")

// tagQueryFilter turns the raw `q` query parameter into the extra WHERE
// conjunct the per-scope key lookups run under. An empty `q` — and any `q`
// at all on the V1 route, which does not take one — yields a nil Frag, and
// a nil Frag adds no clause, so those requests are unchanged down to the
// rendered SQL. Routing the parameter through TagsRoute here rather than at
// the call sites is what keeps a V1 caller from ever reaching the parser:
// upstream answers a malformed V1 `q` with a 200 and the unfiltered key
// set, and a route that parsed it would answer 400 instead.
//
// Parsing and lowering go through the same parseExpr + traceql.Lower pair
// /api/search and /api/metrics/query_range use, including the request
// window on the context, so a query means the same thing on this route as
// on the ones that execute it.
func (h *Handler) tagQueryFilter(ctx context.Context, route TagsRoute, raw string, start, end time.Time) (chsql.Frag, error) {
	raw = route.narrowingQuery(raw)
	if raw == "" {
		return nil, nil
	}
	ctx = traceql_lower.WithSearchWindow(ctx, start, end)
	expr, err := parseExpr(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errParseStage, err)
	}
	plan, err := traceql_lower.Lower(ctx, expr, h.Schema)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errLowerStage, err)
	}
	pred, ok := spanRowPredicate(plan)
	if !ok {
		return nil, fmt.Errorf("%w: %w (%q)", errLowerStage, errTagQueryShape, raw)
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
