package chsql

import (
	"context"
	"sync/atomic"

	"github.com/tsouza/cerberus/internal/chplan"
)

// SchemaStrategies is an alias of chplan.SchemaStrategies, declared there so
// internal/logql can report it without depending on chsql. Every Builder
// renders under one (Builder.strategies): a statement's attribute-map
// accesses read its Attrs, its metrics timestamp reads its MetricsTimestamp
// (metrics_timestamp.go).
//
// It reaches a statement one of three ways, all carrying the same value:
//
//   - chsql.Emit seeds it from the context (WithSchemaStrategies), which
//     internal/engine sets once per request from the head's Lang, and
//     stamps it on every QueryBuilder the plan renders through.
//   - A head that builds a QueryBuilder itself sets it with
//     QueryBuilder.WithSchemaStrategies.
//   - A Frag built outside any QueryBuilder renders under it with
//     UnderSchemaStrategies.
//
// A QueryBuilder that carries none renders under the strategies of the
// statement it is embedded in (Subquery, Spliced, QueryBuilder.Frag), or
// under the zero value when rendered on its own.
type SchemaStrategies = chplan.SchemaStrategies

// schemaStrategiesCtxKey is the context key for WithSchemaStrategies /
// schemaStrategiesFromCtx.
type schemaStrategiesCtxKey struct{}

// WithSchemaStrategies returns a context carrying the SchemaStrategies
// chsql.Emit renders the plan under. A caller that never calls it gets the
// zero value.
func WithSchemaStrategies(ctx context.Context, s SchemaStrategies) context.Context {
	return context.WithValue(ctx, schemaStrategiesCtxKey{}, s)
}

// SchemaStrategiesFromContext reads the SchemaStrategies WithSchemaStrategies
// set on ctx, and reports whether it was ever called; the value is the zero
// value when it was not.
func SchemaStrategiesFromContext(ctx context.Context) (SchemaStrategies, bool) {
	s, ok := ctx.Value(schemaStrategiesCtxKey{}).(SchemaStrategies)
	return s, ok
}

// schemaStrategiesFromCtx reads the SchemaStrategies WithSchemaStrategies
// set, or the zero value when it was never called.
func schemaStrategiesFromCtx(ctx context.Context) SchemaStrategies {
	s, _ := SchemaStrategiesFromContext(ctx)
	return s
}

// UnderSchemaStrategies renders f under s, for a Frag a head builds outside
// any QueryBuilder that carries strategies — a predicate assembled for a
// statement whose QueryBuilder is shared. The enclosing Builder's own
// strategies are restored afterwards.
func UnderSchemaStrategies(s SchemaStrategies, f Frag) Frag {
	return func(b *Builder) {
		prev := b.strategies
		b.strategies = s
		f(b)
		b.strategies = prev
	}
}

// ResolvedSchemaStrategies is the SchemaStrategies of each signal's tables
// as the boot requirements check (internal/preflight) resolves them. Each
// head reads the one for the signal it queries, so a strategy one signal's
// tables decide never reaches another's statements (see AttrStrategies on
// why that matters for the attribute maps).
type ResolvedSchemaStrategies struct {
	Metrics SchemaStrategies
	Logs    SchemaStrategies
	Traces  SchemaStrategies
}

// SchemaStrategiesCell holds the ResolvedSchemaStrategies a running process
// renders every statement under. cmd/cerberus stores the value the boot
// requirements check resolved; a check that ran before the tables existed
// stores the value its background re-probe resolves once they appear,
// before the process reports ready. Every head Loads it once per request,
// concurrently with that Store, and renders every statement of that request
// under the one value it loaded (WithSchemaStrategies on the request
// context). A nil cell, or one never stored, loads the zero value.
type SchemaStrategiesCell struct {
	p atomic.Pointer[ResolvedSchemaStrategies]
}

// NewSchemaStrategiesCell returns a cell holding r.
func NewSchemaStrategiesCell(r ResolvedSchemaStrategies) *SchemaStrategiesCell {
	c := &SchemaStrategiesCell{}
	c.Store(r)
	return c
}

// Load returns the strategies the cell holds.
func (c *SchemaStrategiesCell) Load() ResolvedSchemaStrategies {
	if c == nil {
		return ResolvedSchemaStrategies{}
	}
	if r := c.p.Load(); r != nil {
		return *r
	}
	return ResolvedSchemaStrategies{}
}

// Store replaces the strategies the cell holds. r's AttrStrategies maps are
// shared with every later Load and must not be mutated afterwards.
func (c *SchemaStrategiesCell) Store(r ResolvedSchemaStrategies) { c.p.Store(&r) }

// Signal names the telemetry signal whose SchemaStrategies a head renders
// under.
type Signal int

// The signals of ResolvedSchemaStrategies.
const (
	SignalMetrics Signal = iota
	SignalLogs
	SignalTraces
)

// For returns the SchemaStrategies of signal.
func (r ResolvedSchemaStrategies) For(signal Signal) SchemaStrategies {
	switch signal {
	case SignalLogs:
		return r.Logs
	case SignalTraces:
		return r.Traces
	default:
		return r.Metrics
	}
}

// WithRequestStrategies loads signal's strategies from the cell once and
// returns ctx carrying them (WithSchemaStrategies): the one snapshot every
// statement of the request renders under, however many it emits and however
// the schema re-probe Stores meanwhile. A head calls it where a request
// enters it.
func (c *SchemaStrategiesCell) WithRequestStrategies(ctx context.Context, signal Signal) context.Context {
	return WithSchemaStrategies(ctx, c.Load().For(signal))
}

// RequestStrategies returns the strategies ctx carries — the request's
// snapshot (WithRequestStrategies) — or, for a ctx that carries none, loads
// signal's strategies from the cell.
func (c *SchemaStrategiesCell) RequestStrategies(ctx context.Context, signal Signal) SchemaStrategies {
	if s, ok := SchemaStrategiesFromContext(ctx); ok {
		return s
	}
	return c.Load().For(signal)
}
