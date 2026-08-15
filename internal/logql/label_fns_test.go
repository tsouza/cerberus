package logql

import (
	"context"
	"strings"
	"testing"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerLabelReplace_RejectsInexpressibleBackref is the LogQL twin
// of the PromQL test with the same name. Both heads route their
// replacement template through `qlcommon.ReplacementToCH`, so both must
// surface its refusal as a query error instead of emitting a plan whose
// destination label carries the wrong text.
func TestLowerLabelReplace_RejectsInexpressibleBackref(t *testing.T) {
	t.Parallel()

	// A capture-group NAME two capture groups share, where the first
	// carrier can take part in a match capturing NOTHING while the second
	// captures text: Go's ExpandString stops at the first participant and
	// expands to its empty capture, and `extractGroups` renders "took part
	// matching empty" exactly like "took no part", so SQL cannot observe
	// which. On `host="b"` reference Loki answers `service=""` where the
	// emitted search would answer `service="b"`.
	//
	// The repeated alternation can take two branches in one match, so a
	// sibling probe only records that a sibling took part at some point,
	// not whether this carrier took part on the final pass.
	// Issue #1956 tracks that residue.
	const q = `label_replace(sum by (host) (count_over_time({app="a"}[5m])), ` +
		`"service", "$dup", "host", "(?:(?P<dup>a?)|y)*(?P<dup>b)")`

	expr, err := syntax.ParseExpr(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Lower(context.Background(), expr, schema.DefaultOTelLogs()); err == nil {
		t.Fatal("lowering accepted a carrier whose participation is unobservable; want an error")
	} else if !strings.Contains(err.Error(), "label_replace") {
		t.Fatalf("error does not name the function that failed: %v", err)
	}
}

// TestLowerLabelReplace_AcceptsSharedCaptureName is the LogQL twin of the
// PromQL test with the same name, one row per fact the shared-name
// resolution reasons from. Each lowers to an `extractGroups` search that
// reproduces Go's expansion exactly, and each would have been refused by a
// guard that keyed only on nullability.
func TestLowerLabelReplace_AcceptsSharedCaptureName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		regex string
	}{
		// Every carrier is non-nullable, so taking part in the match and
		// capturing a non-empty string are the same event.
		{"all_carriers_non_nullable", `(?P<dup>a)|(?P<dup>b)`},
		// A nullable carrier in a different branch of one alternation from
		// the other: the two never take part in the same match.
		{"nullable_carrier_in_an_exclusive_branch", `(?P<dup>a?)|(?P<dup>b)`},
		// A nullable LAST carrier: nothing after it to skip ahead to.
		{"nullable_last_carrier", `(?P<dup>a)|(?P<dup>b?)`},
		// A nullable carrier every match must pass through: Go always
		// stops at it, so the search truncates there.
		{"nullable_carrier_on_the_mandatory_spine", `(?P<dup>a?)(?P<dup>b)`},
		// A nullable carrier a match can skip, cleared by rewriting the
		// regex to probe the mandatory text in the quest's body.
		{"nullable_skippable_carrier_with_a_probeable_ancestor", `(?:x(?P<dup>a?))?(?P<dup>b)`},
		// A mandatory alternation with non-empty sibling branches can use
		// their absence as the nullable carrier's participation witness.
		{"nullable_carrier_with_negative_sibling_probe", `(?:(?P<dup>a?)|(?P<dup>b))(?P<dup>c)`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q := `label_replace(sum by (host) (count_over_time({app="a"}[5m])), ` +
				`"service", "$dup", "host", "` + tc.regex + `")`

			expr, err := syntax.ParseExpr(q)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if _, err := Lower(context.Background(), expr, schema.DefaultOTelLogs()); err != nil {
				t.Fatalf("lowering rejected an expressible shared capture-group name: %v", err)
			}
		})
	}
}

// TestLowerLabelReplace_AcceptsBackrefAboveCHCeiling is the LogQL twin of
// the PromQL test with the same name: reference Loki expands `$10` to the
// tenth capture group, and this head now does too, via the template
// decomposition the emitter renders over `extractGroups` rather than a
// `\10` backref ClickHouse has no slot for.
func TestLowerLabelReplace_AcceptsBackrefAboveCHCeiling(t *testing.T) {
	t.Parallel()

	const q = `label_replace(sum by (host) (count_over_time({app="a"}[5m])), ` +
		`"service", "$10", "host", "(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)")`

	expr, err := syntax.ParseExpr(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Lower(context.Background(), expr, schema.DefaultOTelLogs()); err != nil {
		t.Fatalf("lowering rejected a backref above ClickHouse's substitution ceiling: %v", err)
	}
}

// TestLowerLabelReplace_AcceptsNamedCapture is the negative control:
// a template the emitter can express must still lower cleanly.
func TestLowerLabelReplace_AcceptsNamedCapture(t *testing.T) {
	t.Parallel()

	const q = `label_replace(sum by (host) (count_over_time({app="a"}[5m])), ` +
		`"service", "svc-${svc}", "host", "host-(?P<svc>.*)")`

	expr, err := syntax.ParseExpr(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Lower(context.Background(), expr, schema.DefaultOTelLogs()); err != nil {
		t.Fatalf("lowering rejected an expressible named backref: %v", err)
	}
}
