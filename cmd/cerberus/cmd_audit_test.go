package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chaudit"
)

// TestAuditCmd_IsRegisteredOnRoot pins that `cerberus audit` is REACHABLE.
// internal/chaudit shipped as a library first, and a library nothing can
// invoke is not the "live-deployment audit mode" #2679 asked for — every one
// of its own tests passed while the feature was unreachable, so the gap this
// test closes is exactly the one unit tests cannot see.
func TestAuditCmd_IsRegisteredOnRoot(t *testing.T) {
	t.Parallel()

	root := newRootCmd(func() error { return nil })
	for _, c := range root.Commands() {
		if c.Name() == "audit" {
			return
		}
	}
	t.Fatal("`cerberus audit` is not registered on the root command; the audit\n" +
		"package would be unreachable from the binary")
}

// TestAuditCmd_RejectsUnusableOptionsBeforeDialing pins that a bad invocation
// fails on its own arguments rather than after opening a ClickHouse
// connection. Validation ordering is the behaviour: an operator running this
// against a production deployment should not cause a dial to discover that
// --anchors was zero.
func TestAuditCmd_RejectsUnusableOptionsBeforeDialing(t *testing.T) {
	t.Parallel()

	root := newRootCmd(func() error { return nil })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"audit", "--table", "t", "--budget", "100", "--anchors", "0"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected --anchors=0 to be rejected")
	}
	if !strings.Contains(err.Error(), "Anchors must be > 0") {
		t.Fatalf("error should name the offending option, got: %v", err)
	}
}

// TestAuditOverBudgetError_MapsToGateExitCode pins the exit code an operator
// gates a deploy on. An over-budget metric means the tool worked and the
// answer is "no", which must be distinguishable from exit 1 (the tool broke) —
// otherwise a CI gate cannot tell a real finding from a crash.
func TestAuditOverBudgetError_MapsToGateExitCode(t *testing.T) {
	t.Parallel()

	got := exitCodeForError(&auditOverBudgetError{count: 2})
	if got != verifyExitFail {
		t.Errorf("exitCodeForError(auditOverBudgetError) = %d, want %d", got, verifyExitFail)
	}
	if exitCodeForError(errors.New("boom")) == verifyExitFail {
		t.Error("an ordinary error must NOT share the gate exit code, or a crash\n" +
			"reads as a clean audit finding")
	}
}

// TestWriteAuditReport_EmitsTheFactorsIndividually pins that the report keeps
// series / rawRows / bucketWidth separate. They have different remedies —
// width is fixed at the instrumentation, rows by retention, series by the
// label set — so a summary that collapsed them into the cost alone would not
// be actionable.
func TestWriteAuditReport_EmitsTheFactorsIndividually(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	report := chaudit.Report{
		SchemaVersion: chaudit.ReportVersion,
		Table:         "otel_metrics_histogram",
		Metrics: []chaudit.MetricAudit{{
			Metric: "http_request_duration_seconds",
			Series: 7110, RawRows: 512, BucketWidth: 68,
			CostUnits: 1468, Budget: 1000, HeadroomPct: -46.8,
			AmplifyingLabel: "k8s_pod_name", AmplificationRatio: 72.5,
		}},
	}
	if err := writeAuditReport(&buf, report); err != nil {
		t.Fatalf("writeAuditReport: %v", err)
	}
	var back chaudit.Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	m := back.Metrics[0]
	if m.Series != 7110 || m.RawRows != 512 || m.BucketWidth != 68 {
		t.Errorf("the three cost factors must survive the round trip, got %+v", m)
	}
	if m.AmplifyingLabel != "k8s_pod_name" {
		t.Errorf("AmplifyingLabel = %q, want k8s_pod_name — the actionable half of the report", m.AmplifyingLabel)
	}
}
