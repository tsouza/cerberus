// scope-gate.mjs — the shared decision behind "does THIS pull request touch the
// scope a heavy lane guards?", used by the `mutation` and `compose-smoke` lanes.
//
// Why this exists
// ---------------
// Both lanes are expensive enough that they were moved off the ordinary-PR path
// wholesale: the matrix job carried a job-level `if:` that skipped every leg
// unless the event was a push, a schedule, a dispatch, or a `release/*` PR, and
// the required aggregator read a skipped matrix as a green pass-through. That
// bought PR wall-clock at the price of a HOLLOW GREEN — a PR could regress
// exactly what the lane guards, merge with the required context reporting
// green, and only surface the regression after the merge (push-to-main) or, in
// the worst case, at release time, where it blocks the release instead of the
// PR that caused it. Both blockers of the v1.13.2 cycle arrived that way.
//
// The fix is not "run everything on every PR" (that reinstates the wall-clock
// bill this skip was introduced to remove) and not "skip and hope". It is to
// run the legs whose SCOPE the PR actually changed: a PR that edits
// internal/chplan runs the chplan mutation leg and nothing else; a PR that
// edits only docs runs none. Cost tracks the diff, coverage tracks the diff,
// and the class of "regressed a lane that never ran" closes.
//
// The pieces are shared here rather than duplicated per lane because the
// non-obvious parts — which events must always run everything, and what to do
// when the diff cannot be computed — are exactly the parts that are dangerous
// to get subtly different in two places.
//
// node: builtins only — no npm deps, no setup-node needed.

import { git, warning } from './gh.mjs';

// Events that must run the FULL lane, never a diff-scoped subset:
//
//   push / schedule / workflow_dispatch — these are the safety net the scoped
//     PR path leans on. A PR selects legs from its own diff, so a leg whose
//     scope no PR happened to touch (an indirect dependency, a generated
//     golden, a toolchain bump landing through another path) is still swept
//     here. Scoping THESE would leave the sweep with no floor at all.
//
//   pull_request with a `release/*` head — the release-staging PR is the last
//     gate before a tag exists, and RELEASE_REQUIRED_CHECKS names these lanes.
//     It runs the whole thing regardless of how small its own diff is (a
//     release PR's diff is a version bump and a CHANGELOG — nearly empty by
//     construction, and exactly the moment full evidence is required).
export function runsFullLane({ eventName, headRef }) {
  if (eventName !== 'pull_request') return true;
  return String(headRef ?? '').startsWith('release/');
}

// changedPaths — the repo-relative paths this PR touches, as a Set.
//
// Returns null when the diff CANNOT be computed (missing refs, a shallow
// checkout, an unrelated-history merge-base failure). null is not "nothing
// changed" — the caller must read it as "run the full lane". Collapsing an
// unknown diff into an empty selection is the same hollow green this module
// exists to close, so the ambiguity is preserved in the type rather than
// resolved to the cheap answer.
export function changedPaths({ baseSha, headSha }) {
  const base = String(baseSha ?? '').trim();
  const head = String(headSha ?? '').trim();
  if (!base || !head) {
    warning(`scope-gate: base ("${base}") or head ("${head}") sha missing — running the full lane.`);
    return null;
  }

  // Three-dot semantics (changes on the HEAD side since the merge base) is the
  // question being asked: "what did this PR do", not "how does it differ from
  // the tip of main". Resolving the merge base explicitly rather than relying
  // on `a...b` lets a missing base commit surface as a diagnosable failure here
  // instead of an empty diff further down.
  const mb = git(['merge-base', base, head]);
  if (mb.status !== 0) {
    warning(
      `scope-gate: \`git merge-base ${base} ${head}\` failed (${mb.stderr.trim()}) — running the full lane.`,
    );
    return null;
  }

  const diff = git(['diff', '--name-only', `${mb.stdout.trim()}`, head]);
  if (diff.status !== 0) {
    warning(`scope-gate: \`git diff\` failed (${diff.stderr.trim()}) — running the full lane.`);
    return null;
  }

  return new Set(
    diff.stdout
      .split('\n')
      .map((l) => l.trim())
      .filter(Boolean),
  );
}

// underPrefix — is `path` inside directory `prefix` (or the prefix itself)?
//
// Compared segment-wise rather than with a bare `startsWith`, so `internal/log`
// does not claim `internal/logql/lower.go`. Both sides are normalised from the
// `./internal/logql` form the matrix scopes use to the `internal/logql` form
// git reports.
export function underPrefix(path, prefix) {
  const p = normalise(path);
  const dir = normalise(prefix);
  if (dir === '') return true;
  return p === dir || p.startsWith(`${dir}/`);
}

export function normalise(p) {
  return String(p ?? '')
    .replace(/^\.\//, '')
    .replace(/\/+$/, '');
}

// matchesAny — does `path` fall under ANY of `prefixes`? A prefix ending in a
// non-directory name still matches the exact file, so a single file can be
// named as its own scope entry.
export function matchesAny(path, prefixes) {
  return prefixes.some((prefix) => underPrefix(path, prefix));
}
