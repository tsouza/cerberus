package routerrules

import (
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
