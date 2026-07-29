package routerrules

import (
	"fmt"
	"strings"
	"testing"
)

// validCatalogYAML is a minimal well-formed catalog the negative tests mutate.
const validCatalogYAML = `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params:
  - name: pctile
    kind: config
    key: router_rules.watermark_percentile
  - name: mem_wm
    kind: corpus_percentile
    column: memory_usage
    percentile: { ref: pctile }
    partition_by: [language]
    scope: { route: A, exit_status: ok }
rules:
  - id: r1
    severity: high
    since: 1
    status: active
    group_by: [shape_id, language]
    condition:
      all:
        - { col: route, op: eq, enum: A }
        - { col: memory_usage, op: gte, param: mem_wm }
    finding: "x {mem_wm}"
`

func TestValidateAcceptsWellFormed(t *testing.T) {
	if _, err := LoadCatalog([]byte(validCatalogYAML)); err != nil {
		t.Fatalf("expected valid catalog to load, got: %v", err)
	}
}

// TestValidateRejects is the table of negatives that MUST fail to load. Each
// case names the invariant or structural rule it exercises.
func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "number in comparison operand is unrepresentable",
			yaml: `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params: []
rules:
  - id: r
    severity: low
    since: 1
    status: active
    group_by: [shape_id]
    condition: { col: memory_usage, op: gte, enum: 5000 }
    finding: "x"
`,
			want: "enum",
		},
		{
			name: "dangling param ref",
			yaml: `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params: []
rules:
  - id: r
    severity: low
    since: 1
    status: active
    group_by: [shape_id]
    condition: { col: memory_usage, op: gte, param: nope }
    finding: "x"
`,
			want: "undeclared param",
		},
		{
			name: "unknown param kind",
			yaml: `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params:
  - name: p
    kind: voodoo
    key: x
rules: []
`,
			want: "unknown kind",
		},
		{
			name: "unknown column in condition",
			yaml: `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params:
  - name: p
    kind: config
    key: k
rules:
  - id: r
    severity: low
    since: 1
    status: active
    group_by: [shape_id]
    condition: { col: not_a_column, op: gte, param: p }
    finding: "x"
`,
			want: "unknown column",
		},
		{
			name: "duplicate rule id",
			yaml: `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params: []
rules:
  - id: dup
    severity: low
    since: 1
    status: active
    group_by: [shape_id]
    condition: { col: route, op: eq, enum: A }
    finding: "x"
  - id: dup
    severity: low
    since: 1
    status: active
    group_by: [shape_id]
    condition: { col: route, op: eq, enum: B }
    finding: "y"
`,
			want: "duplicate rule id",
		},
		{
			name: "param dependency cycle",
			yaml: `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params:
  - name: a
    kind: corpus_percentile
    column: memory_usage
    percentile: { ref: b }
  - name: b
    kind: corpus_percentile
    column: memory_usage
    percentile: { ref: a }
rules: []
`,
			want: "cycle",
		},
		{
			name: "numeric in enum slot",
			yaml: `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params: []
rules:
  - id: r
    severity: low
    since: 1
    status: active
    group_by: [shape_id]
    condition: { col: route, op: eq, enum: 1 }
    finding: "x"
`,
			want: "enum",
		},
		{
			name: "enum value outside domain",
			yaml: `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params: []
rules:
  - id: r
    severity: low
    since: 1
    status: active
    group_by: [shape_id]
    condition: { col: route, op: eq, enum: C }
    finding: "x"
`,
			want: "category",
		},
		{
			name: "non-enum column compared to category",
			yaml: `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params: []
rules:
  - id: r
    severity: low
    since: 1
    status: active
    group_by: [shape_id]
    condition: { col: memory_usage, op: eq, enum: ok }
    finding: "x"
`,
			want: "non-enum",
		},
		{
			name: "non-numeric column compared to param",
			yaml: `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params:
  - name: p
    kind: config
    key: k
rules:
  - id: r
    severity: low
    since: 1
    status: active
    group_by: [shape_id]
    condition: { col: route, op: gte, param: p }
    finding: "x"
`,
			want: "non-numeric",
		},
		{
			name: "scope on non-enum column",
			yaml: `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params:
  - name: p
    kind: corpus_percentile
    column: memory_usage
    percentile: { ref: q }
    scope: { memory_usage: ok }
  - name: q
    kind: config
    key: k
rules: []
`,
			want: "non-enum",
		},
		{
			name: "wrong apiVersion",
			yaml: `
apiVersion: routerrules.cerberus/v99
catalogVersion: 1
params: []
rules: []
`,
			want: "apiVersion",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadCatalog([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected load to fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestValidateRejectsPartitionNotInGroupBy pins the catalogVersion-2 load-time
// hardening (critique C5): a rule that references a partitioned corpus param
// whose partition column is NOT in the rule's group_by must fail at LOAD, not
// silently at report time. Here mem_wm partitions by language but the rule's
// group_by omits language.
func TestValidateRejectsPartitionNotInGroupBy(t *testing.T) {
	const y = `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params:
  - name: pctile
    kind: config
    key: router_rules.watermark_percentile
  - name: mem_wm
    kind: corpus_percentile
    column: memory_usage
    percentile: { ref: pctile }
    partition_by: [language]
    scope: { exit_status: ok }
rules:
  - id: r1
    severity: high
    since: 1
    status: active
    group_by: [shape_id]
    condition:
      all:
        - { col: memory_usage, op: gte, param: mem_wm }
    finding: "x {mem_wm}"
`
	_, err := LoadCatalog([]byte(y))
	if err == nil {
		t.Fatal("expected load to fail: partition column language not in group_by")
	}
	if !strings.Contains(err.Error(), "partition column") || !strings.Contains(err.Error(), "group_by") {
		t.Fatalf("error should name the partition/group_by mismatch, got: %v", err)
	}
}

// healthScopeCatalogYAML is a one-param catalog whose percentile column and
// exit_status scope the health-scope tests substitute, so each case differs from
// the accepted shape in exactly the axis under test.
const healthScopeCatalogYAML = `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params:
  - name: pctile
    kind: config
    key: router_rules.watermark_percentile
  - name: wm
    kind: corpus_percentile
    column: %s
    percentile: { ref: pctile }
%s
rules:
  - id: r1
    severity: high
    since: 1
    status: active
    group_by: [shape_id]
    condition:
      all:
        - { col: %s, op: gte, param: wm }
    finding: "x {wm}"
`

// TestValidateHealthScope pins the scoping a percentile's column demands, in
// both directions.
//
// A runtime-cost percentile (memory_usage, read_rows, read_bytes,
// query_duration_ms) is a "what does a normal query cost here?" watermark, so it
// must be learned over clean finishes: a timed-out row's duration is the
// deadline and an OOM row's memory is the cap, so those rows describe the
// failure rather than the query, and folding them in drags the watermark toward
// the pathologies the rules exist to detect.
//
// A geometry percentile (fanout, cumulative_d, n_anchors, k_shards) is derived
// from the query shape before dispatch, so it carries no outcome bias to remove;
// scoping it to clean finishes only shrinks the population, and on a deployment
// whose heavy shapes are the ones that fail it empties the bucket and collapses
// the watermark to a fire-on-everything floor.
//
// Both mistakes leave a param that still resolves to a number, just the wrong
// one, so neither surfaces as anything but a rule that quietly under- or
// over-fires. Load-time is the only place they are visible.
func TestValidateHealthScope(t *testing.T) {
	const (
		healthy   = "    scope: { exit_status: ok }"
		unhealthy = "    scope: { exit_status: oom }"
		unscoped  = ""
	)
	cases := []struct {
		name   string
		column string
		scope  string
		want   string // substring the load error must name; empty = must load
	}{
		{name: "cost column scoped to ok loads", column: "query_duration_ms", scope: healthy},
		{name: "geometry column unscoped loads", column: "cumulative_d", scope: unscoped},
		{
			name: "cost column unscoped is rejected", column: "read_rows", scope: unscoped,
			want: "must declare scope.exit_status: ok",
		},
		{
			name: "cost column scoped to a failure class is rejected", column: "memory_usage", scope: unhealthy,
			want: `scopes exit_status to "oom"`,
		},
		{
			name: "geometry column scoped to ok is rejected", column: "fanout", scope: healthy,
			want: "measures geometry column",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := fmt.Sprintf(healthScopeCatalogYAML, tc.column, tc.scope, tc.column)
			_, err := LoadCatalog([]byte(y))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("expected catalog to load, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected load to fail naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error should name %q, got: %v", tc.want, err)
			}
		})
	}
}
