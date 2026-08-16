# Test-fence enrollment guards

## Problem and evidence

The shadow lane registry added by #2233 describes CI ownership, but it does not yet prove that
test-bearing inputs remain enrolled. Four silent gaps are possible:

- a root-module `_test.go` file can add a positive build tag that no registered lane claims;
- `mutation-matrix.mjs` can gain or lose a package scope without the mutation lane's impact paths
  changing with it;
- one query head's from-scratch property entry point can disappear while the shared property job
  still runs and reports green for the remaining heads; and
- one query head can lose its live reference-stack parity lane while the other two remain green.

The current registry demonstrates the first drift concretely: the chaos job builds with
`chaos_sleep`, and `startup-bench` runs `startup_bench`-tagged tests, but neither tag is declared by
its lane in `.github/ci-lanes.json`.

## Scope

This change adds structural, merge-time enrollment guards for the existing shadow registry. It
also corrects the two observed tag declarations. It does not activate impact selection, change
branch protection, move property or compatibility execution between events, or alter any test's
runtime semantics. Vendored upstream modules excluded by `go.mod` `ignore` directives are outside
the root-module tagged-test inventory.

## Design

`test/regression/ci_lane_enrollment_test.go` consumes the same registry parser used by
`TestCILaneRegistry` and asserts four closed contracts:

1. Every root-module positively build-tagged `_test.go` file matches a registered lane whose
   `build_tags` contains the tag and whose impact globs cover the file.
2. Every package scope emitted by `mutation-matrix.mjs` is represented by the mutation lane's
   `package_globs`, and every internal package glob claimed by that lane maps back to a real matrix
   scope.
3. The property tree contains a real runner shape for PromQL, LogQL, and TraceQL, and the property
   lane declares each shape's tags, source domain, shared property tree, and seeded command.
4. PromQL, LogQL, and TraceQL each retain an always-on, layer-6d, live reference-stack lane.

The registry's chaos and startup-benchmark lanes gain the missing build-tag and test-tree claims.
No new tolerance list or expected-failure set is introduced.

## Verification

- `go test -race -run '^TestCILane(TaggedTest|MutationPackage|PropertyShape|LiveParity)Enrollment$' ./test/regression/`
- `node --test .github/scripts/ci-lane-contract.test.mjs`
- `MODE=registry node .github/scripts/ci-lane-contract.mjs`
- normal required CI on the final pushed SHA

The focused Go tests carry negative helper cases for missing tag, missing path, missing property
head, and missing live-parity head so an accidentally vacuous guard cannot validate itself.

## Risks

Path matching must stay aligned with the registry's intentionally small glob vocabulary. The guard
therefore supports exact paths and recursive `/**` prefixes and rejects unsupported glob shapes in
the inventory it evaluates. Property-shape detection is AST-based rather than comment- or filename-
based, so renames remain safe while removal of the actual runner call stays visible.

## Numbered implementation tasks

1. Extend the regression registry projection with the enrollment fields and focused helper
   negative controls.
2. Add tagged-test, mutation-package, property-shape, and live-parity enrollment assertions.
3. Correct the `chaos_sleep` and `startup_bench` registry declarations exposed by the guard.
4. Run the focused regression and registry-contract checks, then ship as one protected PR.
