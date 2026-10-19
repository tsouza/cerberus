package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// instantDomainEvalEnd is the request instant every case below lowers at
// unless the case names its own.
var instantDomainEvalEnd = time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)

// instantDomainMode is one way a case is lowered: an instant query at
// instantDomainEvalEnd, or a range query ending there.
type instantDomainMode struct {
	name  string
	start time.Time
	step  time.Duration
}

var instantDomainModes = []instantDomainMode{
	{name: "instant", start: instantDomainEvalEnd},
	{name: "range", start: instantDomainEvalEnd.Add(-5 * time.Minute), step: time.Minute},
}

func lowerInstantDomainCase(t *testing.T, q string, m instantDomainMode) error {
	t.Helper()
	_, err := LowerAtRange(context.Background(), mustParseExperimental(t, q), schema.DefaultOTelMetrics(),
		m.start, instantDomainEvalEnd, m.step)
	return err
}

// mustUTC parses an RFC 3339 instant.
func mustUTC(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts.UTC()
}

// TestInstantDomain_RejectsUnrepresentableInstants pins the rejection of
// every query that needs an instant ClickHouse cannot hold exactly and
// that is not provably empty. The error names the instant and the range
// cerberus supports.
func TestInstantDomain_RejectsUnrepresentableInstants(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		// A constant inner has an answer at every grid instant, so a grid
		// at year one is rejected rather than read as unpinned.
		{"year_one_pin_constant_subquery", `count_over_time(vector(1)[5m:1m] @ -62135596800)`, "0001-01-01T00:00:00Z"},
		{"pin_before_int64_floor", `count_over_time(vector(1)[5m:1m] @ -9300000000)`, "1675-04-18T02:40:00Z"},
		{"pin_past_int64_ceiling", `count_over_time(vector(1)[5m:1m] @ 10000000000)`, "2286-11-20T17:46:40Z"},
		// The pin itself is inside the int64-nanosecond range; the
		// subquery window it derives reaches below it.
		{"subquery_window_below_floor", `count_over_time(vector(1)[5m:1m] @ -9223372036.854)`, "1677-09-21T00:12:43.146Z"},
		{"subquery_window_below_floor_by_offset", `count_over_time(vector(1)[5m:1m] @ -9223372036 offset 1h)`, "1677-09-20T23:12:44Z"},
		// Instants between 1677 and 1900 fit int64 nanoseconds, but a
		// ClickHouse DateTime64 literal clamps them to 1900-01-01, and
		// storage can hold samples there, so no empty answer is exact.
		{"selector_pin_before_1900", `m @ -5000000000`, "1811-07-23T15:06:40Z"},
		{"selector_window_crossing_1900", `m @ -2208988560`, "1899-12-31T23:59:00Z"},
		{"rate_pin_before_1900", `rate(m[5m] @ -5000000000)`, "1811-07-23T15:06:40Z"},
		// absent reads its labels from the selector, so the selector
		// cannot be replaced.
		{"absent_of_year_one_selector", `absent(m @ -62135596800)`, "0001-01-01T00:00:00Z"},
	}
	for _, tc := range cases {
		for _, m := range instantDomainModes {
			t.Run(tc.name+"/"+m.name, func(t *testing.T) {
				err := lowerInstantDomainCase(t, tc.query, m)
				want := instantDomainError(mustUTC(t, tc.want)).Error()
				if err == nil || err.Error() != want {
					t.Fatalf("lower %q: err = %v, want %q", tc.query, err, want)
				}
				if !strings.Contains(want, "1900-01-01T00:00:00Z to 2262-04-11T23:47:16.854775807Z") {
					t.Fatalf("error %q does not name the supported range", want)
				}
			})
		}
	}
}

// TestInstantDomain_RejectsRequestInstantsOutsideDomain pins the check on
// the request's own instants, which the lowering writes into SQL as the
// result timestamps.
func TestInstantDomain_RejectsRequestInstantsOutsideDomain(t *testing.T) {
	floor := time.Unix(0, -1<<63).UTC()
	cases := []struct {
		name       string
		query      string
		start, end time.Time
		step       time.Duration
		want       time.Time
	}{
		{"instant_before_1900", `vector(1)`, mustUTC(t, "1800-01-01T00:00:00Z"), mustUTC(t, "1800-01-01T00:00:00Z"), 0, mustUTC(t, "1800-01-01T00:00:00Z")},
		// The API clamps a request start below the int64-nanosecond floor
		// to the floor; an unpinned subquery there reaches below it.
		{"range_start_at_int64_floor", `count_over_time(vector(1)[5m:1m])`, floor, floor.Add(time.Hour), time.Minute, floor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LowerAtRange(context.Background(), mustParseExperimental(t, tc.query), schema.DefaultOTelMetrics(), tc.start, tc.end, tc.step)
			want := instantDomainError(tc.want).Error()
			if err == nil || err.Error() != want {
				t.Fatalf("lower %q: err = %v, want %q", tc.query, err, want)
			}
		})
	}
}

// TestInstantDomain_Boundaries pins the exact edges of the check. A
// subquery grid reaches one step below its window and a step past its end;
// a selector reaches back by its lookback. The last accepted pin and the
// first rejected one sit one millisecond apart.
func TestInstantDomain_Boundaries(t *testing.T) {
	instant := instantDomainModes[0]
	cases := []struct {
		name   string
		query  string
		accept bool
	}{
		// 00:06:00 - 5m range - 1m step = 1900-01-01T00:00:00Z.
		{"subquery_lowest_accepted", `count_over_time(vector(1)[5m:1m] @ -2208988440)`, true},
		{"subquery_highest_rejected", `count_over_time(vector(1)[5m:1m] @ -2208988440.001)`, false},
		// A subquery without a step uses the 1m default.
		{"default_step_lowest_accepted", `count_over_time(vector(1)[5m:] @ -2208988440)`, true},
		{"default_step_highest_rejected", `count_over_time(vector(1)[5m:] @ -2208988440.001)`, false},
		// 00:05:00 - 5m lookback = 1900-01-01T00:00:00Z.
		{"selector_lowest_accepted", `m @ -2208988500`, true},
		{"selector_highest_rejected", `m @ -2208988500.001`, false},
		// 23:46:16.854 + 1m step = 23:47:16.854, below the int64 ceiling.
		{"subquery_highest_accepted", `count_over_time(vector(1)[5m:1m] @ 9223371976.854)`, true},
		{"subquery_lowest_rejected", `count_over_time(vector(1)[5m:1m] @ 9223371976.855)`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := lowerInstantDomainCase(t, tc.query, instant)
			if tc.accept && err != nil {
				t.Fatalf("lower %q: unexpected error %v", tc.query, err)
			}
			if !tc.accept && (err == nil || !strings.Contains(err.Error(), "outside the range cerberus supports")) {
				t.Fatalf("lower %q: err = %v, want the instant-domain rejection", tc.query, err)
			}
		})
	}
}

// TestInstantDomain_AnswersEmptyReadsInSQL pins the rewrite of a read whose
// window lies wholly outside what storage holds. Such a selector reads no
// sample, so the expression it makes empty is replaced by `vector(0) < -1`
// and everything around it still lowers to SQL.
func TestInstantDomain_AnswersEmptyReadsInSQL(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{`m @ -62135596800`, `vector(0) < -1`},
		{`count_over_time(m[5m:1m] @ -62135596800)`, `vector(0) < -1`},
		{`max_over_time(rate(m[5m])[10m:1m] @ -62135596800)`, `vector(0) < -1`},
		{`sum by (job) (rate(m[5m] @ 10000000000))`, `sum by (job) (vector(0) < -1)`},
		{`count_over_time(m[5m:1m] @ 100000000000000000)`, `vector(0) < -1`},
		{`m @ -62135596800 or vector(7)`, `vector(0) < -1 or vector(7)`},
		{`scalar(m @ -62135596800)`, `scalar(vector(0) < -1)`},
		{`-(m offset -250y)`, `-(vector(0) < -1)`},
	}
	for _, tc := range cases {
		for _, m := range instantDomainModes {
			t.Run(tc.query+"/"+m.name, func(t *testing.T) {
				expr := mustParseExperimental(t, tc.query)
				before := expr.String()
				ctx := lowerCtx{start: m.start, end: instantDomainEvalEnd, step: m.step}
				got, err := fitInstantDomain(expr, ctx)
				if err != nil {
					t.Fatalf("fitInstantDomain(%q): %v", tc.query, err)
				}
				if got.String() != tc.want {
					t.Fatalf("fitInstantDomain(%q) = %q, want %q", tc.query, got.String(), tc.want)
				}
				if expr.String() != before {
					t.Fatalf("fitInstantDomain modified its input: %q became %q", before, expr.String())
				}
				if err := lowerInstantDomainCase(t, tc.query, m); err != nil {
					t.Fatalf("lower %q: %v", tc.query, err)
				}
			})
		}
	}
}

// TestInstantDomain_InDomainQueryIsUntouched pins that a query whose
// instants all lie inside the domain is lowered exactly as written.
func TestInstantDomain_InDomainQueryIsUntouched(t *testing.T) {
	for _, q := range []string{
		`rate(m[5m])`,
		`count_over_time(m[5m:1m] @ 1767225600)`,
		`absent(m @ 1767225600) or info(m, {job="a"})`,
		`-sum by (job) (rate(m[5m]))`,
		`topk(5, m)`,
		`(m)`,
	} {
		expr := mustParseExperimental(t, q)
		got, err := fitInstantDomain(expr, lowerCtx{start: instantDomainEvalEnd, end: instantDomainEvalEnd})
		if err != nil {
			t.Fatalf("fitInstantDomain(%q): %v", q, err)
		}
		if got != expr {
			t.Fatalf("fitInstantDomain(%q) rewrote an in-domain query into %q", q, got)
		}
	}
}

// TestInstantDomain_NoContextChecksOnlyAbsolutePins pins the bare [Lower]
// path: with no request instant an unpinned selector renders now64(9), so
// only an absolute pin is judged.
func TestInstantDomain_NoContextChecksOnlyAbsolutePins(t *testing.T) {
	if _, err := Lower(context.Background(), mustParseExperimental(t, `rate(m[5m] offset 1h)`), schema.DefaultOTelMetrics()); err != nil {
		t.Fatalf("Lower(unpinned): %v", err)
	}
	_, err := Lower(context.Background(), mustParseExperimental(t, `count_over_time(vector(1)[5m:1m] @ -62135596800)`), schema.DefaultOTelMetrics())
	if err == nil || !strings.Contains(err.Error(), "outside the range cerberus supports") {
		t.Fatalf("Lower(year-one pin): err = %v, want the instant-domain rejection", err)
	}
}

// TestEvalAnchor_ZeroTimePinIsAnchored pins that "no anchor" is its own
// state, not the zero time.Time: an `@` pin at 0001-01-01T00:00:00Z
// (`@ -62135596800`) is Go's zero time and is still a pin.
func TestEvalAnchor_ZeroTimePinIsAnchored(t *testing.T) {
	vs := mustParseExperimental(t, `m @ -62135596800`).(*parser.VectorSelector)
	a, err := anchorFromSelector(vs, lowerCtx{})
	if err != nil {
		t.Fatalf("anchorFromSelector: %v", err)
	}
	if !a.anchored() || !a.End().IsZero() {
		t.Fatalf("selector pinned at the zero time: anchored=%v End=%v, want anchored at the zero time", a.anchored(), a.End())
	}

	sub := mustParseExperimental(t, `vector(1)[5m:1m] @ -62135596800`).(*parser.SubqueryExpr)
	sa, err := subqueryAnchor(sub, lowerCtx{})
	if err != nil {
		t.Fatalf("subqueryAnchor: %v", err)
	}
	if !sa.anchored() || !sa.End().IsZero() {
		t.Fatalf("subquery pinned at the zero time: anchored=%v End=%v, want anchored at the zero time", sa.anchored(), sa.End())
	}

	unpinned := mustParseExperimental(t, `m`).(*parser.VectorSelector)
	ua, err := anchorFromSelector(unpinned, lowerCtx{})
	if err != nil {
		t.Fatalf("anchorFromSelector(unpinned): %v", err)
	}
	if ua.anchored() {
		t.Fatalf("unpinned selector with no request instant reports an anchor at %v", ua.End())
	}
}

// TestInstantDomain_EmptyReadEmitsNoOutOfDomainInstant pins that the SQL for
// a rewritten read carries no literal from the pinned century.
func TestInstantDomain_EmptyReadEmitsNoOutOfDomainInstant(t *testing.T) {
	for _, m := range instantDomainModes {
		plan, err := LowerAtRange(context.Background(), mustParseExperimental(t, `count_over_time(m[5m:1m] @ -62135596800)`),
			schema.DefaultOTelMetrics(), m.start, instantDomainEvalEnd, m.step)
		if err != nil {
			t.Fatalf("%s: lower: %v", m.name, err)
		}
		sql, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("%s: emit: %v", m.name, err)
		}
		texts := []string{sql}
		for _, a := range args {
			if s, ok := a.(string); ok {
				texts = append(texts, s)
			}
		}
		for _, text := range texts {
			if strings.Contains(text, "0001-") || strings.Contains(text, "0000-") {
				t.Fatalf("%s: SQL carries a year-one instant in %q:\n%s", m.name, text, sql)
			}
		}
	}
}

// TestInstantDomain_RangeStepMargin pins that a range query's own step
// widens every window it evaluates, on top of a subquery's step.
func TestInstantDomain_RangeStepMargin(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		start  string
		accept bool
	}{
		// start - 5m lookback - 1m query step = 1900-01-01T00:00:00Z.
		{"selector_lowest_accepted", `m`, "1900-01-01T00:06:00Z", true},
		{"selector_highest_rejected", `m`, "1900-01-01T00:05:59.999Z", false},
		// start - 5m range - 1m subquery step - 1m query step.
		{"subquery_lowest_accepted", `count_over_time(vector(1)[5m:1m])`, "1900-01-01T00:07:00Z", true},
		{"subquery_highest_rejected", `count_over_time(vector(1)[5m:1m])`, "1900-01-01T00:06:59.999Z", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := mustUTC(t, tc.start)
			_, err := LowerAtRange(context.Background(), mustParseExperimental(t, tc.query), schema.DefaultOTelMetrics(),
				start, start.Add(time.Hour), time.Minute)
			if tc.accept && err != nil {
				t.Fatalf("lower %q from %s: unexpected error %v", tc.query, tc.start, err)
			}
			if !tc.accept && (err == nil || !strings.Contains(err.Error(), "outside the range cerberus supports")) {
				t.Fatalf("lower %q from %s: err = %v, want the instant-domain rejection", tc.query, tc.start, err)
			}
		})
	}
}

// TestInstantDomain_StepInvariantIsUntouched pins the StepInvariantExpr
// arm, a node the engine adds and the parser never produces.
func TestInstantDomain_StepInvariantIsUntouched(t *testing.T) {
	ctx := lowerCtx{start: instantDomainEvalEnd, end: instantDomainEvalEnd}
	in := &parser.StepInvariantExpr{Expr: mustParseExperimental(t, `m`)}
	got, err := fitInstantDomain(in, ctx)
	if err != nil || got != in {
		t.Fatalf("fitInstantDomain(in-domain step-invariant) = %v, %v; want the input unchanged", got, err)
	}
	out := &parser.StepInvariantExpr{Expr: mustParseExperimental(t, `m @ -62135596800`)}
	got, err = fitInstantDomain(out, ctx)
	if err != nil || got.String() != `vector(0) < -1` {
		t.Fatalf("fitInstantDomain(year-one step-invariant) = %v, %v; want vector(0) < -1", got, err)
	}
	if out.Expr.String() != `m @ -62135596800.000` {
		t.Fatalf("fitInstantDomain modified its input into %q", out.Expr)
	}
}

// TestInstantDomain_EmptinessRules pins which compositions of an empty read
// are themselves empty. An empty subquery inner lets the whole pinned
// subquery be replaced; a non-empty one leaves it to the rejection.
func TestInstantDomain_EmptinessRules(t *testing.T) {
	const y1 = `m @ -62135596800`
	cases := []struct {
		query string
		want  string // the rewritten query; empty means a rejection
	}{
		{`count_over_time((` + y1 + ` or ` + y1 + `)[5m:1m] @ -62135596800)`, `vector(0) < -1`},
		{`count_over_time((` + y1 + ` or vector(1))[5m:1m] @ -62135596800)`, ``},
		{`count_over_time((vector(1) or ` + y1 + `)[5m:1m] @ -62135596800)`, ``},
		{`count_over_time((` + y1 + ` unless vector(1))[5m:1m] @ -62135596800)`, `vector(0) < -1`},
		{`count_over_time((vector(1) unless ` + y1 + `)[5m:1m] @ -62135596800)`, ``},
		{`count_over_time((vector(1) and ` + y1 + `)[5m:1m] @ -62135596800)`, `vector(0) < -1`},
		{`count_over_time((` + y1 + ` * vector(1))[5m:1m] @ -62135596800)`, `vector(0) < -1`},
		{`count_over_time((` + y1 + ` * 2)[5m:1m] @ -62135596800)`, `vector(0) < -1`},
		{`count_over_time((2 * ` + y1 + `)[5m:1m] @ -62135596800)`, `vector(0) < -1`},
		{`count_over_time((vector(1) * 2)[5m:1m] @ -62135596800)`, ``},
		{`count_over_time((2 * vector(1))[5m:1m] @ -62135596800)`, ``},
		// Only the first series argument decides a call's emptiness.
		{`quantile_over_time(0.5, m[5m] @ -62135596800)`, `vector(0) < -1`},
		{`histogram_quantile(0.9, rate(m[5m] @ -62135596800))`, `histogram_quantile(0.9, vector(0) < -1)`},
		{`count_over_time(histogram_quantile(0.9, rate(m[5m]))[5m:1m] @ -62135596800)`, `vector(0) < -1`},
		{`absent_over_time(m[5m] @ -62135596800)`, ``},
		// info's first argument may be replaced; its second must stay a selector.
		{`info(` + y1 + `, {job="a"})`, `info(vector(0) < -1, {job="a"})`},
		{`info(m, {job="a"} @ -62135596800)`, ``},
		// An aggregation's parameter is fitted like any argument.
		{`topk(5, ` + y1 + `)`, `topk(5, vector(0) < -1)`},
		{`topk(scalar(` + y1 + `), m)`, `topk(scalar(vector(0) < -1), m)`},
		// A window reaching back into storage is not empty: the pin is past
		// the ceiling but its lookback, or its subquery range, is not.
		{`m @ 9223372096.854`, ``},
		{`count_over_time(m[1h:1m] @ 9223373836.854)`, ``},
		{`count_over_time(m[5m:1m] @ 9223373836.854)`, `vector(0) < -1`},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			expr := mustParseExperimental(t, tc.query)
			got, err := fitInstantDomain(expr, lowerCtx{start: instantDomainEvalEnd, end: instantDomainEvalEnd})
			switch {
			case tc.want == "" && err == nil:
				t.Fatalf("fitInstantDomain(%q) = %q, want the instant-domain rejection", tc.query, got)
			case tc.want == "" && !strings.Contains(err.Error(), "outside the range cerberus supports"):
				t.Fatalf("fitInstantDomain(%q): err = %v, want the instant-domain rejection", tc.query, err)
			case tc.want != "" && err != nil:
				t.Fatalf("fitInstantDomain(%q): %v", tc.query, err)
			case tc.want != "" && got.String() != tc.want:
				t.Fatalf("fitInstantDomain(%q) = %q, want %q", tc.query, got.String(), tc.want)
			}
		})
	}
}

// TestInstantDomain_FirstErrorWins pins that a call with two arguments
// outside the domain reports the first one.
func TestInstantDomain_FirstErrorWins(t *testing.T) {
	q := `predict_linear(m[5m] @ -5000000000, scalar(m @ -6000000000))`
	_, err := fitInstantDomain(mustParseExperimental(t, q), lowerCtx{start: instantDomainEvalEnd, end: instantDomainEvalEnd})
	want := instantDomainError(time.UnixMilli(-5000000000000)).Error()
	if err == nil || err.Error() != want {
		t.Fatalf("fitInstantDomain(%q): err = %v, want %q", q, err, want)
	}
}

// TestInstantDomain_StartEndPinsWithoutRange pins that `@ start()` and
// `@ end()` with no request range are left to the lowering's own
// "requires query range context" error rather than judged as instants.
func TestInstantDomain_StartEndPinsWithoutRange(t *testing.T) {
	for _, q := range []string{`m @ start()`, `m @ end()`, `count_over_time(vector(1)[5m:1m] @ start())`} {
		expr := mustParseExperimental(t, q)
		if _, err := fitInstantDomain(expr, lowerCtx{}); err != nil {
			t.Fatalf("fitInstantDomain(%q) with no request range: %v", q, err)
		}
		_, err := Lower(context.Background(), expr, schema.DefaultOTelMetrics())
		if err == nil || !strings.Contains(err.Error(), "requires query range context") {
			t.Fatalf("Lower(%q): err = %v, want the query-range-context error", q, err)
		}
	}
}
