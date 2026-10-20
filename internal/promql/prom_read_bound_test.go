package promql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql/promparse"
	"github.com/tsouza/cerberus/internal/schema"
)

// promReadYear is the 365-day year PromQL's `y` unit names.
const promReadYear = 365 * 24 * time.Hour

// promReadEval returns the evaluator state of a selector below the
// subqueries subs, outermost first, of a request spanning req.
func promReadEval(req [2]int64, subs ...*parser.SubqueryExpr) promEval {
	ev := promEval{req: &req}
	for _, n := range subs {
		ev.sums = ev.sums.add(n, ev.req)
	}
	return ev
}

// TestPromReadSums_Wraps pins when the subquery sums above a selector
// wrap as Prometheus adds them: past what a time.Duration holds, and not
// when a later term brings an overflowing sum back.
func TestPromReadSums_Wraps(t *testing.T) {
	req := [2]int64{0, 0}
	sub := func(rng, off time.Duration) *parser.SubqueryExpr {
		return &parser.SubqueryExpr{Range: rng, OriginalOffset: off}
	}
	cases := []struct {
		name string
		subs []*parser.SubqueryExpr
		want bool
	}{
		{"no_subquery", nil, false},
		{"ranges_within_a_duration", []*parser.SubqueryExpr{sub(146*promReadYear, 0), sub(146*promReadYear, 0)}, false},
		{"ranges_past_a_duration", []*parser.SubqueryExpr{sub(200*promReadYear, 0), sub(200*promReadYear, 0)}, true},
		{"offsets_past_a_duration", []*parser.SubqueryExpr{sub(time.Hour, 200*promReadYear), sub(time.Hour, 200*promReadYear)}, true},
		{"offsets_brought_back", []*parser.SubqueryExpr{sub(time.Hour, 200*promReadYear), sub(time.Hour, 200*promReadYear), sub(time.Hour, -200*promReadYear)}, false},
		{"pin_resets_the_sums", []*parser.SubqueryExpr{sub(200*promReadYear, 0), {Range: 200 * promReadYear, StartOrEnd: parser.START}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := promReadEval(req, tc.subs...).sums.wraps(); got != tc.want {
				t.Fatalf("wraps() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPromSelectorHint pins the transcription of the storage read
// Prometheus bounds a selector with (getTimeRangesForSelector), with the
// subquery sums wrapped: three 200y ranges add up to 474855926290 ms.
func TestPromSelectorHint(t *testing.T) {
	const (
		end       = int64(1893456000000)
		start     = end - 3600000
		wrapped   = int64(474855926290)
		lookback  = int64(300000)
		dayMillis = int64(86400000)
	)
	nested := func(pin parser.ItemType) []*parser.SubqueryExpr {
		return []*parser.SubqueryExpr{{Range: 200 * promReadYear, StartOrEnd: pin}, {Range: 200 * promReadYear}, {Range: 200 * promReadYear}}
	}
	cases := []struct {
		name string
		vs   *parser.VectorSelector
		rng  time.Duration
		pin  parser.ItemType
		want [2]int64
	}{
		{"instant", &parser.VectorSelector{}, 0, 0, [2]int64{start - wrapped - (lookback - 1), end}},
		{"window", &parser.VectorSelector{}, 20 * promReadYear, 0, [2]int64{start - wrapped - (20*365*dayMillis - 1), end}},
		{"own_offset", &parser.VectorSelector{OriginalOffset: 24 * time.Hour}, 0, 0, [2]int64{start - wrapped - (lookback - 1) - dayMillis, end - dayMillis}},
		{"start_pin", &parser.VectorSelector{}, 0, parser.START, [2]int64{start - wrapped - (lookback - 1), start}},
		{"end_pin", &parser.VectorSelector{}, 0, parser.END, [2]int64{end - wrapped - (lookback - 1), end}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := promReadEval([2]int64{start, end}, nested(tc.pin)...)
			if got := promSelectorHint(ev, tc.vs, tc.rng); got != tc.want {
				t.Fatalf("promSelectorHint = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPromSelectorHint_ExtendedRangeSelectorsDoNotParse pins why
// promSelectorHint has no arm for the `anchored` and `smoothed` modifiers:
// the parser cerberus parses with refuses them.
func TestPromSelectorHint_ExtendedRangeSelectorsDoNotParse(t *testing.T) {
	for _, q := range []string{`rate(m[5m] anchored)`, `rate(m[5m] smoothed)`, `m smoothed`} {
		if _, err := promparse.New().ParseExpr(q); err == nil {
			t.Fatalf("%q parses; promSelectorHint does not widen the read for its modifier", q)
		}
	}
}

// TestBoundReads_BoundsEachSelectorsScans pins that the lowering bounds
// every scan of a selector whose subquery sums wrap to that selector's
// own storage read, and leaves every other scan unbounded.
func TestBoundReads_BoundsEachSelectorsScans(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	const nest = `max_over_time(max_over_time(max_over_time((%s)[200y:100d])[200y:100d])[200y:100d])`
	// A scan reads its bound, or nothing past the selector's own window:
	// unbounded, the zero read.
	type read struct {
		bounded bool
		span    chplan.MillisSpan
	}
	spans := func(t *testing.T, query string) map[read]int {
		t.Helper()
		plan, err := LowerAt(context.Background(), mustParseExperimental(t, query), schema.DefaultOTelMetrics(), at, at)
		if err != nil {
			t.Fatalf("lower %q: %v", query, err)
		}
		got := map[read]int{}
		chplan.WalkDeep(plan, func(n chplan.Node) bool {
			if s, ok := n.(*chplan.Scan); ok && s.Database == "" {
				var r read
				if s.ReadMillis != nil {
					r = read{bounded: true, span: *s.ReadMillis}
				}
				got[r]++
			}
			return true
		})
		return got
	}
	const day = int64(86400000)
	bound := read{bounded: true, span: chplan.MillisSpan{From: 1418599773711, To: 1893456000000}}
	offset := read{bounded: true, span: chplan.MillisSpan{From: bound.span.From - day, To: bound.span.To - day}}
	for _, tc := range []struct {
		query string
		want  []read
	}{
		{fmt.Sprintf(nest, `m`), []read{bound}},
		{fmt.Sprintf(nest, `m + m offset 1d`), []read{bound, offset}},
		// The 5m window reads what the instant selector's lookback does.
		{fmt.Sprintf(nest, `quantile_over_time(scalar(m offset 1d), m[5m])`), []read{bound, offset}},
		// A selector outside the subqueries reads what it evaluates.
		{fmt.Sprintf(nest, `m`) + ` + n`, []read{bound, {}}},
		{`max_over_time(max_over_time(m[5m:1m])[5m:1m])`, []read{{}}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got := spans(t, tc.query)
			for _, w := range tc.want {
				if got[w] == 0 {
					t.Fatalf("no scan reads %v; scans by read: %v", w, got)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("scans by read %v, want only %v", got, tc.want)
			}
		})
	}
}
