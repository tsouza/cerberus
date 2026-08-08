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
// several groups share where at least one of them can match the empty
// string. Go picks the first carrier that took part in the row's match,
// and `extractGroups` renders "took part matching empty" and "took no
// part" identically, so which one Go would pick is unobservable from
// SQL. Lowering must surface that as a query error rather than fall
// through and emit a plan whose destination label silently holds the
// wrong text.
func TestLowerLabelReplace_RejectsInexpressibleBackref(t *testing.T) {
	t.Parallel()

	const q = `label_replace(temperature, "service", "$dup", "host", "(?P<dup>a?)|(?P<dup>b)")`

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Lower(context.Background(), expr, schema.DefaultOTelMetrics()); err == nil {
		t.Fatal("lowering accepted a nullable capture group sharing a name; want an error")
	} else if !strings.Contains(err.Error(), "label_replace") {
		t.Fatalf("error does not name the function that failed: %v", err)
	}
}

// TestLowerLabelReplace_AcceptsSharedCaptureName is the boundary's other
// side. When EVERY group sharing the name is non-nullable, taking part
// in the match and capturing a non-empty string are the same event, so
// "the first carrier Go picks" is exactly "the first carrier whose
// capture is non-empty" — which `arrayFirst` over `extractGroups` says
// directly. This shape must lower rather than be refused along with the
// nullable one.
func TestLowerLabelReplace_AcceptsSharedCaptureName(t *testing.T) {
	t.Parallel()

	const q = `label_replace(temperature, "service", "$dup", "host", "(?P<dup>a)|(?P<dup>b)")`

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Lower(context.Background(), expr, schema.DefaultOTelMetrics()); err != nil {
		t.Fatalf("lowering rejected a shared capture-group name whose carriers are all non-nullable: %v", err)
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
