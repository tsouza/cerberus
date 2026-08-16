# PromQL conflict-resistant test shards

## Problem and evidence

Issue #2235 records repeated conflicts between the independent PromQL work in pull requests 2228,
2231, and 2232. All three appended to `compatibility/prometheus/cerberus-test-queries.yml`; the file is now
922 lines and mixes scalar, selector, operator, function, subquery, native-histogram, and
duplicate-labelset cases under one `test_cases` sequence. Independent histogram work also edits two
large shared Go test files: `internal/promql/exp_histogram_reject_test.go` (1,036 lines) and
`internal/api/prom/handler_chdb_histogram_valued_test.go` (631 lines). Those conflicts serialize
otherwise independent PRs and force the final branch to restart canonical CI.

## Scope

This first #2235 slice stores the Prometheus compatibility corpus as ordered per-family fragments,
materializes the exact single YAML document expected by the upstream compatibility tester, and
routes the hollow-green regression test through the same loader. It also moves the existing
PromQL lowering and Prom HTTP chDB histogram tests into function-family files when the move is
mechanical.

The change does not alter a query, variant, expected-failure verdict, seed, lowering behavior, HTTP
behavior, or parity baseline. Lowering dispatch and `compatibility/parity-baseline.json` remain
separate #2235 implementation slices because either would broaden this PR across production code or
generated compatibility state.

## Design

`compatibility/prometheus/query-corpus/header.yml` owns the document header and `test_cases:` key.
`query-corpus/fragments/NNN-<family>.yml` owns the indented list entries for one query family. The
three-digit prefixes are contiguous from zero and are the canonical order. A shared Go loader under
`compatibility/prometheus/querycorpus` discovers the fragments, rejects directories, unexpected
names, gaps, empty fragments, malformed YAML, empty corpora, and duplicate complete test cases,
then returns the concatenated single-document bytes and decoded cases.

The compatibility shell harness invokes a small `cmd/assemble-queries` adapter when
`TESTER_QUERIES` is unset and gives the resulting temporary YAML file to the upstream tester. An
explicit `TESTER_QUERIES` path retains its existing override semantics. The regression corpus test
imports the loader directly, so its metric-name analysis and the live compatibility run consume the
same assembled bytes.

The initial split is byte-preserving: concatenating the header and fragments in loader order
recreates the current monolith exactly. Test-file splits move functions and their private helpers
without changing their bodies.

## Verification

- Migration verification compares the assembled SHA-256 to the pre-split corpus bytes. Focused
  `querycorpus` unit tests carry negative controls for an omitted numeric shard, duplicate shard
  content, malformed order/name, empty content, unexpected directory, and hollow corpus.
- `TestCompatCorpusReferencesOnlySeededMetrics` continues to parse the assembled corpus and proves
  every referenced metric is seeded on both backends.
- Focused existing PromQL lowering tests cover the mechanically moved test functions.
- Focused existing chDB-tagged Prom handler tests cover the mechanically moved HTTP tests when the
  local chDB substrate is available; the final pushed SHA runs the canonical CI lanes.

## Risks

The upstream tester accepts one YAML document rather than fragment syntax, so partial input must
never reach it. Materialization therefore happens before the tester invocation and any loader error
terminates the harness. Filesystem enumeration can vary by platform, so the loader sorts by
filename and separately proves numeric contiguity. Duplicate detection keys the full decoded case,
not only the query text, preserving the possibility of the same query with intentionally different
metadata while rejecting a copied case that would inflate the score.

## Numbered implementation tasks

1. Mechanically split the current YAML bytes into the header and contiguous per-family fragments.
2. Add the shared fail-closed loader, command adapter, and negative-control unit tests.
3. Route the compatibility harness and seed-corpus regression through the shared loader.
4. Mechanically split the PromQL rejection and Prom handler chDB histogram tests by function family.
5. Run focused checks, publish a ready PR, and drive required CI through protected squash merge.
6. Continue issue #2235 in separate lowering-dispatch and parity-baseline shard PRs.
