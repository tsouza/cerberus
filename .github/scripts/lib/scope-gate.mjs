// scope-gate.mjs — the shared decision behind "does THIS change touch the scope
// a heavy lane guards?", used by the `mutation` and `compose-smoke` lanes. The
// change is a pull request's diff or a merge-queue batch's diff; both reduce to
// a base/head pair of SHAs.
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

// Events that run a diff-scoped subset rather than the FULL lane. Everything
// not named here runs everything, so an event nobody anticipated fails OPEN.
//
//   pull_request — scoped to its own diff, unless the head branch is
//     `release/*`. The release-staging PR is the last gate before a tag exists
//     and RELEASE_REQUIRED_CHECKS names these lanes, so it runs the whole thing
//     regardless of how small its own diff is (a release PR's diff is a version
//     bump and a CHANGELOG — nearly empty by construction, and exactly the
//     moment full evidence is required).
//
//   merge_group — scoped to the batch's own diff, for the same reason and by
//     the same arithmetic as a pull request: `base_sha..head_sha` is precisely
//     "what this batch adds on top of main", which is the union of the queued
//     PRs' diffs. A merge-queue entry is a PRE-merge gate on a projected trunk,
//     so it must ask the question a PR asks; running the full sweep there would
//     charge every batch the whole matrix on top of the post-merge push run
//     that already sweeps it, which is the wall-clock bill scoping exists to
//     remove. The queue does NOT weaken the floor: `main` still advances to the
//     merge group's own head SHA, and the push-to-main sweep (unscoped, below)
//     runs on that commit exactly as it does today.
//
//     A `release/*` PR travelling through the queue is scoped here — the merge
//     group payload carries no constituent head_ref to test — but it is not
//     un-gated: it swept the full lane on its own `pull_request` event, and
//     sweeps it again on the push that lands it.
//
// Events NOT named here, and why the full lane is right for them:
//
//   push / schedule / workflow_dispatch — the safety net every scoped path
//     leans on. A leg whose scope no PR happened to touch (an indirect
//     dependency, a generated golden, a toolchain bump landing through another
//     path) is still swept here. Scoping THESE would leave the sweep with no
//     floor at all.
const DIFF_SCOPED_EVENTS = ['pull_request', 'merge_group'];

export function runsFullLane({ eventName, headRef }) {
  if (!DIFF_SCOPED_EVENTS.includes(eventName)) return true;
  return String(headRef ?? '').startsWith('release/');
}

// changedPaths — the repo-relative paths this change touches, as a Set.
//
// Callers pass `github.event.pull_request.base.sha` / `.head.sha` on a pull
// request and `github.event.merge_group.base_sha` / `.head_sha` in a merge
// queue. Both pairs answer the same question; a caller that opts a lane into
// `merge_group:` must wire the merge-group pair, or the missing-sha branch
// below runs the full lane.
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
