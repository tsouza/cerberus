package promql

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// civilNumber is one value of the ClickHouse integer/float arithmetic the
// date kernel emits, as [evalCivil] models it: Int64, UInt64 or Float64.
type civilNumber struct {
	kind byte // 'i', 'u' or 'f'
	bits uint64
	f    float64
}

func civilInt(v int64) civilNumber   { return civilNumber{kind: 'i', bits: uint64(v)} }
func civilUint(v uint64) civilNumber { return civilNumber{kind: 'u', bits: v} }
func civilBool(b bool) civilNumber {
	if b {
		return civilUint(1)
	}
	return civilUint(0)
}

// signed reports the value as an int64, failing when a non-negative
// UInt64 does not fit: the kernel never relies on that conversion.
func (n civilNumber) signed(t *testing.T) int64 {
	t.Helper()
	if n.kind == 'u' && n.bits > math.MaxInt64 {
		t.Fatalf("UInt64 %d used where the kernel needs it signed", n.bits)
	}
	return int64(n.bits)
}

// evalCivil evaluates e over value as the Value column, with the
// ClickHouse semantics the kernel depends on: integer literals are
// non-negative and typed unsigned; `+` and `*` of two unsigned operands
// wrap modulo 2^64; `-` yields Int64; `intDiv` and `modulo` truncate toward
// zero; `toInt64` of a Float64 truncates and must never see NaN or ±Inf,
// which ClickHouse rejects. Any signed overflow fails the test.
func evalCivil(t *testing.T, e chplan.Expr, value float64) civilNumber {
	t.Helper()
	switch v := e.(type) {
	case *chplan.LitInt:
		if v.V < 0 {
			t.Fatalf("negative integer literal %d", v.V)
		}
		return civilUint(uint64(v.V))
	case *chplan.LitFloat:
		return civilNumber{kind: 'f', f: v.V}
	case *chplan.ColumnRef:
		return civilNumber{kind: 'f', f: value}
	case *chplan.FuncCall:
		args := make([]civilNumber, len(v.Args))
		for i, a := range v.Args {
			args[i] = evalCivil(t, a, value)
		}
		switch v.Fn {
		case chplan.FnIf:
			if args[0].bits != 0 {
				return args[1]
			}
			return args[2]
		case chplan.FnToInt64:
			f := args[0].f
			if math.IsNaN(f) || f < math.MinInt64 || f >= -math.MinInt64 {
				t.Fatalf("toInt64(%v) throws or is implementation-defined in ClickHouse", f)
			}
			return civilInt(int64(f))
		case chplan.FnReinterpretAsUInt64, chplan.FnToUInt64:
			return civilUint(args[0].bits)
		case chplan.FnIntDiv:
			return civilDivMod(t, args[0], args[1], true)
		case chplan.FnModulo:
			return civilDivMod(t, args[0], args[1], false)
		}
		t.Fatalf("unmodelled function %s", v.Fn)
	case *chplan.Binary:
		l, r := evalCivil(t, v.Left, value), evalCivil(t, v.Right, value)
		switch v.Op {
		case chplan.OpGe, chplan.OpLt:
			var less bool
			if l.kind == 'f' {
				less = l.f < r.f
			} else {
				less = l.signed(t) < r.signed(t)
			}
			return civilBool(less == (v.Op == chplan.OpLt))
		case chplan.OpEq:
			return civilBool(l.bits == r.bits)
		case chplan.OpNe:
			return civilBool(l.bits != r.bits)
		case chplan.OpAnd:
			return civilBool(l.bits != 0 && r.bits != 0)
		case chplan.OpOr:
			return civilBool(l.bits != 0 || r.bits != 0)
		case chplan.OpAdd, chplan.OpMul:
			if l.kind == 'u' && r.kind == 'u' {
				if v.Op == chplan.OpAdd {
					return civilUint(l.bits + r.bits)
				}
				return civilUint(l.bits * r.bits)
			}
			a, b := l.signed(t), r.signed(t)
			if v.Op == chplan.OpAdd {
				return civilInt(civilChecked(t, a+b, (b > 0 && a+b < a) || (b < 0 && a+b > a)))
			}
			return civilInt(civilChecked(t, a*b, a != 0 && (a*b)/a != b))
		case chplan.OpSub:
			a, b := l.signed(t), r.signed(t)
			return civilInt(civilChecked(t, a-b, (b > 0 && a-b > a) || (b < 0 && a-b < a)))
		}
		t.Fatalf("unmodelled operator %s", v.Op)
	}
	t.Fatalf("unmodelled expression %T", e)
	return civilNumber{}
}

func civilChecked(t *testing.T, v int64, overflowed bool) int64 {
	t.Helper()
	if overflowed {
		t.Fatal("signed overflow in the date kernel")
	}
	return v
}

func civilDivMod(t *testing.T, l, r civilNumber, quotient bool) civilNumber {
	t.Helper()
	if l.kind == 'u' && r.kind == 'u' {
		if quotient {
			return civilUint(l.bits / r.bits)
		}
		return civilUint(l.bits % r.bits)
	}
	a, b := l.signed(t), r.signed(t)
	if quotient {
		return civilInt(a / b)
	}
	return civilInt(a % b)
}

// civilGoReference is what Prometheus's date functions answer for the
// sample value v: `time.Unix(int64(v), 0).UTC()`'s calendar components.
func civilGoReference(fn string, v float64) float64 {
	tm := time.Unix(civilGoInt64(v), 0).UTC()
	switch fn {
	case "year":
		return float64(tm.Year())
	case "month":
		return float64(tm.Month())
	case "day_of_month":
		return float64(tm.Day())
	case "day_of_week":
		return float64(tm.Weekday())
	case "day_of_year":
		return float64(tm.YearDay())
	case "days_in_month":
		return float64(32 - time.Date(tm.Year(), tm.Month(), 32, 0, 0, 0, 0, time.UTC).Day())
	case "hour":
		return float64(tm.Hour())
	case "minute":
		return float64(tm.Minute())
	}
	panic(fn)
}

// civilGoInt64 is Go's int64(v) as amd64 computes it, spelled out so the
// test does not depend on the architecture it runs on.
func civilGoInt64(v float64) int64 {
	if v >= math.MinInt64 && v < -math.MinInt64 {
		return int64(v)
	}
	return math.MinInt64
}

var civilKernelFunctions = []string{
	"year", "month", "day_of_month", "day_of_week", "day_of_year", "days_in_month", "hour", "minute",
}

// civilKernelValues are the sample values the kernel is checked at,
// beyond a random sweep: fractions, calendar boundaries, the int64
// conversion edges and the edge where Go's absolute calendar wraps.
func civilKernelValues() []float64 {
	const wrap, spacing = 9223372028741760000, 1024
	values := []float64{
		0, -0.5, 0.5, -1.5, 1.5, -1, 86399, -86400, -86401, 5e9, -5e9,
		math.NaN(), math.Inf(1), math.Inf(-1), 1e19, -1e19, math.MaxInt64, math.MinInt64,
		math.Nextafter(math.MaxInt64, 0), math.Nextafter(math.MinInt64, 0),
	}
	for _, steps := range []float64{-3000, -2700, -2600, -100, -1, 0, 1} {
		values = append(values, -wrap+steps*spacing)
	}
	for _, year := range []int{-1_000_000_000, -401, -400, -100, -1, 0, 1, 1600, 1700, 1900, 1969, 2000, 2100, 2106, 2262, 2400, 200_000_000_000} {
		for _, month := range []time.Month{time.January, time.February, time.March, time.December} {
			for _, day := range []int{1, 28, 29, 30, 31} {
				sec := float64(time.Date(year, month, day, 23, 59, 59, 0, time.UTC).Unix())
				values = append(values, sec, sec+1)
			}
		}
	}
	rng := rand.New(rand.NewPCG(3838, 3838))
	for range 2000 {
		values = append(values, float64(int64(rng.Uint64())), (rng.Float64()-0.5)*1e13)
	}
	return values
}

// TestGoCivilMatchesGoTime evaluates the emitted date kernel, under the
// ClickHouse semantics it relies on, against Go's time package — the
// calendar Prometheus answers from — across the full float64 range.
func TestGoCivilMatchesGoTime(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	values := civilKernelValues()
	for _, fn := range civilKernelFunctions {
		expr := dateFnExpr(fn, valueAsUnixSeconds(s), nil)
		for _, v := range values {
			got := evalCivil(t, expr, v).signed(t)
			if want := civilGoReference(fn, v); float64(got) != want {
				t.Fatalf("%s(%v) = %d, Go time %v", fn, v, got, want)
			}
		}
	}
}

// TestEvalUnixSecondsReadsTheEvaluationInstant pins which instant the
// no-argument date functions read: the literal request anchor in instant
// mode, the per-step `now64(9)` placeholder [syntheticScalarVector]
// rewrites to `anchor_ts` in range mode.
func TestEvalUnixSecondsReadsTheEvaluationInstant(t *testing.T) {
	t.Parallel()
	at := time.Date(1950, time.June, 15, 12, 34, 56, 0, time.UTC)
	instantOf := func(ctx lowerCtx) chplan.Expr {
		call, ok := evalUnixSeconds(ctx).(*chplan.FuncCall)
		if !ok || call.Fn != chplan.FnIntDiv {
			t.Fatalf("evalUnixSeconds = %#v, want intDiv(...)", call)
		}
		nanos, ok := call.Args[0].(*chplan.FuncCall)
		if !ok || nanos.Fn != chplan.FnToUnixNanos {
			t.Fatalf("evalUnixSeconds dividend = %#v, want toUnixTimestamp64Nano(...)", call.Args[0])
		}
		if !call.Args[1].Equal(&chplan.LitInt{V: chplan.NanoToSecondDivisor}) {
			t.Fatalf("evalUnixSeconds divisor = %#v", call.Args[1])
		}
		return nanos.Args[0]
	}
	for _, tc := range []struct {
		name string
		ctx  lowerCtx
		want chplan.Expr
	}{
		{"instant", lowerCtx{end: at}, chplan.TimeLiteral(at)},
		{"instant without anchor", lowerCtx{}, chplan.NowNano()},
		{"range", lowerCtx{end: at, step: time.Minute}, chplan.NowNano()},
	} {
		if got := instantOf(tc.ctx); !got.Equal(tc.want) {
			t.Errorf("%s: evalUnixSeconds reads %#v, want %#v", tc.name, got, tc.want)
		}
	}
}

// TestDateFnExprNames pins the recognised names, which
// [isDateComponentFn] reads through dateFnExpr.
func TestDateFnExprNames(t *testing.T) {
	t.Parallel()
	for _, fn := range append([]string{timestampFunctionName}, civilKernelFunctions...) {
		if dateFnExpr(fn, nil, nil) == nil {
			t.Errorf("dateFnExpr(%q) = nil", fn)
		}
	}
	if got := dateFnExpr("time", nil, nil); got != nil {
		t.Errorf("dateFnExpr(\"time\") = %s, want nil", fmt.Sprint(got))
	}
}
