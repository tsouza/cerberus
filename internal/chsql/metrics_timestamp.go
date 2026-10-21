package chsql

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/tsouza/cerberus/internal/chplan"
)

// MetricsTimestampStrategy names the physical ClickHouse type of the OTel-CH
// metrics timestamp columns (`TimeUnix`, `StartTimeUnix`,
// `Exemplars.TimeUnix`) a statement reads, and so how the emitter renders a
// timestamp function over them.
//
// ClickHouse's `toUnixTimestamp64Nano` / `toUnixTimestamp64Milli` accept only
// a DateTime64 argument; over a `DateTime` column they fail with
// ILLEGAL_TYPE_OF_ARGUMENT. Every other operation the emitter applies to a
// metrics timestamp — comparison against a DateTime64 literal, min/max,
// argMax keys, `fromUnixTimestamp64*` results, interval arithmetic — accepts
// either type. The strategy therefore changes how those two functions render,
// plus the timestamp argument of the native `timeSeries*` aggregates, which
// accept a DateTime but compute in its scale (see timestampDateTime64).
//
// The zero value is MetricsTimestampDateTime64, which renders the functions
// directly, exactly as the emitter always has.
type MetricsTimestampStrategy uint32

const (
	// MetricsTimestampDateTime64 is a metrics schema whose timestamp
	// columns are `DateTime64(9)` (the OTel-CH exporter's metrics schema
	// before v0.157). The timestamp functions take the column directly.
	MetricsTimestampDateTime64 MetricsTimestampStrategy = iota

	// MetricsTimestampDateTime is a metrics schema whose timestamp columns
	// are `DateTime` (the OTel-CH exporter's metrics schema from v0.157):
	// second precision. The timestamp functions read the argument through
	// `toDateTime64(<x>, 9)`, an exact widening of a DateTime and the
	// identity on a DateTime64(9), so every statement keeps the result
	// types and values it has over a DateTime64(9) table holding the same
	// whole-second samples.
	MetricsTimestampDateTime
)

// String names the strategy for logs and error messages.
func (s MetricsTimestampStrategy) String() string {
	if s == MetricsTimestampDateTime {
		return "DateTime"
	}
	return "DateTime64"
}

// MetricsTimestampCell holds the MetricsTimestampStrategy a running process
// serves PromQL with. The boot probe stores the strategy it resolved; a probe
// that ran before the metric tables existed stores the one its background
// re-probe resolves once they appear, before the process reports ready.
// Requests Load it concurrently with that Store. A nil cell loads the zero
// strategy.
type MetricsTimestampCell struct{ v atomic.Uint32 }

// NewMetricsTimestampCell returns a cell holding s.
func NewMetricsTimestampCell(s MetricsTimestampStrategy) *MetricsTimestampCell {
	c := &MetricsTimestampCell{}
	c.Store(s)
	return c
}

// Load returns the strategy the cell holds, or the zero strategy for a nil
// cell.
func (c *MetricsTimestampCell) Load() MetricsTimestampStrategy {
	if c == nil {
		return MetricsTimestampDateTime64
	}
	return MetricsTimestampStrategy(c.v.Load())
}

// Store replaces the strategy the cell holds.
func (c *MetricsTimestampCell) Store(s MetricsTimestampStrategy) { c.v.Store(uint32(s)) }

// metricsTimestampCtxKey is the context key for WithMetricsTimestamp /
// metricsTimestampFromCtx, following the unexported-key pattern
// WithAttrStrategies uses.
type metricsTimestampCtxKey struct{}

// WithMetricsTimestamp returns a context carrying the MetricsTimestampStrategy
// chsql.Emit renders the plan's metrics timestamp functions against.
// internal/engine calls it once per PromQL request with the strategy the
// boot probe resolved; a caller that never calls it gets
// MetricsTimestampDateTime64.
func WithMetricsTimestamp(ctx context.Context, s MetricsTimestampStrategy) context.Context {
	return context.WithValue(ctx, metricsTimestampCtxKey{}, s)
}

// metricsTimestampFromCtx reads the strategy WithMetricsTimestamp set, or
// MetricsTimestampDateTime64 when it was never called.
func metricsTimestampFromCtx(ctx context.Context) MetricsTimestampStrategy {
	s, _ := ctx.Value(metricsTimestampCtxKey{}).(MetricsTimestampStrategy)
	return s
}

// ToUnixNanos renders the Int64 Unix nanoseconds of the timestamp x,
// `toUnixTimestamp64Nano(<x>)`, through the Builder's
// MetricsTimestampStrategy (see unixTimestamp64).
func ToUnixNanos(x Frag) Frag { return unixTimestamp64(chplan.FnToUnixNanos, x) }

// ToUnixMillis renders the Int64 Unix milliseconds of the timestamp x,
// `toUnixTimestamp64Milli(<x>)`, through the Builder's
// MetricsTimestampStrategy (see unixTimestamp64).
func ToUnixMillis(x Frag) Frag { return unixTimestamp64(chplan.FnToUnixMillis, x) }

// unixTimestamp64 renders `<fn>(<x>)` for one of the DateTime64-only
// `toUnixTimestamp64*` functions. Under MetricsTimestampDateTime it renders
// `<fn>(toDateTime64(<x>, 9))` instead: x may be a DateTime metrics column,
// which the function rejects, or a DateTime64 expression (an anchor, a
// literal), on which the widening is the identity — so the one rendering is
// right whatever x is, and no caller has to know which of the two it holds.
func unixTimestamp64(fn chplan.Fn, x Frag) Frag {
	return Call(string(fn), timestampDateTime64(x))
}

// timestampDateTime64 renders the timestamp x so it reads as a DateTime64:
// x itself under MetricsTimestampDateTime64, `toDateTime64(<x>, 9)` under
// MetricsTimestampDateTime. Besides the `toUnixTimestamp64*` functions
// (unixTimestamp64), the native timeSeries* aggregates read their
// timestamp argument through it: they accept a DateTime, but compute in its
// scale, so the same samples would answer differently in the last float
// bit than they do over a DateTime64(9) column.
func timestampDateTime64(x Frag) Frag {
	return func(b *Builder) {
		if b.metricsTimestamp == MetricsTimestampDateTime {
			Call(toDateTime64Fn, x, InlineLit(int64(nanoScale)))(b)
			return
		}
		x(b)
	}
}

// toDateTime64Fn is ClickHouse's DateTime64 conversion function.
const toDateTime64Fn = "toDateTime64"

// exprUnixTimestamp64 renders a chplan.FuncCall of FnToUnixNanos /
// FnToUnixMillis through unixTimestamp64. It is special-cased in exprFunc
// ahead of the fnResolutions lookup for the same package-init-cycle reason
// exprMapContainsKey is: a Render hook stored in fnResolutions that calls
// b.Expr would reach fnResolutions again.
func (b *Builder) exprUnixTimestamp64(f *chplan.FuncCall) error {
	if len(f.Args) != 1 {
		return fmt.Errorf("%w: chplan.Fn %q takes one timestamp argument, got %d", ErrUnsupported, f.Fn, len(f.Args))
	}
	arg := f.Args[0]
	unixTimestamp64(f.Fn, func(b *Builder) { _ = b.Expr(arg) })(b)
	return b.err
}

// isUnixTimestamp64Fn reports whether fn is one of the DateTime64-only
// timestamp functions unixTimestamp64 renders.
func isUnixTimestamp64Fn(fn chplan.Fn) bool {
	return fn == chplan.FnToUnixNanos || fn == chplan.FnToUnixMillis
}

// inheritMetricsTimestamp renders s for splicing into the statement b is
// rendering. A *QueryBuilder that carries no strategy of its own renders
// under b's: the strategy is a property of the whole statement, and an
// emitter that composes an inner QueryBuilder by hand never stamps one on.
func inheritMetricsTimestamp(b *Builder, s Subqueryable) (string, []any, error) {
	qb, ok := s.(*QueryBuilder)
	if !ok || qb.metricsTimestamp != MetricsTimestampDateTime64 || b.metricsTimestamp == MetricsTimestampDateTime64 {
		return s.subquerySQL()
	}
	qb.metricsTimestamp = b.metricsTimestamp
	defer func() { qb.metricsTimestamp = MetricsTimestampDateTime64 }()
	return qb.subquerySQL()
}

// UnderMetricsTimestamp renders f with s as the statement's
// MetricsTimestampStrategy, for a Frag a caller builds outside any emitter
// — a metadata or exemplar predicate assembled straight on a QueryBuilder —
// whose timestamp functions read the metrics tables. The enclosing
// Builder's own strategy is restored afterwards.
func UnderMetricsTimestamp(s MetricsTimestampStrategy, f Frag) Frag {
	return func(b *Builder) {
		prev := b.metricsTimestamp
		b.metricsTimestamp = s
		f(b)
		b.metricsTimestamp = prev
	}
}
