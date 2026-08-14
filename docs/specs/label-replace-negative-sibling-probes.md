# Label Replace Negative Sibling Probes

## Problem

`label_replace` resolves a duplicate capture name to the first group that
participated in the match. ClickHouse's `extractGroups` cannot distinguish a
non-participating group from a participating group that captured an empty
string. The existing positive probe rewrite resolves a carrier with a
non-empty ancestor, but still rejects a nullable carrier that is alone in a
mandatory alternation branch even when every sibling branch is non-empty.

For `(?:(?P<dup>a?)|(?P<dup>b))(?P<dup>c)`, an empty first carrier participated
exactly when neither sibling branch's non-empty probe participated. Prometheus
therefore returns the first empty capture while Cerberus currently rejects the
query.

## Scope

This change accepts that case only when the alternation is entered at most once
per match, the carrier is mandatory within its branch, and every sibling branch
is non-empty. It does not attempt greedy empty-path participation or repeated
alternations, which remain covered by #1956.

## Design

`internal/qlcommon` will add synthetic probes around qualifying sibling
branches and record their required emptiness beside the existing positive probe
conditions. `internal/chplan` will carry those conditions. `internal/chsql`
will render an ordered `multiIf` selection when a segment has negative probes;
the candidate is selected only when its positive participation witness is
non-empty and every sibling witness is empty.

## Verification

Unit tests will pin rewrite shape, rejection boundaries, and exhaustive
`ExpandString` agreement in `internal/qlcommon`. PromQL lowering tests and a
TXTAR fixture will pin the head, plan, SQL, and chDB answer. Generated fixture
sections will be regenerated with `just update-golden promql`.

## Risks

Incorrect source-span selection could change regex language or choose a
repeated alternation. The rewrite keeps the original source intact except for
capturing wrappers, validates capture-index round-tripping, and declines any
shape outside the structural proof above.

## Tasks

1. Model qualified sibling probes and negative conditions in qlcommon/chplan;
   verify with focused unit tests.
2. Emit ordered conditional selection in chsql; verify SQL and chDB semantics.
3. Add PromQL lowering and fixture coverage; regenerate the PromQL golden
   shard and review generated artifacts.
