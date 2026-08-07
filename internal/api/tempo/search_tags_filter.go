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
// tag-NAME discovery routes (`/api/search/tags` and `/api/v2/search/tags`).
//
// Upstream Tempo accepts `q` on both, and Grafana's tag autocomplete sends
// it as soon as the user has typed any part of a query: the point is to
// offer the keys that exist on the spans the query already selects rather
// than every key in the window. Without it cerberus answered the same
// unfiltered key set for every `q`, so the autocomplete list was wider than
// the data it was completing against (#1820).
//
// The narrowing is a predicate over the SPAN ROW, which is what makes it
// composable with the per-scope key projections: each scope bucket already
// runs `SELECT DISTINCT <keys-of-column> FROM <spans> WHERE <window>`, and
// `q` contributes one more conjunct to that WHERE. Nothing else about the
// lookup changes, so a request without `q` renders byte-identical SQL.

// errTagQueryShape is the sentinel for "this TraceQL query is well-formed
// but does not reduce to a predicate over a single span row". Structural
// (`{...} >> {...}`), multi-spanset and metrics-pipeline queries lower to
// plan shapes whose matching spans are defined by a join or an aggregate,
// not by a row predicate, so there is no conjunct to push into the key
// lookups. Answering the unfiltered key set for those would be a silently
// wrong answer — wider than the query asked for — so they are rejected.
var errTagQueryShape = errors.New("traceql: tag-name filtering needs a query that selects spans by a row predicate")

// tagQueryFilter turns the raw `q` query parameter into the extra WHERE
// conjunct the per-scope key lookups run under. An empty `q` yields a nil
// Frag, and a nil Frag adds no clause — the no-`q` request is unchanged
// down to the rendered SQL.
//
// Parsing and lowering go through the same parseExpr + traceql.Lower pair
// /api/search and /api/metrics/query_range use, including the request
// window on the context, so a query means the same thing on this route as
// on the ones that execute it.
func (h *Handler) tagQueryFilter(ctx context.Context, raw string, start, end time.Time) (chsql.Frag, error) {
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
