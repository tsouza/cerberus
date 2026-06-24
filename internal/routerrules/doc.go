// Package routerrules is an offline analysis engine that mines the
// cerberus_router_corpus table (or its per-pod JSONL fallback) and emits
// findings: shape classes where the recorded route A/B decision is paying an
// observable cost the corpus shows the other route would avoid. It changes no
// routing — it is a report generator the operator runs (via cmd/route-rules).
//
// # The hard invariant
//
// The shipped catalog file (catalog/router_rules.yaml) contains ONLY generic
// drivers — the rule STRUCTURE, detection LOGIC, and NAMED parameter references
// — and ZERO deployment-specific numbers. Every threshold, watermark, cap, or
// percentile cutoff is a named parameter resolved at runtime, per-deployment,
// from that deployment's own corpus aggregates or its config. The number lives
// in the deployment's data or config; it is never a literal in the YAML, and
// never a literal in Go.
//
// The invariant is enforced three independent ways:
//
//  1. Structural: the condition AST (see condition.go) has no number-literal
//     node. A bare number in a comparison operand position is not merely
//     rejected at validation time — it is unrepresentable in the parsed model.
//  2. Schema validation at load (see validate.go): dangling parameter refs,
//     unknown resolver kinds, unknown columns, duplicate rule ids, and cyclic
//     parameter dependencies are all hard load errors.
//  3. A CI grep test (catalog/router_rules_test.go) asserts no digit-bearing
//     scalar appears in any parameter or condition value position of the
//     embedded catalog. This is the reviewer-facing proof.
//
// This mirrors the self-tuning generic-loop / local-constants split: the
// shipped catalog generalizes across all deployments; the numbers come from
// each deployment's own data.
package routerrules
