package promql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// atModifierInstantEnd is the eval instant every case below lowers at.
var atModifierInstantEnd = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// The `@` timestamps the cases pin to, in the Unix seconds a query
// spells them in.
const (
	// atGoZeroTime is 0001-01-01T00:00:00Z — Go's zero time.Time,
	// which the lowering also uses to mean "no anchor".
	atGoZeroTime = "-62135596800"
	// atBeforeWindow is 1675-04-18: storable in Prometheus's int64
	// milliseconds, below the int64-nanosecond floor.
	atBeforeWindow = "-9300000000"
	// atAfterWindow is 2262-04-12: just past the int64-nanosecond ceiling.
	atAfterWindow = "9300000000"
	// atFirstNano / atLastNano are the outermost whole milliseconds
	// inside the int64-nanosecond window, outside DateTime64(9)'s.
	atFirstNano = "-9223372036.854"
	atLastNano  = "9223372036.854"
	// atBefore1900 is 1899-12-31T00:00:00Z, inside the int64-nanosecond
	// window but below DateTime64(9)'s floor.
	atBefore1900 = "-2209075200"
	// atInWindow is 2026-01-01T12:00:00Z, an ordinary pin.
	atInWindow = "1767268800"
)

// unixSeconds spells t as the Unix seconds a query's `@` takes.
func unixSeconds(t time.Time) string {
	return fmt.Sprintf("%d", t.Unix())
}

// atModifierInstantShapes are the query shapes, each with a %s for the
// `@` timestamp, that route a pin through every path resolving one:
// the selector seam ([anchorFromSelector]) and each subquery guard that
// reads [subqueryAnchor] — [subqueryGridCtx], [lowerSubqueryOverAbsent],
// [lowerSubqueryOverCallSubquery] and [widenNestedCallSubqueryInner].
var atModifierInstantShapes = []string{
	`up @ %s`,
	`rate(up[5m] @ %s)`,
	`count_over_time(vector(1)[5m:1m] @ %s)`,
	`max_over_time((time() - up)[5m:1m] @ %s)`,
	`max_over_time(absent(up)[5m:1m] @ %s)`,
	`rate(((latency_exp_hist) and (latency_exp_hist))[2m:1m])[10m:1m] @ %s`,
	`sum_over_time(rate(((latency_exp_hist) or (num_cpus))[3m:1m])[4m:1m] @ %s)`,
}

// lowerAtModifierCase lowers query at [atModifierInstantEnd], as an
// instant query or as a five-step query_range.
func lowerAtModifierCase(t *testing.T, query string, rangeMode bool) error {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	s := schema.DefaultOTelMetrics()
	if rangeMode {
		_, err = LowerAtRange(context.Background(), expr, s,
			atModifierInstantEnd.Add(-5*time.Minute), atModifierInstantEnd, time.Minute)
		return err
	}
	_, err = LowerAt(context.Background(), expr, s, atModifierInstantEnd, atModifierInstantEnd)
	return err
}

// TestAtModifierInstant_OutsideDomainIsRefused pins the lowering's
// answer to an `@` pin outside the evaluation domain: a refusal naming
// the cause, in both grid modes, on every path that resolves a pin. The
// HTTP handlers never lower such a query — they ask
// LeavesEvaluationDomain first and answer it with the reference engine —
// so the refusal is what a direct lowering caller sees instead of a
// wrong plan.
//
// The Go zero time is the case that matters most. It is also the
// lowering's "no anchor" marker, so before the refusal a subquery
// pinned there took the unpinned path and silently answered over the
// request window (or over now64(9)) instead of at the pin. The other
// instants wrapped `UnixNano()` into an unrelated century, or were
// clamped to 1900 by ClickHouse's DateTime64(9) parse.
func TestAtModifierInstant_OutsideDomainIsRefused(t *testing.T) {
	t.Parallel()
	for _, at := range []string{atGoZeroTime, atBeforeWindow, atAfterWindow, atFirstNano, atLastNano, atBefore1900} {
		for _, shape := range atModifierInstantShapes {
			query := strings.Replace(shape, "%s", at, 1)
			for _, rangeMode := range []bool{false, true} {
				err := lowerAtModifierCase(t, query, rangeMode)
				if !errors.Is(err, ErrOutsideEvaluationDomain) {
					t.Errorf("lower(%q, range=%v): got %v, want ErrOutsideEvaluationDomain", query, rangeMode, err)
				}
			}
		}
	}
}

// TestAtModifierInstant_InsideDomainIsLowered is the positive control
// for the refusal above: pins an hour inside each edge of the domain,
// and an ordinary one, lower without error on the same shapes. Without
// it, a refusal that fired on every `@` would pass the test above.
func TestAtModifierInstant_InsideDomainIsLowered(t *testing.T) {
	t.Parallel()
	nearFirst := unixSeconds(evaluationDomainMin.Add(time.Hour))
	nearLast := unixSeconds(evaluationDomainMax.Add(-time.Hour))
	for _, at := range []string{nearFirst, nearLast, atInWindow} {
		for _, shape := range atModifierInstantShapes {
			query := strings.Replace(shape, "%s", at, 1)
			for _, rangeMode := range []bool{false, true} {
				if err := lowerAtModifierCase(t, query, rangeMode); err != nil {
					t.Errorf("lower(%q, range=%v): got error %q for an instant inside the domain", query, rangeMode, err)
				}
			}
		}
	}
}
