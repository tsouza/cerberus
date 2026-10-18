package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// atModifierInstantEnd is the eval instant every case below lowers at.
var atModifierInstantEnd = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// atModifierInstantRejection is the substring of [atModifierInstant]'s
// rejection every out-of-window case must surface.
const atModifierInstantRejection = "outside the int64-nanosecond range cerberus evaluates in"

// The `@` timestamps the cases pin to, in the Unix seconds a query
// spells them in.
const (
	// atGoZeroTime is 0001-01-01T00:00:00Z — Go's zero time.Time,
	// which the lowering also uses to mean "no anchor".
	atGoZeroTime = "-62135596800"
	// atBeforeWindow is 1675-04-18: storable in Prometheus's int64
	// milliseconds, below cerberus's int64-nanosecond floor.
	atBeforeWindow = "-9300000000"
	// atAfterWindow is 2262-04-12: just past the int64-nanosecond ceiling.
	atAfterWindow = "9300000000"
	// atFirstStorable / atLastStorable are the outermost whole
	// milliseconds inside the window, whose edges are
	// time.Unix(0, math.MinInt64) and time.Unix(0, math.MaxInt64).
	atFirstStorable = "-9223372036.854"
	atLastStorable  = "9223372036.854"
	// atBelowFirstStorable / atAboveLastStorable are one millisecond
	// past each edge.
	atBelowFirstStorable = "-9223372036.855"
	atAboveLastStorable  = "9223372036.855"
	// atInWindow is 2026-01-01T12:00:00Z, an ordinary pin.
	atInWindow = "1767268800"
)

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

// TestAtModifierInstant_OutsideWindowIsRejected pins the answer to an
// `@` pin cerberus cannot evaluate: a rejection naming the cause, in
// both grid modes, on every path that resolves a pin.
//
// The Go zero time is the case that matters most. It is also the
// lowering's "no anchor" marker, so before the rejection a subquery
// pinned there took the unpinned path and silently answered over the
// request window (or over now64(9)) instead of at the pin. The two
// other out-of-window instants wrapped `UnixNano()` and answered over a
// grid in an unrelated century.
func TestAtModifierInstant_OutsideWindowIsRejected(t *testing.T) {
	t.Parallel()
	for _, at := range []string{atGoZeroTime, atBeforeWindow, atAfterWindow, atBelowFirstStorable, atAboveLastStorable} {
		for _, shape := range atModifierInstantShapes {
			query := strings.Replace(shape, "%s", at, 1)
			for _, rangeMode := range []bool{false, true} {
				err := lowerAtModifierCase(t, query, rangeMode)
				if err == nil {
					t.Errorf("lower(%q, range=%v): got no error, want a rejection of the unevaluable @ instant", query, rangeMode)
					continue
				}
				if !strings.Contains(err.Error(), atModifierInstantRejection) {
					t.Errorf("lower(%q, range=%v): got error %q, want it to contain %q", query, rangeMode, err, atModifierInstantRejection)
				}
			}
		}
	}
}

// TestAtModifierInstant_WindowEdgesAreAccepted is the positive control
// for the rejection above: the outermost instants inside the window, and
// an ordinary one, lower without error on the same shapes. Without it,
// a rejection that fired on every `@` would pass the test above.
func TestAtModifierInstant_WindowEdgesAreAccepted(t *testing.T) {
	t.Parallel()
	for _, at := range []string{atFirstStorable, atLastStorable, atInWindow} {
		for _, shape := range atModifierInstantShapes {
			query := strings.Replace(shape, "%s", at, 1)
			for _, rangeMode := range []bool{false, true} {
				if err := lowerAtModifierCase(t, query, rangeMode); err != nil {
					t.Errorf("lower(%q, range=%v): got error %q for an instant inside the window", query, rangeMode, err)
				}
			}
		}
	}
}
