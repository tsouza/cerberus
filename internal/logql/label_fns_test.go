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

	// Ten capture groups, so `$10` resolves to a group that exists and
	// is above ClickHouse's `\9` substitution ceiling.
	const q = `label_replace(sum by (host) (count_over_time({app="a"}[5m])), ` +
		`"service", "$10", "host", "(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)")`

	expr, err := syntax.ParseExpr(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Lower(context.Background(), expr, schema.DefaultOTelLogs()); err == nil {
		t.Fatal("lowering accepted a replacement above ClickHouse's substitution ceiling; want an error")
	} else if !strings.Contains(err.Error(), "label_replace") {
		t.Fatalf("error does not name the function that failed: %v", err)
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
