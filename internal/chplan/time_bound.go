package chplan

import (
	"math"
	"time"
)

// The instants a DateTime64(9) value, and so every OTel-CH timestamp
// column, can hold: int64 nanoseconds since the Unix epoch.
var (
	StorageMinTime = time.Unix(0, math.MinInt64).UTC()
	StorageMaxTime = time.Unix(0, math.MaxInt64).UTC()
)

// TimeLiteralMin is the earliest instant a parsed time string names
// exactly. ClickHouse parses an earlier string as this instant: it clamps
// rather than rejects. [TimeLiteral] writes an earlier instant as Unix
// nanoseconds instead.
var TimeLiteralMin = time.Date(1900, time.January, 1, 0, 0, 0, 0, time.UTC)

// TimeBound is the exact SQL form of a closed bound `x >= t` or `x <= t`
// on a DateTime64(9) value x. It compares either x with [TimeLiteral] of
// T, or, when Nanos is set, `toUnixTimestamp64Nano(x)` with the Int64 N.
type TimeBound struct {
	Op    BinaryOp
	Nanos bool
	N     int64
	T     time.Time
}

// AtLeastBound returns the exact form of `x >= t`, and false when the
// bound excludes no value x can take (t at or before [StorageMinTime]):
//
//   - after [StorageMaxTime]: `toUnixTimestamp64Nano(x) > 9223372036854775807`,
//     which no value satisfies;
//   - before [TimeLiteralMin], where a parsed time string would be clamped:
//     `toUnixTimestamp64Nano(x) >= <t in Unix nanoseconds>`;
//   - otherwise `x >= <TimeLiteral(t)>`.
func AtLeastBound(t time.Time) (TimeBound, bool) {
	switch {
	case !t.After(StorageMinTime):
		return TimeBound{}, false
	case t.After(StorageMaxTime):
		return TimeBound{Op: OpGt, Nanos: true, N: math.MaxInt64}, true
	case t.Before(TimeLiteralMin):
		return TimeBound{Op: OpGe, Nanos: true, N: t.UnixNano()}, true
	}
	return TimeBound{Op: OpGe, T: t}, true
}

// AtMostBound returns the exact form of `x <= t`, and false when the
// bound excludes no value x can take (t at or after [StorageMaxTime]). It
// mirrors [AtLeastBound]: before [StorageMinTime] it is
// `toUnixTimestamp64Nano(x) < -9223372036854775808`, which no value
// satisfies, and before [TimeLiteralMin] it compares Unix nanoseconds.
func AtMostBound(t time.Time) (TimeBound, bool) {
	switch {
	case !t.Before(StorageMaxTime):
		return TimeBound{}, false
	case t.Before(StorageMinTime):
		return TimeBound{Op: OpLt, Nanos: true, N: math.MinInt64}, true
	case t.Before(TimeLiteralMin):
		return TimeBound{Op: OpLe, Nanos: true, N: t.UnixNano()}, true
	}
	return TimeBound{Op: OpLe, T: t}, true
}

// Expr builds the bound over x as a plan expression.
func (b TimeBound) Expr(x Expr) Expr {
	if b.Nanos {
		return &Binary{
			Op:    b.Op,
			Left:  &FuncCall{Fn: FnToUnixNanos, Args: []Expr{x}},
			Right: &LitInt{V: b.N},
		}
	}
	return &Binary{Op: b.Op, Left: x, Right: TimeLiteral(b.T)}
}
