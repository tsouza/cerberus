package chplan

import (
	"math"
	"testing"
	"time"
)

// TestTimeLiteral pins the two forms of an instant: the parsed string from
// 1900-01-01 on, and Unix nanoseconds before it, where ClickHouse would
// parse the string as 1900-01-01. Outside int64 nanoseconds the nearer
// bound is named.
func TestTimeLiteral(t *testing.T) {
	t.Parallel()
	nanos := func(n int64) Expr {
		return &FuncCall{Fn: FnFromUnixNanos, Args: []Expr{&FuncCall{Fn: FnToInt64, Args: []Expr{&LitInt{V: n}}}}}
	}
	parsed := func(s string) Expr {
		return &FuncCall{Fn: FnParseDateTime64BestEffort, Args: []Expr{&LitString{V: s}, &LitInt{V: NanoScale}}}
	}
	cases := []struct {
		name string
		at   time.Time
		want Expr
	}{
		{"literal_min", TimeLiteralMin, parsed("1900-01-01 00:00:00.000000000Z")},
		{"before_literal_min", TimeLiteralMin.Add(-time.Nanosecond), nanos(TimeLiteralMin.UnixNano() - 1)},
		{"storage_min", StorageMinTime, nanos(math.MinInt64)},
		{"before_storage_min", time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), nanos(math.MinInt64)},
		{"storage_max", StorageMaxTime, parsed("2262-04-11 23:47:16.854775807Z")},
	}
	for _, tc := range cases {
		if got := TimeLiteral(tc.at); !got.Equal(tc.want) {
			t.Errorf("%s: TimeLiteral(%s) = %#v, want %#v", tc.name, tc.at, got, tc.want)
		}
	}
	if got := TimeLiteralNanos(StorageMaxTime.Add(time.Hour)); got != math.MaxInt64 {
		t.Errorf("TimeLiteralNanos(past the ceiling) = %d, want %d", got, int64(math.MaxInt64))
	}
}
