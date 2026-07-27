// shard-coverage.mjs — the Playwright spec-partition coverage rules shared by
// the two e2e shard-matrix modules (compose-smoke-matrix.mjs and
// dashboard-matrix.mjs).
//
// Both lanes answer the same question — "is every tracked spec either ASSIGNED
// to exactly one shard or EXPLICITLY excluded?" — and the whole point of those
// gates is that a spec can never silently go unrun. One implementation means a
// new rule protects BOTH partitions; a per-lane copy would leave whichever lane
// nobody remembered to edit unguarded.
//
// Env:
//   PLAYWRIGHT_DIR  glob root for discoverSpecs(); default `test/e2e/playwright`.
//
// node: builtins only (via gh.mjs) — no npm deps, no setup-node needed.

import process from 'node:process';
import { lsFiles } from './gh.mjs';

export const PW_DIR = process.env.PLAYWRIGHT_DIR || 'test/e2e/playwright';

// Shard names render straight into a matrix `name` -> child check context
// (`<lane>-shard (shard-x)`) + the per-shard artifact name. Keep them
// filename-safe and branch-protection-stable.
export const SHARD_NAME_RE = /^[a-z0-9-]+$/;

const stripDir = (p) => p.replace(new RegExp(`^${PW_DIR}/`), '');

// discoverSpecs() — the tracked spec universe. Two explicit globs (the dir
// root + the crawl/ subdir) rather than `**` so a future deeper subdir is
// itself a reviewable event, not silently vacuumed into scope.
export function discoverSpecs() {
  return lsFiles([`${PW_DIR}/*.spec.ts`, `${PW_DIR}/crawl/*.spec.ts`]).map(stripDir);
}

// collectShardCoverageViolations() — returns { violations, owners }:
// `violations` is a string[] of human-readable problems (empty == clean),
// `owners` the spec -> [owning shard…] map so a lane can append its own rules
// without re-deriving it. Collect-then-fail (not fail-fast) so a maintainer
// reworking a partition sees every problem in one run.
//
// `emptySubstrate` names what an empty shard would boot for nothing ("a compose
// stack" / "a k3d cluster"), so the message reads in the lane's own terms.
export function collectShardCoverageViolations({ discovered, shards, excluded, emptySubstrate }) {
  const v = [];
  const dset = new Set(discovered);
  if (dset.size !== discovered.length) {
    v.push('discovery returned duplicate paths');
  }

  // Shard hygiene + build the spec -> [owning shard…] map.
  const owners = new Map();
  const names = new Set();
  for (const s of shards) {
    if (!SHARD_NAME_RE.test(s.name)) {
      v.push(`bad shard name "${s.name}" (must match ${SHARD_NAME_RE})`);
    }
    if (names.has(s.name)) {
      v.push(`duplicate shard name: ${s.name}`);
    }
    names.add(s.name);
    if (!s.specs || s.specs.length === 0) {
      v.push(`empty shard (would boot ${emptySubstrate} to run nothing): ${s.name}`);
      continue;
    }
    for (const spec of s.specs) {
      owners.set(spec, [...(owners.get(spec) || []), s.name]);
    }
  }

  const assigned = new Set(owners.keys());
  const exset = new Set(excluded);

  if (exset.size !== excluded.length) {
    v.push('EXCLUDED contains duplicate entries');
  }

  // double-assignment (wasted work + a spec running on two substrates).
  for (const [spec, who] of owners) {
    if (who.length > 1) {
      v.push(`double-assigned spec ${spec} -> shards [${who.join(', ')}]`);
    }
  }
  // assigned AND excluded — a contradiction.
  for (const spec of assigned) {
    if (exset.has(spec)) {
      v.push(`spec is both assigned and excluded: ${spec}`);
    }
  }
  // stale exclude: names a spec that no longer exists (rename/delete) — it
  // could be masking a coverage hole, so it's a hard error not a warning.
  for (const spec of exset) {
    if (!dset.has(spec)) {
      v.push(`excluded spec not found on disk (stale exclude / rename?): ${spec}`);
    }
  }
  // phantom assignment: a shard lists a spec that isn't on disk.
  for (const spec of assigned) {
    if (!dset.has(spec)) {
      v.push(`phantom spec (assigned but not on disk): ${spec} [shard ${owners.get(spec).join(', ')}]`);
    }
  }
  // THE coverage gap: discovered, neither assigned nor excluded.
  for (const spec of discovered) {
    if (!assigned.has(spec) && !exset.has(spec)) {
      v.push(
        `UNASSIGNED spec (silent coverage gap): ${spec} — assign it to a shard in SHARDS or add it to EXCLUDED with a reason`,
      );
    }
  }
  return { violations: v, owners };
}
