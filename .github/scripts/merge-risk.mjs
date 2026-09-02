// merge-risk.mjs — what this pull request's green does NOT cover.
//
// # The class (#2899)
//
// A pull request can be green on all seventeen required checks and still turn
// `main` red the moment it merges. Two independent mechanisms produce that one
// outcome, and both have already fired in this repository:
//
//   1. LANE SKIPPING. Several lanes that gate `main` or a release declare
//      `merge_posture: never` in `.github/ci-lanes.json` — `compatibility/*`,
//      the chDB round-trip and perf-guard shards, `perf-nightly`, the e2e
//      family. They run on push-to-main, on a schedule, or on a release PR,
//      never on an ordinary one. GitHub renders a lane that did not run as
//      nothing at all, so a skipped lane and a passing lane look identical on
//      the checks list. #2895 is what that costs: a 144-case Loki parity
//      regression merged green and sat on `main` for 32 hours and 31 red runs
//      before anyone read the run history.
//
//   2. STALE BASE. Branch protection deliberately does NOT require a branch to
//      be up to date before merging (`required_status_checks.strict: false`),
//      because forcing every one of ~20 PRs a day through an update-branch and
//      a full ~40-minute re-run serialises merges and spends the most CI
//      exactly when the repo is busiest. The cost of that trade is that a
//      regenerate-and-diff ratchet runs against the tree AS THE PR SAW IT, and
//      two PRs that each regenerate the same whole-tree artefact against
//      different bases can both be green and still leave `main` drifted.
//      `post-merge-golden-drift.mjs` caught exactly that with 8 drifted
//      cardinality artefacts and diagnosed it in those terms.
//
// # What this gate does about each, and why they differ
//
// STALE BASE is BLOCKING here, because it is precisely decidable from two git
// ranges and it fires only on a real collision. A golden artefact is a pure
// function of (its fixture corpus, its generator code). When neither side of
// the race moved generator code, each side's golden writes are determined by
// its own fixtures and the merged tree is simply their union — no drift. When
// EITHER side moved Go code, the function itself changed, and any golden the
// other side wrote was produced by the old one. So: a collision is both sides
// writing under the same shard's golden root while at least one side also
// changes Go. Same-FILE races need no help from this gate — git already
// reports those as merge conflicts and GitHub blocks them; the residual hole
// this closes is the different-files-same-artefact race, which merges clean.
//
// This is the targeted form of `strict: true`: it forces an update-branch on
// the handful of PRs that actually race a generated artefact, and leaves every
// other PR unserialised. It is not a policy change to branch protection and
// does not need one.
//
// LANE SKIPPING is REPORTED, not blocked, and that is a deliberate limit
// rather than a soft landing. Blocking it would fail nearly every code PR —
// an ordinary `internal/promql` change is unvalidated by `compatibility/
// prometheus`, three chDB shards, `quality.property` and `performance.profile`
// simultaneously, and the only escape from such a gate would be a per-PR
// waiver citation, which is the receipt-issue pattern #2893 exists to delete.
// What this gate does instead is make the skip legible: the lanes that gate
// `main` and will not run before this merges are named on the PR, with the
// local command that runs each one, so "green" is never silently read as
// "validated".
//
// # Attribution (#2902)
//
// Which lanes a change touches is DERIVED from the Go import graph, not read
// off each lane's declared `package_globs`. Those globs name a lane's own
// directories, and #2824 — the change that caused #2895, the very outage above
// — moved `internal/chclient`, `internal/chopt`, `internal/engine`,
// `internal/config` and `cmd/cerberus`, matching none of `compatibility.loki`'s
// three globs. The lane that would have caught the regression did not consider
// itself touched by the change that caused it. `lib/lane-closure.mjs` treats
// the declared globs as SEEDS and unions them with the dependency closure `go
// list` reports for those seeds, so the shared pipeline every head runs through
// is attributed to every head's lane, and a sibling head's change still is not.
// That module's header carries the full reasoning and the measured cost.
//
// # Env
//
//   BASE_SHA            start of the change's range (PR base, merge-group base,
//                       or push `before`). Falls back to HEAD^.
//   HEAD_SHA            end of the range. Falls back to HEAD.
//   MERGE_TARGET_REF    the ref this change will land on, for the stale-base
//                       comparison. Default `origin/main`. A ref that does not
//                       resolve (a push to main, where it is the same commit)
//                       disables the stale-base half rather than failing it.
//   CI_LANE_REGISTRY    lane registry path. Default `.github/ci-lanes.json`.
//   GITHUB_STEP_SUMMARY optional summary destination (written via lib/gh.mjs).
//
// Node builtins only, plus the Go toolchain the attribution derivation reads
// the import graph with. `process.exit(1)` on a stale-base collision, an
// unreadable registry, or an import graph that cannot be loaded — a derivation
// that silently degrades to the declared globs would report the pre-#2902
// answer while claiming the post-#2902 one. The lane inventory itself never
// exits non-zero.
//
// The pure halves are exported and pinned by merge-risk.test.mjs.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { matchesGlob } from './ci-lane-contract.mjs';
import { appendStepSummary, assertSafeArg, error, git, log, notice, warning } from './lib/gh.mjs';
import { SHARDS } from './lib/golden-shards.mjs';
import { laneAffectedGlobs } from './lib/lane-closure.mjs';

export const DEFAULT_REGISTRY_PATH = '.github/ci-lanes.json';
export const DEFAULT_MERGE_TARGET_REF = 'origin/main';

/** The extension that marks a generator-code change, as opposed to a data one. */
const GO_EXT = '.go';

// MAX_LISTED_FILES caps how many colliding paths each side of a stale-base
// report names. The reader needs enough to recognise the artefact, not the
// whole regeneration; a cardinality race can touch hundreds of shard files.
const MAX_LISTED_FILES = 6;

/** revExists — does `ref` name a commit in this checkout? */
export function revExists(ref, { run = git } = {}) {
  if (!ref) return false;
  return run(['rev-parse', '--verify', '--quiet', `${ref}^{commit}`]).status === 0;
}

/** changedFiles — the paths a git range touches, newest-name form. */
export function changedFiles(range, { run = git } = {}) {
  const res = run(['diff', '--name-only', range]);
  if (res.status !== 0) throw new Error(`git diff --name-only ${range} failed: ${res.stderr.trim()}`);
  return res.stdout.split('\n').filter(Boolean);
}

/** revParse — one revision resolved to a sha, or a thrown explanation. */
function revParse(args, { run = git } = {}) {
  const res = run(args);
  if (res.status !== 0) throw new Error(`git ${args.join(' ')} failed: ${res.stderr.trim()}`);
  return res.stdout.trim();
}

/**
 * goldenRootsOf — every path prefix a shard WRITES, as declared by
 * lib/golden-shards.mjs. Read from the shard table rather than restated, so a
 * shard that repoints its goldens moves this gate with it.
 */
export function goldenRootsOf(shards = SHARDS) {
  return Object.fromEntries(Object.entries(shards).map(([name, s]) => [name, s.goldens ?? []]));
}

/** underGoldenRoot — is `file` written by a shard whose golden root is `root`? */
export function underGoldenRoot(file, root) {
  return matchesGlob(file, root) || matchesGlob(file, `${root}/**`);
}

/** shardsWritten — which shards' golden roots this file set writes under. */
export function shardsWritten(files, roots = goldenRootsOf()) {
  const hit = new Map();
  for (const [name, prefixes] of Object.entries(roots)) {
    const written = (files ?? []).filter((f) => prefixes.some((p) => underGoldenRoot(f, p)));
    if (written.length > 0) hit.set(name, written);
  }
  return hit;
}

/**
 * staleBaseCollisions — the shards this change and its merge target BOTH
 * regenerate, where at least one side also moves generator code.
 *
 * Returns `[{ shard, ours, theirs }]`, ours/theirs being the written paths on
 * each side. Empty is the overwhelmingly common answer.
 */
export function staleBaseCollisions({ ours, theirs, roots = goldenRootsOf() }) {
  const codeMoved = [...(ours ?? []), ...(theirs ?? [])].some((f) => f.endsWith(GO_EXT));
  if (!codeMoved) return [];

  const mine = shardsWritten(ours, roots);
  const yours = shardsWritten(theirs, roots);
  const out = [];
  for (const [shard, written] of mine) {
    if (!yours.has(shard)) continue;
    out.push({ shard, ours: written, theirs: yours.get(shard) });
  }
  return out.sort((a, b) => a.shard.localeCompare(b.shard));
}

/**
 * gatingLanesThatSkipPRs — lanes that gate `main` or a release yet never run
 * on a pull request. `merge_posture: never` is the skip; a lane that also runs
 * nowhere afterwards (`main_posture: never` and no release requirement) gates
 * nothing and is not a risk, so it is excluded.
 */
export function gatingLanesThatSkipPRs(registry) {
  return (registry?.lanes ?? []).filter(
    (l) => l.merge_posture === 'never'
      && (l.main_posture !== 'never' || l.release_posture === 'required'),
  );
}

/**
 * unvalidatedLanes — of those, the ones whose AFFECTED paths this change
 * actually touches.
 *
 * `affectedGlobs` is `Map<laneID, string[]>`, normally the derived set from
 * `lib/lane-closure.mjs`'s `laneAffectedGlobs`. It is a required argument
 * rather than a fallback to `lane.package_globs`: attributing by the raw
 * declaration is exactly the #2902 blind spot, and a default that quietly
 * restored it would be indistinguishable from the fix at every call site. A
 * lane the map does not cover throws for the same reason.
 */
export function unvalidatedLanes(registry, files, affectedGlobs) {
  const out = [];
  for (const lane of gatingLanesThatSkipPRs(registry)) {
    const globs = affectedGlobs?.get(lane.id);
    if (!globs) throw new Error(`merge-risk: no affected-path set was derived for lane ${lane.id}`);
    const matched = (files ?? []).filter((f) => globs.some((g) => matchesGlob(f, g)));
    if (matched.length > 0) out.push({ lane, matched });
  }
  return out.sort((a, b) => a.lane.id.localeCompare(b.lane.id));
}

/** collisionReport — the printable failure for one stale-base collision. */
export function collisionReport({ shard, ours, theirs }, targetRef = DEFAULT_MERGE_TARGET_REF) {
  return (
    `stale base: this change and ${targetRef} both regenerate the \`${shard}\` golden shard against `
    + `different bases. Here: ${ours.slice(0, MAX_LISTED_FILES).join(', ')}`
    + `${ours.length > MAX_LISTED_FILES ? ` (+${ours.length - MAX_LISTED_FILES} more)` : ''}. `
    + `On ${targetRef} since this branch forked: ${theirs.slice(0, MAX_LISTED_FILES).join(', ')}`
    + `${theirs.length > MAX_LISTED_FILES ? ` (+${theirs.length - MAX_LISTED_FILES} more)` : ''}. `
    + 'A golden is a function of its corpus AND its generator code, and one side of this race moved '
    + `code, so whichever artefact lands second was written by the other's generator. Merge `
    + `${targetRef} into this branch and regenerate — \`just update-golden ${shard}\` locally, or `
    + `\`gh workflow run update-golden.yml -f shards=${shard} -f branch=<this branch>\` when the `
    + 'shard needs libchdb — then push. That makes the ratchet check the tree that actually lands.'
  );
}

async function main() {
  const targetRef = assertSafeArg(process.env.MERGE_TARGET_REF || DEFAULT_MERGE_TARGET_REF, 'MERGE_TARGET_REF');
  const registryPath = process.env.CI_LANE_REGISTRY || DEFAULT_REGISTRY_PATH;

  const head = assertSafeArg(process.env.HEAD_SHA || 'HEAD', 'HEAD_SHA');
  const baseSha = assertSafeArg(process.env.BASE_SHA || '', 'BASE_SHA');
  const base = revExists(baseSha) ? baseSha : `${head}^`;
  const ours = changedFiles(`${base}...${head}`);

  const summary = ['## merge-risk', ''];

  // --- stale base (blocking) -------------------------------------------------
  let collisions = [];
  if (!revExists(targetRef)) {
    summary.push(
      `Stale-base check skipped: \`${targetRef}\` does not resolve in this checkout, which is what a`
        + ' push to that same branch looks like — there is no other base to race.',
      '',
    );
  } else {
    const mergeBase = revParse(['merge-base', head, targetRef]);
    const theirs = changedFiles(`${mergeBase}..${targetRef}`);
    collisions = staleBaseCollisions({ ours, theirs });
    summary.push(
      collisions.length === 0
        ? `Stale base: no golden shard is regenerated by both this change and \`${targetRef}\`.`
        : `Stale base: **${collisions.length} colliding golden shard(s)** — see the error annotations.`,
      '',
    );
  }

  // --- unvalidated lanes (reported) -----------------------------------------
  let registry;
  try {
    registry = JSON.parse(readFileSync(registryPath, 'utf8'));
  } catch (e) {
    error(
      `merge-risk: ${registryPath} could not be read (${String(e?.message ?? e)}), so the lanes this `
        + 'change leaves unvalidated cannot be enumerated at all.',
      { title: 'merge-risk' },
    );
    process.exit(1);
  }

  const skipping = gatingLanesThatSkipPRs(registry);
  let affected;
  try {
    affected = laneAffectedGlobs(skipping, { repoRoot: process.cwd() });
  } catch (e) {
    error(
      `merge-risk: the Go import graph could not be loaded (${String(e?.message ?? e)}), so which lanes `
        + 'this change leaves unvalidated cannot be derived. Attributing by the declared `package_globs` '
        + 'instead would silently report the pre-#2902 answer, so this fails rather than degrades.',
      { title: 'merge-risk' },
    );
    process.exit(1);
  }
  const unvalidated = unvalidatedLanes(registry, ours, affected);
  summary.push(
    `### Lanes that gate \`main\` but do not run on this PR (${unvalidated.length} of ${skipping.length} touched)`,
    '',
  );
  if (unvalidated.length === 0) {
    summary.push(
      'No lane\'s affected paths match this change. Attribution is the dependency closure `go list`',
      'reports for each lane\'s own packages, so a change to the shared query pipeline counts against',
      'every head\'s lane, not only against the head it was filed under.',
      '',
    );
  } else {
    summary.push('| lane | validates | run it with |', '| --- | --- | --- |');
    for (const { lane } of unvalidated) {
      summary.push(`| \`${lane.id}\` | ${lane.description} | \`${lane.command ?? lane.recipes?.join(', ') ?? '—'}\` |`);
    }
    summary.push(
      '',
      'These run on push-to-main, a schedule, or a release PR — never on this one. A green PR is not',
      'evidence they pass. Attribution is the dependency closure `go list` reports for each lane\'s own',
      'packages, unioned with the non-Go paths it declares.',
      '',
    );
  }
  appendStepSummary(summary.join('\n'));

  if (unvalidated.length > 0) {
    warning(
      `merge-risk: ${unvalidated.length} lane(s) that gate main will not run before this merges — `
        + `${unvalidated.map((u) => u.lane.id).join(', ')}. A green PR does not mean they pass.`,
      { title: 'merge-risk' },
    );
  }
  for (const { lane } of unvalidated) log(`unvalidated lane: ${lane.id} (${lane.command ?? lane.id})`);

  if (collisions.length > 0) {
    for (const c of collisions) error(`merge-risk: ${collisionReport(c, targetRef)}`, { title: 'merge-risk' });
    process.exit(1);
  }

  notice(
    `merge-risk: no golden shard collides with ${targetRef}; `
      + `${unvalidated.length} gating lane(s) will not run before this merges.`,
    { title: 'merge-risk' },
  );
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((e) => {
    error(`merge-risk: ${String(e?.message ?? e)}`, { title: 'merge-risk' });
    process.exit(1);
  });
}
