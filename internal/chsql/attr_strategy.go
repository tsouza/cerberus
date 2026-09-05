package chsql

import (
	"context"

	"github.com/tsouza/cerberus/internal/chplan"
)

// AttrStrategy, AttrStrategies, AttrStrategyMap and AttrStrategyJSON are
// aliases of the chplan types of the same name — declared there, not here,
// because internal/logql's Lang.EmitAttrStrategies() must return this type
// to satisfy the shared Lang contract engine.attrStrategier reads, and
// logql is architecturally forbidden from depending on chsql
// (.go-arch-lint.yml); chplan is the one package both logql and chsql
// already depend on. See chplan.AttrStrategy's own doc for the full
// design (the missing-vs-empty semantics decision, the per-signal scoping
// rationale, cerberus issue #2777). Aliasing here means every existing
// chsql.AttrStrategy* reference keeps compiling unchanged.
type (
	AttrStrategy   = chplan.AttrStrategy
	AttrStrategies = chplan.AttrStrategies
)

const (
	AttrStrategyMap  = chplan.AttrStrategyMap
	AttrStrategyJSON = chplan.AttrStrategyJSON
)

// attrStrategiesCtxKey is the context key for WithAttrStrategies /
// attrStrategiesFromCtx. Mirrors the unexported-key pattern
// WithSpansTable/spansTableFromCtx (scan_resource_bound.go) and
// WithDeltaPrefixLookback/deltaPrefixLookbackFromCtx (range_window.go)
// already use for per-request emit-time configuration.
type attrStrategiesCtxKey struct{}

// WithAttrStrategies returns a context carrying the resolved
// AttrStrategies chsql.Emit renders the plan's attribute-map accesses
// against. internal/engine calls this once per request, scoped to the
// signal actually being queried (see AttrStrategies's doc) — a caller
// that never calls it (every non-engine chsql.Emit call, every direct
// QueryBuilder use) gets the nil default, i.e. AttrStrategyMap
// everywhere.
func WithAttrStrategies(ctx context.Context, s AttrStrategies) context.Context {
	return context.WithValue(ctx, attrStrategiesCtxKey{}, s)
}

// attrStrategiesFromCtx reads the AttrStrategies WithAttrStrategies set,
// or nil (AttrStrategyMap everywhere) when it was never called.
func attrStrategiesFromCtx(ctx context.Context) AttrStrategies {
	s, _ := ctx.Value(attrStrategiesCtxKey{}).(AttrStrategies)
	return s
}
