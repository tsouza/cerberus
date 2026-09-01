package ddl

import (
	"strings"
	"testing"
)

// TestRenderTraceMaterializedAttrColumns_ExactSQL pins the ADD COLUMN
// statements cerberus issue #2776 specifies verbatim: the idempotent IF
// NOT EXISTS guard, the LowCardinality(String) type, the DEFAULT map
// subscript reading the right carrier column (SpanAttributes for the span
// registry, ResourceAttributes for the resource registry), deterministic
// (sorted) ordering across multiple keys, and the optional ON CLUSTER
// clause — plus, for http.status_code, the numeric shape cerberus issue
// #2869 gives it instead: Nullable(Int32) DEFAULT
// toInt32OrNull(SpanAttributes['http.status_code']).
func TestRenderTraceMaterializedAttrColumns_ExactSQL(t *testing.T) {
	cfg := Config{
		MaterializedSpanAttributeColumns: map[string]string{
			"rpc.method":       "__cerberus_materialized_rpc.method",
			"http.status_code": "__cerberus_materialized_http.status_code",
		},
		MaterializedResourceAttributeColumns: map[string]string{
			"k8s.namespace.name": "__cerberus_materialized_k8s.namespace.name",
		},
	}.withDefaults()

	want := []string{
		"ALTER TABLE default.otel_traces ADD COLUMN IF NOT EXISTS `__cerberus_materialized_http.status_code` " +
			"Nullable(Int32) DEFAULT toInt32OrNull(`SpanAttributes`['http.status_code'])",
		"ALTER TABLE default.otel_traces ADD COLUMN IF NOT EXISTS `__cerberus_materialized_rpc.method` " +
			"LowCardinality(String) DEFAULT `SpanAttributes`['rpc.method']",
		"ALTER TABLE default.otel_traces ADD COLUMN IF NOT EXISTS `__cerberus_materialized_k8s.namespace.name` " +
			"LowCardinality(String) DEFAULT `ResourceAttributes`['k8s.namespace.name']",
	}
	got := renderTraceMaterializedAttrColumns(cfg)
	if len(got) != len(want) {
		t.Fatalf("renderTraceMaterializedAttrColumns() = %d statements, want %d:\n%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement %d =\n  %s\nwant:\n  %s", i, got[i], want[i])
		}
	}
}

// TestRenderTraceMaterializedAttrColumns_OnCluster confirms the ON CLUSTER
// clause is threaded onto every rendered ADD COLUMN, mirroring every other
// ALTER this package renders.
func TestRenderTraceMaterializedAttrColumns_OnCluster(t *testing.T) {
	cfg := Config{
		Cluster:                          "prod",
		MaterializedSpanAttributeColumns: map[string]string{"http.status_code": "__cerberus_materialized_http.status_code"},
	}.withDefaults()
	got := renderTraceMaterializedAttrColumns(cfg)
	if len(got) != 1 {
		t.Fatalf("got %d statements, want 1: %v", len(got), got)
	}
	if !strings.Contains(got[0], "ON CLUSTER `prod`") {
		t.Errorf("statement missing ON CLUSTER clause: %s", got[0])
	}
}

// TestRenderSignal_TraceMaterializedAttributesEnabled pins the enable-gate
// contract at the render layer: TraceMaterializedAttributesEnabled=false
// renders NO materialized-attribute ADD COLUMN statements even when the
// key/column registries are populated (the two-part gate —
// SchemaProvisioning.TraceMaterializedAttrsEnabled's doc explains why both
// halves must agree) — and true renders exactly one per configured key.
// Metrics and Logs are untouched regardless: the feature is traces-only.
func TestRenderSignal_TraceMaterializedAttributesEnabled(t *testing.T) {
	for _, tt := range []struct {
		name    string
		enabled bool
		want    int
	}{
		{"disabled", false, 0},
		{"enabled", true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				TraceMaterializedAttributesEnabled: tt.enabled,
				MaterializedSpanAttributeColumns:   map[string]string{"http.status_code": "__cerberus_materialized_http.status_code"},
			}.withDefaults()
			for _, sig := range []Signal{Metrics, Logs, Traces} {
				stmts, err := renderSignal(cfg, sig)
				if err != nil {
					t.Fatalf("renderSignal(%s): %v", sig, err)
				}
				got := 0
				for _, stmt := range stmts {
					if strings.Contains(stmt, "__cerberus_materialized_http.status_code") {
						got++
					}
				}
				want := 0
				if sig == Traces {
					want = tt.want
				}
				if got != want {
					t.Errorf("%s: %d materialized-attribute statements, want %d, rendered:\n%v", sig, got, want, stmts)
				}
			}
		})
	}
}

// TestRenderTraceMaterializedAttrColumns_EmptyRegistriesNoop confirms that
// TraceMaterializedAttributesEnabled=true with nil/empty registries
// renders nothing — the enable bit alone provisions nothing without at
// least one configured key, matching TraceMaterializedAttributesEnabled's
// own doc comment.
func TestRenderTraceMaterializedAttrColumns_EmptyRegistriesNoop(t *testing.T) {
	cfg := Config{TraceMaterializedAttributesEnabled: true}.withDefaults()
	got := renderTraceMaterializedAttrColumns(cfg)
	if len(got) != 0 {
		t.Errorf("got %d statements with empty registries, want 0: %v", len(got), got)
	}
}
