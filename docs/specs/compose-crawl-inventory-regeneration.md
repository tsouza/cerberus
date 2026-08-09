# Spec: CI regeneration path for the compose crawl surface inventory

Tracks issue #1826. This document is the worked example for the plan-first workflow described in
`docs/agent-workflow.md`. **Implemented** — see the task list at the bottom for what shipped, where
it diverged from the original design, and what still needs a real `workflow_dispatch` run to confirm
end to end.

## Problem

`e2e.yml` can regenerate the **k3d** crawl surface inventory from CI. It cannot regenerate the
**compose** one, even though both files gate on every PR and every push to `main`.

Evidence, all in `.github/workflows/e2e.yml`:

- The `workflow_dispatch` input `update_crawl_inventory` names k3d in its own description and is
  consumed in exactly two places, both inside the `dashboard` job's k3d crawl step: the
  `CERBERUS_UPDATE_INVENTORY` assignment, guarded by
  `if: env.RUN_SHARD == 'true' && matrix.crawlStack == 'k3d'`, and the artifact upload, guarded by
  `matrix.crawlStack == 'k3d'` and uploading `grafana-surface-inventory.k3d.json` alone.
- The compose crawl runs in the `compose-smoke-shard-info` lane's `compose-smoke-shard` step. That
  step sets `SWEEP_DEPTH`, `CRAWL_STACK: compose`, and the three service URLs. It never sets
  `CERBERUS_UPDATE_INVENTORY`, and no step anywhere in the file uploads
  `grafana-surface-inventory.compose.json`.

Two properties turn the omission from an inconvenience into a dead end:

- Regeneration requires an exhaustive crawl. `crawl.spec.ts` hard-asserts
  `expect(depth, 'inventory regeneration requires exhaustive crawl: rerun SWEEP_DEPTH=full').toBe('full')`.
  The compose lane runs `SWEEP_DEPTH=full` only on the nightly `schedule` event, which carries no
  dispatch inputs.
- `test/e2e/playwright/crawl/grafana-surface-inventory.compose.json` is marked `-merge` in
  `.gitattributes`, so hand-editing it is against repository policy — invariant 9 in `CLAUDE.md`.

The consequence: when Grafana or the stack grows a surface, the compose inventory ratchet goes red on
`main`, and the only way to produce a correct replacement file is to boot the full compose stack on a
developer machine and run a full-depth Playwright crawl locally.

## Goals

1. A maintainer can regenerate `grafana-surface-inventory.compose.json` entirely from CI, by a
   `workflow_dispatch` run, with no local compose stack.
2. The regenerated file arrives as a downloadable artifact, per stack, so the two inventories can
   never be confused for one another.
3. The dispatch surface states which stacks it regenerates, so the k3d-only reading that produced
   this gap is not available to the next reader.

## Non-goals

- Changing what the crawl visits, how the inventory is shaped, or how either ratchet compares it.
- Automatically committing a regenerated inventory. The artifact is downloaded and committed through
  a PR like any other generated file.
- Anything about issue #1825, the current red state of the k3d inventory ratchet, or issue #1674's
  per-PR content-exact ratchet. Those are separate changes against separate issues.

## Design

Three coordinated edits, all in `.github/workflows/e2e.yml` — **plus a fourth this design missed**:
`compose-smoke-scope.mjs` hard-codes `workflow_dispatch` as a `NON_BOOTING_EVENT`, unconditionally
(the compose lane's only prior dispatch behaviour was "never boots" — the exclusion predates this
issue and exists because the only dispatch input used to regenerate the k3d inventory alone). Without
a fourth edit there, `compose-smoke-setup` (and therefore `compose-smoke-shard-info`, which is where
the crawl actually runs) never fires on a dispatch at all, regardless of what the workflow-level
`update_crawl_inventory` edits below do — the three-edit plan would ship a dead path. The fix is a
narrow carve-out in `decide()`: a dispatch selection of `compose` or `both` boots the stack exactly
like push/schedule; every other dispatch selection (`none`, `k3d`) still short-circuits, preserving
the existing k3d-only-regen behaviour and its pinned test
(`compose-smoke-scope.test.mjs`'s "workflow_dispatch does not boot the stack ... whatever the diff
says").

**1. Widen the dispatch surface to name a stack.** Replace the boolean `update_crawl_inventory` with
an input that selects which inventories to regenerate — `none`, `k3d`, `compose`, or `both` — with
`none` as the default so an ordinary dispatch is unaffected. A `choice` input keeps the surface
self-describing, which is the property whose absence caused the gap. The description must name every
inventory path it can write.

**2. Force full depth when regenerating.** `SWEEP_DEPTH` for the compose lane currently derives from
the event alone. It must additionally resolve to `full` when this run is regenerating the compose
inventory, otherwise `crawl.spec.ts`'s assertion fails by construction and the dispatch is useless.
The same reasoning applies to the k3d lane, which today gets full depth only incidentally. The
resolution is a small expression, and if it does not stay a one-liner it belongs in
`.github/scripts/` per invariant 15.

**3. Set the env var and upload the artifact on the compose lane.** Mirror the two k3d sites onto
`compose-smoke-shard`: set `CERBERUS_UPDATE_INVENTORY` when the resolved selection includes compose,
and add an upload step for `grafana-surface-inventory.compose.json`. Keep the artifact names
stack-qualified.

The compose crawl is sharded. Whether a single shard produces a complete inventory, or the shards
must be merged, is the one open question in this design, and task 1 below settles it before any
workflow edit is written. If merging is required, the merge belongs in a `.github/scripts/` module
with its own unit test, not in inline YAML.

## Verification

- **Structural.** `just lint-actions` (actionlint) over the edited workflow.
- **Behavioural, and the only verification that actually proves the goal.** A `workflow_dispatch` run
  on the branch with the compose selection, whose artifact is downloaded and compared against the
  committed `grafana-surface-inventory.compose.json`. On an unchanged surface the two must be
  identical. This is the acceptance criterion: a green workflow that uploads a file nobody compared
  proves nothing.
- **Negative control.** A dispatch with the default `none` must leave both lanes behaving exactly as
  they do today — no `CERBERUS_UPDATE_INVENTORY`, no depth change, no inventory artifact.
- **Regression.** The existing k3d regeneration path must still work after the input is reshaped;
  exercise it in the same dispatch matrix rather than assuming the rename was lossless.

## Risks

- **Silent no-op.** The failure mode this class of change is most prone to is a gate or a flag that
  degrades to OFF while every check stays green. The behavioural verification above exists
  specifically to prove the path activates, not merely that it runs.
- **Input rename.** Any saved dispatch, documentation reference, or muscle memory using the boolean
  `update_crawl_inventory` stops working. Grep the tree and the docs for the name and update every
  hit in the same change.
- **Depth cost.** A full-depth compose crawl is materially slower than the lean PR default. Gating it
  behind an explicit dispatch selection keeps that cost off the PR path, which is why the default is
  `none` rather than inferring intent from the event.

## Task list for owner review

Ordered so the tree is green between any two tasks.

1. **Done.** One compose shard yields a complete inventory — no merge needed. `SHARDS` in
   `compose-smoke-matrix.mjs` has exactly one entry in `GATE_EXCLUDED_SHARDS`
   (`shard-crawl`), so it is also the only entry in `matrix_informational`; that single shard's
   `crawl/crawl.spec.ts` is the sole spec anywhere in the compose cohort that touches
   `CERBERUS_UPDATE_INVENTORY` or writes the inventory file. There is exactly one informational shard,
   so the artifact upload can use a static, non-matrix-suffixed name, mirroring the k3d upload.
2. **Done, plus the fourth edit this design missed** (see Design above): reshaped
   `workflow_dispatch.inputs.update_crawl_inventory` to a `choice` (`none` default,
   `k3d`/`compose`/`both`), rewired the two k3d consumers, and added the `compose-smoke-scope.mjs`
   carve-out so a `compose`/`both` dispatch actually boots the compose lane in the first place.
   Verified with `just lint-actions` (clean) and `node --test .github/scripts/compose-smoke-scope.test.mjs`
   (15/15, including two new tests pinning the carve-out in both directions).
3. **Done.** `SWEEP_DEPTH` on the compose crawl step (`compose-smoke-shard-info`) now resolves to
   `full` on `schedule` OR a dispatch selecting `compose`/`both`. The k3d crawl step was already
   hard-coded to `SWEEP_DEPTH: full` unconditionally, so it needed no change.
4. **Done.** `CERBERUS_UPDATE_INVENTORY` is set on the `compose-smoke-shard-info` crawl step (the
   step that actually runs `crawl/crawl.spec.ts`, not `compose-smoke-shard`, which never runs it —
   see task 1) when the dispatch selection includes compose.
5. **Done.** Added `Upload regenerated compose crawl inventory`, uploading
   `grafana-surface-inventory.compose.json` as artifact `grafana-surface-inventory-compose`. No merge
   step needed per task 1.
6. **Not done — needs a real dispatch.** This is CLAUDE.md's `compose-smoke` local-verification
   exception: it needs a real Docker Compose stack, not chDB, so it cannot be proven from an agent
   worktree. What's confirmed locally: `just lint-actions` is clean; the `compose-smoke-scope.mjs`
   unit tests pin the boot decision for all four selection values against both a real and a null/empty
   diff; a manual read of the resolved `SWEEP_DEPTH` / `CERBERUS_UPDATE_INVENTORY` expressions for
   `none`/`k3d`/`compose`/`both` confirms only `compose`/`both` flip either. What still needs the
   dispatch: that the uploaded `grafana-surface-inventory-compose` artifact is well-formed and, on an
   unchanged surface, byte-identical to the committed inventory; that a `none` dispatch is truly an
   inert no-op end to end; and that the k3d path (re-verified, not merely re-read) still regenerates
   correctly through the renamed input.
7. **Done.** Updated `docs/test-strategy.md`'s crawl-inventory section to state the regeneration path for both
   stacks.
