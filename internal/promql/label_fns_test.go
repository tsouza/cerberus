package promql

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerLabelReplace_RejectsInexpressibleBackref pins the head's
// error path. `qlcommon.ReplacementToCH` refuses the handful of
// replacement templates ClickHouse's fixed `\0`-`\9` substitution
// cannot carry; lowering must surface that as a query error rather
// than fall through and emit a plan whose destination label silently
// holds the wrong text.
func TestLowerLabelReplace_RejectsInexpressibleBackref(t *testing.T) {
	t.Parallel()

	// Ten capture groups, so `$10` resolves to a group that exists and
	// is above the `\9` ceiling.
	const q = `label_replace(temperature, "service", "$10", "host", "(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)")`

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Lower(context.Background(), expr, schema.DefaultOTelMetrics()); err == nil {
		t.Fatal("lowering accepted a replacement above ClickHouse's substitution ceiling; want an error")
	} else if !strings.Contains(err.Error(), "label_replace") {
		t.Fatalf("error does not name the function that failed: %v", err)
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
