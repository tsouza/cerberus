// crawl-inventory-merge.mjs — union-merge the per-shard crawl surface
// slices emitted by the sharded BFS frontier crawl (e2e.yml dashboard
// lane) into ONE full surface inventory, asserting the union is an
// EXACT, DISJOINT cover.
//
// Why this exists
// ---------------
// crawl/crawl.spec.ts is one async BFS test(); the only way below its
// full-depth wall-clock floor is to shard the frontier (see
// test/e2e/playwright/crawl/sharding.ts). Each shard runs the WHOLE
// cheap discovery walk but only audits + pins the ~1/N of surfaces it
// OWNS (fnv1a(path) % CRAWL_SHARD_COUNT == CRAWL_SHARD_INDEX). On the
// bootstrap/regen dispatch (update_crawl_inventory=true) every shard
// writes its owned slice as an artifact; this script downloads them all,
// unions them, and writes the full inventory — IDENTICAL to what a
// single full crawl would pin (the single-shard run owns everything, so
// its one slice IS the full inventory).
//
// THE COVERAGE GUARANTEE lives in the assertions: a surface missing from
// every slice (a discovery gap), a surface owned by two shards (a
// non-disjoint partition), a missing shard slice, or a slice that
// smuggles in a surface the deterministic assignment doesn't map to it,
// all `::error::` + exit 1. The merge can never silently drop or double
// a surface.
//
// This mirrors test/e2e/playwright/crawl/sharding.ts:mergeShardSlices
// (the TS the spec runs); the two are kept in lockstep — the same FNV-1a
// hash, the same path-only ownership key, the same disjoint-union rules.
// The unit guard (crawl-inventory-merge.test.mjs) pins that lockstep on
// a sample partition.
//
// Modes (env MODE or argv[2]; default `merge`):
//   - merge  : read every $SLICES_DIR/grafana-surface-slice.<stack>.shard-*.json,
//              union + assert, write $INVENTORY_OUT (the canonical
//              marshalled inventory, byte-for-byte the spec's
//              marshalInventory output). exit 1 on any partition
//              violation.
//   - verify : run the same assertions over the slices without writing —
//              for a dry CI check.
//
// Env:
//   MODE          `merge` | `verify`; default `merge`.
//   SLICES_DIR    dir holding the downloaded per-shard slice JSONs.
//   STACK         stack name the slices + inventory pin (e.g. `k3d`).
//   INVENTORY_DOC the `doc` header to write into the merged inventory.
//   INVENTORY_OUT (merge) path to write the full inventory to.
//
// node: builtins only (via lib/gh.mjs) — no npm deps, no setup-node.

import process from 'node:process';
import { readFileSync, writeFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { error, notice, log } from './lib/gh.mjs';

// FNV-1a 32-bit — MUST match sharding.ts fnv1a32 exactly so this script
// agrees with the spec on who owns what.
const FNV_OFFSET_BASIS_32 = 0x811c9dc5;
const FNV_PRIME_32 = 0x01000193;

export function fnv1a32(s) {
  let h = FNV_OFFSET_BASIS_32;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, FNV_PRIME_32) >>> 0;
  }
  return h >>> 0;
}

// PATH-only ownership key — strip the `#<state>` suffix AND the `?<query>`
// so a surface, its structural-param children, and its in-place states
// all hash to one shard. MUST match sharding.ts baseSurfaceOf.
export function baseSurfaceOf(stateKey) {
  const noState = stateKey.split('#', 1)[0] ?? stateKey;
  return noState.split('?', 1)[0] ?? noState;
}

export function ownsSurface(stateKey, index, count) {
  if (count === 1) return true;
  return fnv1a32(baseSurfaceOf(stateKey)) % count === index;
}

// Canonical inventory marshalling — MUST be byte-for-byte identical to
// sharding/lib.ts marshalInventory so the committed file and the
// merge-written file agree (the spec asserts the committed inventory
// round-trips through the marshaller).
export function marshalInventory(inv) {
  const surfaces = [...inv.surfaces].sort((a, b) => a.url.localeCompare(b.url));
  return `${JSON.stringify({ doc: inv.doc, stack: inv.stack, surfaces }, null, 2)}\n`;
}

// collectViolations() — pure: returns a string[] of partition violations
// (empty == a clean, total, disjoint cover). Collect-then-fail so a
// maintainer sees every problem in one run.
export function collectViolations(slices) {
  const v = [];
  if (slices.length === 0) {
    v.push('no shard slices supplied');
    return v;
  }
  const count = slices[0].shardCount;
  if (!Number.isInteger(count) || count < 1) {
    v.push(`first slice declares a bad shardCount ${JSON.stringify(count)}`);
    return v;
  }

  const seen = new Map();
  for (const s of slices) {
    if (s.shardCount !== count) {
      v.push(`slice (index ${s.shardIndex}) declares shardCount ${s.shardCount}, expected ${count} — inconsistent CRAWL_SHARD_COUNT`);
    }
    if (!Number.isInteger(s.shardIndex) || s.shardIndex < 0 || s.shardIndex >= count) {
      v.push(`slice declares shardIndex ${s.shardIndex} out of range [0, ${count})`);
      continue;
    }
    if (seen.has(s.shardIndex)) {
      v.push(`shard index ${s.shardIndex} appeared twice`);
    }
    seen.set(s.shardIndex, s);
  }
  for (let i = 0; i < count; i++) {
    if (!seen.has(i)) {
      v.push(`missing slice for shard index ${i} of ${count} — every shard must upload its slice or the union is incomplete (coverage gap)`);
    }
  }

  const byUrl = new Map();
  for (const s of slices) {
    for (const surface of s.surfaces || []) {
      if (byUrl.has(surface.url)) {
        v.push(`surface ${JSON.stringify(surface.url)} owned by more than one shard — the partition is not disjoint`);
        continue;
      }
      if (!ownsSurface(surface.url, s.shardIndex, count)) {
        v.push(`shard ${s.shardIndex} claims surface ${JSON.stringify(surface.url)} that the deterministic assignment does NOT map to it`);
      }
      byUrl.set(surface.url, surface);
    }
  }
  return v;
}

// mergeShardSlices() — assert + union into a full inventory.
export function mergeShardSlices(slices, doc, stack) {
  for (const s of slices) {
    if (s.stack !== stack) {
      throw new Error(`slice (index ${s.shardIndex}) pins stack ${JSON.stringify(s.stack)} but the merge target stack is ${JSON.stringify(stack)}`);
    }
  }
  const v = collectViolations(slices);
  if (v.length > 0) {
    const err = new Error(`partition violations:\n  - ${v.join('\n  - ')}`);
    err.violations = v;
    throw err;
  }
  const byUrl = new Map();
  for (const s of slices) {
    for (const surface of s.surfaces || []) byUrl.set(surface.url, surface);
  }
  const surfaces = [...byUrl.values()].sort((a, b) => a.url.localeCompare(b.url));
  return { doc, stack, surfaces };
}

const SLICE_PREFIX = 'grafana-surface-slice.';

function loadSlices(dir, stack) {
  const want = `${SLICE_PREFIX}${stack}.shard-`;
  const files = readdirSync(dir).filter(
    (f) => f.startsWith(want) && f.endsWith('.json'),
  );
  if (files.length === 0) {
    error(`crawl-inventory-merge: no slice files matching ${want}*.json under ${dir}`);
    process.exit(1);
  }
  return files.map((f) => {
    const parsed = JSON.parse(readFileSync(join(dir, f), 'utf8'));
    if (!Array.isArray(parsed.surfaces)) {
      error(`crawl-inventory-merge: ${f} has no surfaces[] array`);
      process.exit(1);
    }
    return parsed;
  });
}

function run(mode) {
  const dir = process.env.SLICES_DIR;
  const stack = process.env.STACK;
  if (!dir || !stack) {
    error('crawl-inventory-merge: SLICES_DIR and STACK are required');
    process.exit(1);
  }
  const slices = loadSlices(dir, stack);
  let inv;
  try {
    inv = mergeShardSlices(slices, process.env.INVENTORY_DOC || '', stack);
  } catch (e) {
    for (const m of e.violations || [String(e.message)]) {
      error(`crawl-inventory-merge: ${m}`, { title: 'crawl shard partition violation' });
    }
    error(`crawl-inventory-merge: the shard slices are not a clean, disjoint, total cover — fix the crawl shard run, never relax the merge`);
    process.exit(1);
  }
  notice(
    `crawl-inventory-merge: merged ${slices.length} shard slice(s) into ${inv.surfaces.length} surface(s) for stack ${stack}.`,
  );
  if (mode === 'merge') {
    const out = process.env.INVENTORY_OUT;
    if (!out) {
      error('crawl-inventory-merge: INVENTORY_OUT is required in merge mode');
      process.exit(1);
    }
    writeFileSync(out, marshalInventory(inv));
    log(`crawl-inventory-merge: wrote ${out}`);
  }
  process.exit(0);
}

const invokedDirectly =
  process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) {
  const mode = (process.env.MODE || process.argv[2] || 'merge').toLowerCase();
  if (mode === 'merge' || mode === 'verify') run(mode);
  else {
    error(`crawl-inventory-merge: unknown MODE "${mode}" (want merge|verify)`);
    process.exit(1);
  }
}

export { SLICE_PREFIX };
