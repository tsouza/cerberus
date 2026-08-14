package promql

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerLabelReplace_RejectsInexpressibleBackref pins the head's
// error path. `qlcommon.ReplacementToCH` refuses the one replacement
// template no ClickHouse expression can carry — a capture-group NAME
// several groups share where the first carrier to take part in a match
// can capture the empty string while a LATER carrier in that same match
// captures text. Go picks the first carrier that took part, and
// `extractGroups` renders "took part matching empty" and "took no part"
// identically, so which one Go would pick is unobservable from SQL.
// Lowering must surface that as a query error rather than fall through
// and emit a plan whose destination label silently holds the wrong text.
//
// The regex below is the smallest shape that really is unobservable, and
// it carries its own proof: on `host="b"` the alternation takes its empty
// first branch, so `dup` takes part capturing nothing and Prometheus
// answers `service=""`, while the emitted search answers `service="b"`.
//
// A carrier under a quest is NOT enough on its own any more — where the
// carrier has an ancestor that cannot match empty, cerberus rewrites the
// regex to probe it and lowers the query, which
// TestLowerLabelReplace_AcceptsSharedCaptureName covers. What is left is a
// carrier alone in a nullable branch, with nothing above it guaranteed to
// be consumed. Issue #1956 tracks that residue, and
// ClickHouse/ClickHouse#114733 tracks the upstream gap behind it.
func TestLowerLabelReplace_RejectsInexpressibleBackref(t *testing.T) {
	t.Parallel()

	const q = `label_replace(temperature, "service", "$dup", "host", "(?:(?P<dup>a?)|y)(?P<dup>b)")`

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Lower(context.Background(), expr, schema.DefaultOTelMetrics()); err == nil {
		t.Fatal("lowering accepted a carrier whose participation is unobservable; want an error")
	} else if !strings.Contains(err.Error(), "label_replace") {
		t.Fatalf("error does not name the function that failed: %v", err)
	}
}

// TestLowerLabelReplace_AcceptsSharedCaptureName is the boundary's other
// side, one row per fact the resolution reasons from. Each of these
// lowers to a `extractGroups` search that reproduces Go's expansion
// exactly, and each would have been refused by a guard that keyed only on
// nullability.
func TestLowerLabelReplace_AcceptsSharedCaptureName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
	}{
		{
			// Every carrier is non-nullable, so taking part in the match
			// and capturing a non-empty string are the same event.
			"all_carriers_non_nullable",
			`label_replace(temperature, "service", "$dup", "host", "(?P<dup>a)|(?P<dup>b)")`,
		},
		{
			// A nullable carrier in a different branch of one alternation
			// from the other: the two never take part in the same match,
			// so an empty capture by the first means the second is empty
			// too and both searches answer "".
			"nullable_carrier_in_an_exclusive_branch",
			`label_replace(temperature, "service", "$dup", "host", "(?P<dup>a?)|(?P<dup>b)")`,
		},
		{
			// A nullable LAST carrier: there is nothing after it for a
			// first-non-empty search to wrongly skip ahead to.
			"nullable_last_carrier",
			`label_replace(temperature, "service", "$dup", "host", "(?P<dup>a)|(?P<dup>b?)")`,
		},
		{
			// A nullable carrier every match must pass through: Go always
			// stops at it, so the search truncates there and the later
			// carrier is unreachable.
			"nullable_carrier_on_the_mandatory_spine",
			`label_replace(temperature, "service", "$dup", "host", "(?P<dup>a?)(?P<dup>b)")`,
		},
		{
			// A nullable carrier a match CAN skip, which no static fact
			// clears: the quest's body holds mandatory text, so cerberus
			// rewrites the regex to capture that text and reads the
			// carrier's participation off it. This shape was rejected
			// until probes existed.
			"nullable_skippable_carrier_with_a_probeable_ancestor",
			`label_replace(temperature, "service", "$dup", "host", "(?:x(?P<dup>a?))?(?P<dup>b)")`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := parser.NewParser(parser.Options{}).ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if _, err := Lower(context.Background(), expr, schema.DefaultOTelMetrics()); err != nil {
				t.Fatalf("lowering rejected an expressible shared capture-group name: %v", err)
			}
		})
	}
}

// TestLowerLabelReplace_AcceptsBackrefAboveCHCeiling pins the shape this
// head used to reject: reference Prometheus expands `$10` to the tenth
// group, and cerberus now does too, by decomposing the template so the
// emitter can index `extractGroups` instead of spelling a `\10` backref
// ClickHouse has no slot for.
func TestLowerLabelReplace_AcceptsBackrefAboveCHCeiling(t *testing.T) {
	t.Parallel()

	const q = `label_replace(temperature, "service", "$10", "host", "(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)")`

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Lower(context.Background(), expr, schema.DefaultOTelMetrics()); err != nil {
		t.Fatalf("lowering rejected a backref above ClickHouse's substitution ceiling: %v", err)
	}
}

// TestLowerLabelReplace_AcceptsNamedCapture is the negative control for
// the test above: a template the emitter *can* express must still lower
// cleanly, so the rejection above is evidence of a real boundary rather
// than of label_replace being broken outright.
func TestLowerLabelReplace_AcceptsNamedCapture(t *testing.T) {
	t.Parallel()

	const q = `label_replace(temperature, "service", "svc-${svc}", "host", "host-(?P<svc>.*)")`

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Lower(context.Background(), expr, schema.DefaultOTelMetrics()); err != nil {
		t.Fatalf("lowering rejected an expressible named backref: %v", err)
	}
}
