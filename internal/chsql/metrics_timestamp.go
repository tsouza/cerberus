package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// A metrics timestamp read renders for the precision of the deployed
// timestamp columns (SchemaStrategies.MetricsTimestamp).
//
// ClickHouse's `toUnixTimestamp64Nano` / `toUnixTimestamp64Milli` accept only
// a DateTime64 argument; over a `DateTime` column they fail with
// ILLEGAL_TYPE_OF_ARGUMENT. The native `timeSeries*` aggregates accept any
// timestamp but compute in its scale, so the same samples answer
// differently in the last float bit over a `DateTime` or `DateTime64(3)`
// column than over a `DateTime64(9)` one. Every other operation the emitter
// applies to a metrics timestamp — comparison against a DateTime64 literal,
// min/max, argMax keys, `fromUnixTimestamp64*` results, interval arithmetic
// — reads the instant alone and accepts any of them.
//
// Under schema.MetricsTimestampNanos (the zero value) those reads take the
// column directly, the SQL every DateTime64(9) deployment has always run.
// Under schema.MetricsTimestampSeconds they read it through
// `toDateTime64(<x>, 9)` (timestampDateTime64): an exact conversion of every
// coarser timestamp, and the identity on a DateTime64(9) expression such as
// an anchor or a literal, so every statement keeps the result types and
// values it has over a DateTime64(9) table holding the same samples.

// ToUnixNanos renders the Int64 Unix nanoseconds of the timestamp x,
// `toUnixTimestamp64Nano(<x>)`, for the statement's metrics timestamp
// precision (see unixTimestamp64).
func ToUnixNanos(x Frag) Frag { return unixTimestamp64(chplan.FnToUnixNanos, x) }

// ToUnixMillis renders the Int64 Unix milliseconds of the timestamp x,
// `toUnixTimestamp64Milli(<x>)`, for the statement's metrics timestamp
// precision (see unixTimestamp64).
func ToUnixMillis(x Frag) Frag { return unixTimestamp64(chplan.FnToUnixMillis, x) }

// unixTimestamp64 renders `<fn>(<x>)` for one of the DateTime64-only
// `toUnixTimestamp64*` functions, resolving fn's ClickHouse name through
// fnResolutions like every other plan function, with x read through
// timestampDateTime64: x may be a coarser metrics column, which the function
// rejects, or a DateTime64(9) expression (an anchor, a literal), on which the
// widening is the identity — so the one rendering is right whatever x is,
// and no caller has to know which of the two it holds.
func unixTimestamp64(fn chplan.Fn, x Frag) Frag {
	return func(b *Builder) {
		name, _, err := resolveFn(fn)
		if err != nil {
			if b.err == nil {
				b.err = err
			}
			return
		}
		Call(name, timestampDateTime64(x))(b)
	}
}

// timestampDateTime64 renders the timestamp x so it reads as a
// DateTime64(9): x itself under schema.MetricsTimestampNanos,
// `toDateTime64(<x>, 9)` under schema.MetricsTimestampSeconds. Besides the
// `toUnixTimestamp64*` functions (unixTimestamp64), the native timeSeries*
// aggregates read their timestamp argument through it.
func timestampDateTime64(x Frag) Frag {
	return func(b *Builder) {
		if b.strategies.MetricsTimestamp == schema.MetricsTimestampSeconds {
			ToDateTime64Nanos(x)(b)
			return
		}
		x(b)
	}
}

// toDateTime64Fn is ClickHouse's DateTime64 conversion function.
const toDateTime64Fn = "toDateTime64"

// ToDateTime64Nanos renders `toDateTime64(<x>, 9)`: the timestamp x as a
// DateTime64(9), an exact widening of a DateTime or a coarser DateTime64 and
// the identity on a DateTime64(9).
func ToDateTime64Nanos(x Frag) Frag { return Call(toDateTime64Fn, x, InlineLit(int64(nanoScale))) }

// exprUnixTimestamp64 renders a chplan.FuncCall of FnToUnixNanos /
// FnToUnixMillis through unixTimestamp64. exprFunc dispatches to it after
// resolving the Fn, rather than through a fnResolutions Render hook, for the
// same package-init-cycle reason exprMapContainsKey is special-cased: a
// Render hook stored in fnResolutions that calls b.Expr would reach
// fnResolutions again.
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
