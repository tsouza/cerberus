package chplan

import "time"

// NanoScale is the DateTime64 sub-second scale (fractional-digit count)
// every cerberus timestamp expression uses: 9 digits = nanosecond
// precision, matching the OTel-CH schema's `Timestamp DateTime64(9)`
// columns. It is the scale arg to `now64(scale)` and `toDateTime64(s,
// scale)` everywhere a server-side or literal timestamp is stamped.
const NanoScale = 9

// NanoToSecondDivisor converts a nanosecond timestamp/duration into
// seconds: it is the `/ 1e9` divisor applied wherever a DateTime64(9)
// epoch (`toUnixTimestamp64Nano(...)`) or a nanosecond duration is
// rebased to the seconds unit PromQL/Grafana expects.
const NanoToSecondDivisor int64 = 1_000_000_000

// stalenessLookbackNanos is how far before the server clock an
// instant-shape sample row is anchored when the plan exposes no real
// per-row timestamp (rate/count_over_time/… in instant mode): 5 seconds
// expressed in nanoseconds. It mirrors the scrape-interval-sized window
// the synthesized anchor needs so the matrix pivot lands on one bucket.
const stalenessLookbackNanos = 5 * int64(time.Second)

// NowNano builds the canonical server-clock timestamp expression
// `now64(9)` — CH's nanosecond-precision wall clock. Use this instead of
// hand-constructing the FuncCall so the NanoScale arg stays in one place.
func NowNano() Expr {
	return &FuncCall{Name: "now64", Args: []Expr{&LitInt{V: NanoScale}}}
}

// NowNanoMinusStaleness builds `now64(9) - toIntervalNanosecond(5e9)` —
// the synthesized instant anchor stamped on derived-shape sample rows
// that lack a real per-row timestamp. See stalenessLookbackNanos.
func NowNanoMinusStaleness() Expr {
	return &Binary{
		Op:   OpSub,
		Left: NowNano(),
		Right: &FuncCall{
			Name: "toIntervalNanosecond",
			Args: []Expr{&LitInt{V: stalenessLookbackNanos}},
		},
	}
}
