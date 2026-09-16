// Untagged companion to this package's three chdb-tagged per-head tests
// (promql_resource_chdb_test.go / logql_resource_chdb_test.go /
// traceql_resource_chdb_test.go). Those need libchdb.so and the `chdb`
// build tag (`just semantic-resource-fixture`); this file needs neither —
// it runs under the default `just test` / `go test ./...` gate — and pins
// the CONFIGURATION and NORMALIZATION half of SIGNAL-RESOURCE-001 directly
// against production code: that each head's schema override genuinely
// differs from that head's own default name, and that PromQL/LogQL's dotted
// -> underscored resource-label normalizer produces exactly the wire label
// the chdb-tagged tests assert on the wire.
package resourcefixture

import (
	"testing"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestResourceFixture_CustomSchemaNamesAreGenuineOverrides pins acceptance
// criterion "Custom names are exercised through configured schemas rather
// than default-name fixtures": each chdb test file's custom table name
// must differ from that head's own DefaultOTel* name, or the "custom
// override" leg of the contract would silently degrade into the default
// path it claims to exercise.
func TestResourceFixture_CustomSchemaNamesAreGenuineOverrides(t *testing.T) {
	if promqlFixtureMetricsTable == schema.DefaultOTelMetrics().SumTable {
		t.Errorf("PromQL fixture table %q equals the default SumTable name; not a genuine override", promqlFixtureMetricsTable)
	}
	if logqlFixtureLogsTable == schema.DefaultOTelLogs().LogsTable {
		t.Errorf("LogQL fixture table %q equals the default LogsTable name; not a genuine override", logqlFixtureLogsTable)
	}
	if traceqlFixtureTracesTable == schema.DefaultOTelTraces().SpansTable {
		t.Errorf("TraceQL fixture table %q equals the default SpansTable name; not a genuine override", traceqlFixtureTracesTable)
	}
}

// TestResourceFixture_DottedKeyNormalization pins the PromQL/LogQL half of
// SIGNAL-RESOURCE-001's dotted-key claim directly against the production
// normalizer both heads share (internal/api/format.OTelToPromLabel):
// NamespaceKey sanitizes to NamespaceWireLabel, and reverses back via the
// dotted-fallback candidate chain (format.PromLabelToOTelCandidates) both
// heads consult when resolving a matcher against a dotted map key. TeamKey,
// having no character outside the label-name grammar, is its own
// normalized form — the "no normalization needed" baseline the fixture's
// simple shared key exists to cover.
func TestResourceFixture_DottedKeyNormalization(t *testing.T) {
	if got := format.OTelToPromLabel(NamespaceKey); got != NamespaceWireLabel {
		t.Errorf("OTelToPromLabel(%q) = %q, want %q", NamespaceKey, got, NamespaceWireLabel)
	}
	if got := format.OTelToPromLabel(TeamKey); got != TeamKey {
		t.Errorf("OTelToPromLabel(%q) = %q, want %q unchanged (no dots to sanitize)", TeamKey, got, TeamKey)
	}

	candidates := format.PromLabelToOTelCandidates(NamespaceWireLabel)
	found := false
	for _, c := range candidates {
		if c == NamespaceKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("PromLabelToOTelCandidates(%q) = %v, want it to include the dotted OTel key %q", NamespaceWireLabel, candidates, NamespaceKey)
	}
}

// TestResourceFixture_ExplicitPromResourceLabelsAllowlist pins acceptance
// criterion "PromQL dotted-label normalization/selection ... are explicit":
// the chdb test's PromResourceLabels allow-list must name both fixture keys
// explicitly rather than relying on the nil "promote every key" default —
// see schema.Metrics.PromResourceLabels's own doc comment for why nil is
// opt-OUT narrowing, not an opt-in this contract can lean on to prove
// "selection is explicit".
func TestResourceFixture_ExplicitPromResourceLabelsAllowlist(t *testing.T) {
	if len(promqlFixtureResourceLabels) == 0 {
		t.Fatal("promqlFixtureResourceLabels is empty; SIGNAL-RESOURCE-001 requires an explicit, non-nil allow-list")
	}
	want := map[string]bool{TeamKey: false, NamespaceKey: false}
	for _, k := range promqlFixtureResourceLabels {
		if _, ok := want[k]; !ok {
			t.Errorf("promqlFixtureResourceLabels contains unexpected key %q", k)
			continue
		}
		want[k] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("promqlFixtureResourceLabels is missing fixture key %q", k)
		}
	}
}
