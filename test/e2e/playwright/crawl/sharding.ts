/**
 * Crawl BFS-frontier sharding — the deterministic partition that lets
 * the ~50min single-test() crawl run concurrently across N isolated
 * k3d clusters.
 *
 * The problem it solves
 * ---------------------
 * crawl.spec.ts is one async BFS `test()`: Playwright's native
 * `--shard` (which splits at test() granularity) can't divide it, and
 * spec-file sharding can't either (one file, one test). The only path
 * below the full-depth wall-clock floor is to split the BFS frontier
 * itself.
 *
 * The cost asymmetry the partition exploits
 * -----------------------------------------
 * DISCOVERY — navigating to a surface and harvesting its nav links to
 * grow the frontier — is comparatively cheap (one navigation + a DOM
 * `a[href]` read per surface). The DOMINANT cost is the per-surface
 * INTERACTION SWEEP (sweepInteractions in crawl.spec.ts drives a FRESH
 * navigation per planned gesture — many navigations per eligible
 * surface at full depth) plus the per-surface oracle battery. The
 * renderer-recycle budget (CONTEXT_RECYCLE_NAVIGATIONS) is spent
 * overwhelmingly on those gesture navigations.
 *
 * So the partition boundary is: EVERY shard runs the WHOLE (cheap) BFS
 * discovery walk — navigate every surface, harvest, expand the
 * frontier — so the visited set converges identically on every shard
 * and discovery stays correct. But shard `i` only runs the HEAVY work
 * (base-surface oracle battery + the interaction sweep + the in-place
 * interaction-state audits) on surfaces it OWNS:
 *
 *     owns(surface) ⇔ fnv1a(surface) % CRAWL_SHARD_COUNT == CRAWL_SHARD_INDEX
 *
 * That splits the dominant cost ~evenly while every shard keeps a
 * correct, complete frontier.
 *
 * Ownership is keyed by the BASE canonical surface, never the in-place
 * state key (`<canonical>#<control>=<value>`): a surface and ALL its
 * interaction states must land on the SAME shard so the sweep runs
 * atomically (the sweep is driven from the base surface). ownerOf()
 * strips any `#…` state suffix before hashing for exactly this reason.
 *
 * The coverage guarantee
 * ----------------------
 * The inventory ratchet (lib.ts diffInventory) asserts the FULL audited
 * set. Per-shard we must NOT weaken it. Each shard emits its OWNED
 * slice of audited states as an artifact; a final merge step unions
 * every shard's slice and asserts the union EXACTLY equals the pinned
 * inventory — no surface missed, none double-owned. mergeShardSlices()
 * is that exact, disjoint union; it fails loudly on a gap or an overlap.
 *
 * Default (unset env) = single shard = today's behavior: COUNT=1,
 * INDEX=0 owns everything, so compose-smoke's CRAWL_STACK=compose crawl
 * and any non-sharded run are byte-for-byte unchanged.
 */

import { join } from 'node:path';

import type { InventorySurface, SurfaceInventory } from './lib.js';

// Env var names — the contract the workflow matrix sets per shard.
export const CRAWL_SHARD_INDEX_ENV = 'CRAWL_SHARD_INDEX';
export const CRAWL_SHARD_COUNT_ENV = 'CRAWL_SHARD_COUNT';

// The single-shard default: one shard owning everything, identical to
// the pre-sharding behavior. Unset env resolves to exactly this.
export const SINGLE_SHARD_COUNT = 1;
export const SINGLE_SHARD_INDEX = 0;

// FNV-1a 32-bit constants — a tiny, dependency-free, deterministic
// string hash. The value only has to be stable run-to-run and
// well-distributed across the modest shard counts the lane uses; FNV-1a
// is the canonical pick for that (the same family Go's hash/fnv ships).
const FNV_OFFSET_BASIS_32 = 0x811c9dc5;
const FNV_PRIME_32 = 0x01000193;

/**
 * Stable 32-bit FNV-1a hash of a string, as an unsigned integer.
 * Deterministic across runs and platforms — the assignment must be
 * reproducible so every shard agrees on who owns what without any
 * cross-shard coordination.
 */
export function fnv1a32(s: string): number {
  let h = FNV_OFFSET_BASIS_32;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    // `Math.imul` keeps the multiply in 32-bit space; `>>> 0` coerces
    // back to unsigned after each step.
    h = Math.imul(h, FNV_PRIME_32) >>> 0;
  }
  return h >>> 0;
}

/**
 * The PATH-only base an audited-state key hashes to — the unit of
 * ownership. Two kinds of derived key must follow their parent surface
 * onto ONE shard, because the parent's interaction sweep is the ONLY
 * place they are discovered:
 *
 *   - in-place interaction states `<canonical>#<control>=<value>` —
 *     audited in place during the parent's sweep.
 *   - structural-param surfaces `<path>?<structural-params>` (e.g.
 *     `/a/…/explore?var-groupBy=kind`) — URL-encoding deviations the
 *     parent's sweep enqueues as first-class surfaces; no other shard's
 *     link-harvest reliably reaches them (they are click-driven).
 *
 * Hashing by the bare PATH (strip the first `#…` AND the `?…` query)
 * groups a surface, all its structural-param children, and all their
 * in-place states onto the shard that owns the path — the same shard
 * that sweeps the surface and DISCOVERS the children. Discovery and
 * ownership coincide, so the frontier never needs a child that a
 * different shard alone could find. It also makes a surface's whole
 * sweep an atomic unit of work, which is exactly the cost we balance.
 */
export function baseSurfaceOf(stateKey: string): string {
  const noState = stateKey.split('#', 1)[0] ?? stateKey;
  return noState.split('?', 1)[0] ?? noState;
}

/** A resolved, validated shard assignment. */
export type ShardAssignment = {
  index: number;
  count: number;
};

/**
 * Resolve the shard assignment from env (or explicit values), with the
 * unset case defaulting to the single-shard identity. Validates loudly:
 * a malformed CRAWL_SHARD_INDEX/COUNT must fail the run, never silently
 * degrade to mis-partitioned coverage.
 *
 * Rules:
 *   - both unset → { index: 0, count: 1 } (today's behavior).
 *   - count must be an integer ≥ 1.
 *   - index must be an integer in [0, count).
 *   - setting one without the other is a configuration error.
 */
export function resolveShardAssignment(env: {
  index?: string;
  count?: string;
}): ShardAssignment {
  const rawIndex = env.index ?? '';
  const rawCount = env.count ?? '';
  if (rawIndex === '' && rawCount === '') {
    return { index: SINGLE_SHARD_INDEX, count: SINGLE_SHARD_COUNT };
  }
  if (rawIndex === '' || rawCount === '') {
    throw new Error(
      `crawl sharding: ${CRAWL_SHARD_INDEX_ENV} and ${CRAWL_SHARD_COUNT_ENV} must be set together ` +
        `(got ${CRAWL_SHARD_INDEX_ENV}=${JSON.stringify(rawIndex)}, ${CRAWL_SHARD_COUNT_ENV}=${JSON.stringify(rawCount)}) — ` +
        `set both for a sharded crawl, or neither for the single-shard default`,
    );
  }
  const count = Number(rawCount);
  const index = Number(rawIndex);
  if (!Number.isInteger(count) || count < 1) {
    throw new Error(
      `crawl sharding: ${CRAWL_SHARD_COUNT_ENV}=${JSON.stringify(rawCount)} must be an integer ≥ 1`,
    );
  }
  if (!Number.isInteger(index) || index < 0 || index >= count) {
    throw new Error(
      `crawl sharding: ${CRAWL_SHARD_INDEX_ENV}=${JSON.stringify(rawIndex)} must be an integer in [0, ${count})`,
    );
  }
  return { index, count };
}

/** Resolve directly from process.env (the spec's entry point). */
export function shardAssignmentFromEnv(
  procEnv: NodeJS.ProcessEnv,
): ShardAssignment {
  return resolveShardAssignment({
    index: procEnv[CRAWL_SHARD_INDEX_ENV],
    count: procEnv[CRAWL_SHARD_COUNT_ENV],
  });
}

/**
 * Does THIS shard own (run the heavy audit + interaction sweep on) the
 * given surface/state key? Ownership is by the base canonical surface
 * so a surface and all its in-place states share one shard. With
 * count==1 every key is owned (the single-shard identity).
 */
export function ownsSurface(
  stateKey: string,
  assignment: ShardAssignment,
): boolean {
  if (assignment.count === SINGLE_SHARD_COUNT) return true;
  return fnv1a32(baseSurfaceOf(stateKey)) % assignment.count === assignment.index;
}

/**
 * Marshalled per-shard slice artifact. Carries the shard identity (so
 * the merge can verify it saw every shard exactly once) and the OWNED
 * audited surfaces this shard pinned. `surfaces` is the same
 * InventorySurface shape the inventory uses, so the merge can assemble a
 * full inventory directly.
 */
export type ShardSlice = {
  stack: string;
  shardIndex: number;
  shardCount: number;
  surfaces: InventorySurface[];
};

/**
 * Filesystem name of a shard's emitted slice, for stack `stack` and
 * shard `index`. Lives next to the inventory (crawl/) so the bootstrap
 * dispatch can upload it as an artifact and the merge step can collect
 * every shard's slice by glob. Deterministic + filename-safe.
 */
export function shardSliceFilename(stack: string, index: number): string {
  return `grafana-surface-slice.${stack}.shard-${index}.json`;
}

/** Absolute path of a shard's slice within the crawl/ directory. */
export function shardSlicePath(
  crawlDir: string,
  stack: string,
  index: number,
): string {
  return join(crawlDir, shardSliceFilename(stack, index));
}

/**
 * Render a shard's owned slice to its canonical serialized form (sorted
 * by url, two-space indent, trailing newline) — same convention as
 * marshalInventory so slices diff cleanly.
 */
export function marshalShardSlice(slice: ShardSlice): string {
  const surfaces = [...slice.surfaces].sort((a, b) =>
    a.url.localeCompare(b.url),
  );
  return `${JSON.stringify(
    {
      stack: slice.stack,
      shardIndex: slice.shardIndex,
      shardCount: slice.shardCount,
      surfaces,
    },
    null,
    2,
  )}\n`;
}

/**
 * Exact, disjoint union-merge of every shard's owned slice into the
 * full surface inventory.
 *
 * The coverage guarantee lives here: the merge FAILS LOUDLY when the
 * slices are not a clean partition. It asserts that
 *   - exactly `count` slices arrived, one per shard index in [0, count),
 *     none missing, none duplicated;
 *   - every slice agrees on the stack and the shard count;
 *   - no surface url is owned by two shards (disjointness);
 *   - every owned surface's ownership matches the deterministic
 *     assignment (a slice can't smuggle in a surface it doesn't own).
 *
 * The result is a SurfaceInventory whose surface set is the exact union
 * of the slices — identical to what a single full crawl would pin (the
 * single-shard run owns everything, so its one slice IS the full
 * inventory). The caller marshals it through marshalInventory.
 */
export function mergeShardSlices(
  slices: ReadonlyArray<ShardSlice>,
  doc: string,
  stack: string,
): SurfaceInventory {
  if (slices.length === 0) {
    throw new Error('mergeShardSlices: no shard slices supplied');
  }
  const count = slices[0]!.shardCount;
  if (!Number.isInteger(count) || count < 1) {
    throw new Error(
      `mergeShardSlices: first slice declares a bad shardCount ${JSON.stringify(count)}`,
    );
  }

  const seenIndices = new Map<number, ShardSlice>();
  for (const slice of slices) {
    if (slice.stack !== stack) {
      throw new Error(
        `mergeShardSlices: slice (index ${slice.shardIndex}) pins stack ${JSON.stringify(slice.stack)} ` +
          `but the merge target stack is ${JSON.stringify(stack)}`,
      );
    }
    if (slice.shardCount !== count) {
      throw new Error(
        `mergeShardSlices: slice (index ${slice.shardIndex}) declares shardCount ${slice.shardCount}, ` +
          `expected ${count} — the shards ran with inconsistent CRAWL_SHARD_COUNT`,
      );
    }
    if (slice.shardIndex < 0 || slice.shardIndex >= count) {
      throw new Error(
        `mergeShardSlices: slice declares shardIndex ${slice.shardIndex} out of range [0, ${count})`,
      );
    }
    if (seenIndices.has(slice.shardIndex)) {
      throw new Error(
        `mergeShardSlices: shard index ${slice.shardIndex} appeared twice`,
      );
    }
    seenIndices.set(slice.shardIndex, slice);
  }
  for (let i = 0; i < count; i++) {
    if (!seenIndices.has(i)) {
      throw new Error(
        `mergeShardSlices: missing slice for shard index ${i} of ${count} — ` +
          `every shard must upload its slice or the union is incomplete (coverage gap)`,
      );
    }
  }

  const assignment = (idx: number): ShardAssignment => ({ index: idx, count });
  const byUrl = new Map<string, InventorySurface>();
  for (const slice of slices) {
    for (const surface of slice.surfaces) {
      // Disjointness: a surface owned by two shards is a partition bug.
      const prior = byUrl.get(surface.url);
      if (prior !== undefined) {
        throw new Error(
          `mergeShardSlices: surface ${JSON.stringify(surface.url)} owned by more than one shard — ` +
            `the partition is not disjoint`,
        );
      }
      // The slice can only legitimately own a surface the deterministic
      // assignment maps to it — guards a hand-tampered slice.
      if (!ownsSurface(surface.url, assignment(slice.shardIndex))) {
        throw new Error(
          `mergeShardSlices: shard ${slice.shardIndex} claims surface ${JSON.stringify(surface.url)} ` +
            `that the deterministic assignment does NOT map to it`,
        );
      }
      byUrl.set(surface.url, surface);
    }
  }

  const surfaces = [...byUrl.values()].sort((a, b) =>
    a.url.localeCompare(b.url),
  );
  return { doc, stack, surfaces };
}
