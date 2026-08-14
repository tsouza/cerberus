# Spec: Crawl frontier partitioning

Tracks issue #2005.

## Problem

`crawl/crawl.spec.ts` contains one BFS test which visits and interaction-sweeps every Grafana
surface. Both `.github/scripts/compose-smoke-matrix.mjs` and
`.github/scripts/dashboard-matrix.mjs` put that test in one `shard-crawl` entry, so it remains the
full-depth wall-clock long pole.

## Scope

Included: deterministic `CRAWL_SHARD_INDEX` / `CRAWL_SHARD_COUNT` validation and ownership; a
per-shard visited-slice artifact; a merge-and-ratchet command; and compose and k3d matrix wiring.

Excluded: changing the crawler's canonicalization, page or interaction oracles, inventory format,
or the crawl's release-gate posture.

## Design

The committed inventory is the common seed frontier. Each shard owns a canonical surface when a
stable UTF-16 hash of its route path (the part before `?` or `#`) modulo the shard count equals its
zero-based index. URL-encoded and in-place interaction states therefore remain with the page that
can drive them, so the owning shard also audits every interaction result it discovers.

Every shard still performs discovery from its owned pages. It emits only the states it audited plus
the canonical links it discovered. The final merge unions audited slices and discovered canonicals,
then runs the existing inventory comparison against the union. A discovered surface absent from the
inventory consequently fails as growth even when its deterministic owner was not able to receive it
within the isolated CI job. This is necessary because GitHub matrix jobs cannot exchange a live BFS
frontier; the next deliberate inventory update makes the new surface a seed and assigns it normally.

Unsharded execution remains the one-shard contract (`index=0`, `count=1`) and preserves the
existing direct inventory regeneration path. Sharded runs never write the inventory directly; the
merge command is the sole writer and requires full depth.

The two matrix manifests create the same number of crawl entries, pass their index/count to the
Playwright process, upload uniquely named slice artifacts, and run one merge job after all crawl
entries complete. The merge job downloads every required slice, verifies matching stack/depth/count
metadata and a total, disjoint ownership cover, then applies `diffInventory` to their union.

## Verification

- Playwright unit pins in `crawl.spec.ts` cover valid and invalid shard contracts, deterministic
  assignment, base-surface co-location, and the merge ratchet's growth and shrink detection.
- Node tests for each matrix pin the emitted crawl shard count, contiguous indexes, and index/count
  environment wiring.
- The merge command is covered with fixture slice files through `node --test`.
- `actionlint` and the Node test files are executable locally. A real compose and k3d scheduled or
  dispatch run is CI-only evidence for artifact transfer and live crawl timing.

## Risks

- A newly discovered surface assigned to another shard is reported as coverage growth in the current
  run but is not audited until it is present in the common inventory on a later run. The merge fails
  loudly, so this cannot become a silent coverage gap.
- More crawl shards multiply stack startup and browser resource use. The shard count is kept as one
  named constant per substrate and is pinned by matrix tests.

## Task List

1. Add and test the deterministic crawl shard contract and slice serialization. Verification: focused
   Playwright crawl unit tests.
2. Add and test the slice merge and inventory-ratchet command. Verification: focused Node tests.
3. Fan out compose and k3d crawl matrices, pass the contract, publish slices, and merge them.
   Verification: matrix tests and `actionlint`.
