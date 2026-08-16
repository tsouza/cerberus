# Manual golden-update workflow

## Purpose

Maintainers need to regenerate selected `just update-golden` shards for an
existing pull-request branch without moving generated files through a local
checkout. The workflow must preserve the shard runner's dependency ordering
while preventing matrix workers from racing pushes or combining generated
files with Git's line merge.

## Interface

`update-golden.yml` is dispatched from the repository's default branch with two
required inputs:

- `shards`: the same space/comma-separated vocabulary accepted by
  `just update-golden`, including `all`;
- `branch`: an existing same-repository short branch name.

The default branch is never a valid target. Publication requires the existing
`RELEASE_PAT` secret so the resulting branch push triggers pull-request checks.

## Execution contract

The plan job validates the branch, expands and orders the shard set through the
canonical shard table, and confirms that the selection covers the target
branch's diff against the default branch.

Each selected shard receives a matrix row with read-only repository authority.
A row regenerates the selected shards from earlier stages as local context, then
its own shard. This preserves the local runner's pre/body/post relation: a body
shard can observe a freshly generated solver ledger, and cardinality profiles
freshly generated fixture SQL. The artifact uploaded by the row contains only
the row's own generated paths.

One publisher downloads every patch, checks that the target branch still points
to the planned commit, rejects missing, duplicate or cross-shard paths, applies
the disjoint patches, and creates one commit and one push. If the branch moved,
the run fails without publication and a new dispatch starts from its new head.
An empty aggregate diff succeeds without making an empty commit.

## Security and conflict boundaries

- Target-branch generation never receives write credentials.
- Only controller code checked out from the workflow ref handles publication.
- Matrix jobs cannot push, so they cannot race non-fast-forward updates.
- The publisher commits once, so a dispatch cannot leave a partially updated
  branch.
- Every artifact is checked against the canonical generated paths declared by
  its shard before application.
- The existing `-merge` attributes remain the repository-level backstop for
  generated artifacts changed by independent pull requests.

## Acceptance checks

- Controller tests pin input normalization, stage context, protected-branch
  rejection, path ownership and a real two-patch/one-push git round trip.
- The workflow contract test pins read-only matrix checkout, artifact handoff,
  `RELEASE_PAT` publication and the single publisher.
- `actionlint` accepts the workflow.
