// release-preflight.mjs — the green-check guard for the release path. It runs
// on BOTH release paths and refuses to publish unless the tree being released
// is provably green.
//
// Two MODES, selected from the pushed branch:
//   MAINLINE     (`main`) — the merge commit of a release PR. Branch protection
//                already refused to merge a PR whose REQUIRED checks were red,
//                but that is not the whole story: a lane with no
//                `pull_request:` trigger (today `migration-e2e`) has NEVER run
//                on the tree being released, so the merge commit is the FIRST
//                tree it sees. The mainline preflight is what makes those lanes
//                gate. release.yml gates the job on "something actually
//                publishes", so an ordinary merge pays nothing for it.
//   MAINTENANCE  (`release/<major>.<minor>.x`) — a hotfix cherry-picked
//                straight onto the line, with NO PR gate at all. Everything
//                mainline mode does, PLUS two maintenance-only rules: the pushed
//                commit must be the current TIP of its line, and the line must
//                be inside the support window (an EOL line gets no hotfixes).
//                Both are maintenance concepts — `main` legitimately moves
//                under a release that is still waiting on CI, and the EOL
//                window describes maintenance lines.
//
// ABSENCE IS A FAILURE (the rule an observation-derived gate cannot express):
//   The green-check evaluation below iterates the check-runs the API RETURNED,
//   so a lane that never ran contributes zero entries and therefore zero
//   problems — a silently smaller gate that still reports "all N gated check(s)
//   are settled green". RELEASE_REQUIRED_CHECKS is the EXPECTED set, and it is
//   NOT derived from observation: every name in it must have posted a check-run
//   on the commit, and a missing one is a blocking problem. An EMPTY
//   RELEASE_REQUIRED_CHECKS is itself a blocking problem, so the gate cannot
//   degrade back to pure observation by deleting one workflow env line. A
//   required name that RELEASE_INFORMATIONAL_CHECKS would swallow by prefix, or
//   that RELEASE_SELF_JOBS excludes, is a WIRING ERROR and blocks too —
//   de-gating a required lane is not a de-gate, it is a hole.
//
// WAIT-then-EVALUATE (why it can't snapshot once):
//   release.yml fires on the SAME push event as the CI workflows
//   (ci / e2e / compatibility / chdb / migration-e2e / …), so a one-shot
//   snapshot taken the moment preflight starts sees every lane `queued` /
//   `in_progress` and aborts with "still queued (not completed)" — a guaranteed
//   false negative that used to force a manual re-run of release.yml after CI
//   finished. So the driver first POLLS until BOTH (a) every check-SUITE except
//   this release run's own is `completed` AND (b) every RELEASE_REQUIRED_CHECKS
//   name has posted a COMPLETED check-run, THEN runs the one-shot
//   green/tip/EOL evaluation. Condition (b) is what makes a lane that never
//   started fail on the timeout rather than pass on its silence: a suite-only
//   wait is `done` the instant the suites that DID run finish. Waiting on our
//   own suite would deadlock (it is necessarily in-progress while we poll), so
//   it is resolved by GITHUB_RUN_ID -> check_suite_id and excluded. The wait is
//   bounded (releaseWaitMinutes / pollIntervalMs); on timeout the preflight
//   ABORTS naming the MISSING and the still-RUNNING lanes separately —
//   fail-safe, never publish on an unknown/incomplete state.
//
// The rule, with no softening:
//   1. Every RELEASE_REQUIRED_CHECKS name must have posted a check-run on the
//      commit. Silence is a failure, not a pass. (Both modes.)
//   2. Every gated check-run + legacy status on the commit must be COMPLETED
//      (nothing still running / queued) AND green (success / skipped / neutral).
//      One running check, one failure, one cancelled/timed-out lane -> release
//      ABORTS. Two name sets are excluded: this release run's OWN jobs
//      (RELEASE_SELF_JOBS, structural — gating on them deadlocks) and explicitly
//      de-gated INFORMATIONAL lanes (RELEASE_INFORMATIONAL_CHECKS, prefix-matched
//      — the compose-smoke-shard-info crawl shard, the dashboard smoke). Those
//      are deliberately not part of the required gate (the required compose-smoke
//      aggregate already rolls up the gating shards), so a flake there must not
//      block a hotfix. Everything else gates by default — a new lane is never
//      silently un-gated. (Both modes.)
//   3. The pushed commit MUST be the current tip of the `release/*.x` branch
//      it was pushed to. You release the tip of a maintenance line, never an
//      older/side commit. (For a branch push GITHUB_SHA is normally already the
//      tip; the check defends against a stale re-drive racing a newer push.)
//      MAINTENANCE only.
//   4. The maintenance line must be INSIDE the support window — the latest
//      SUPPORTED_MINOR_LINES minor lines (current + the two prior). A push to a
//      line that is end-of-life (3+ minors behind the highest released minor)
//      is REFUSED: an EOL line gets no further hotfixes. See the "Release
//      support window / EOL policy" subsection of docs/operations.md.
//      MAINTENANCE only.
//
// The ONLY structural exclusion is THIS release run's own jobs (gate /
// preflight / goreleaser / release-artifact-migration / publish / brew-smoke /
// chart-release): they are necessarily in-progress while the preflight runs, so
// gating on them would deadlock. They are identified by name via
// RELEASE_SELF_JOBS — structural, not a flakiness heuristic. A reusable
// workflow called from one of those jobs posts its children as
// "<job> / <child>", so self-job matching also covers that prefix
// (REUSABLE_JOB_SEPARATOR); without it, `release-artifact-migration`'s children
// would be gated on by the very preflight that must finish before they start.
//
// `skipped` counts as green: a job whose path-filter / `if:` guard deliberately
// did not run is a settled non-failure (e.g. `changes` / `gate` no-ops), not a
// red and not "still running". Treating it as a failure would make the gate
// impossible to satisfy.
//
// Re-runs leave several check-runs with the same name; the latest (highest id)
// is the check's current state, so a green re-run supersedes an earlier red.
//
// Env contract (the single source of truth):
//   GITHUB_TOKEN       token with checks:read + statuses:read + contents:read.
//   GITHUB_REPOSITORY  "owner/name".
//   GITHUB_SHA         the pushed commit SHA.
//   GITHUB_REF_NAME    the pushed branch name. `release/<major>.<minor>.x`
//                      selects MAINTENANCE mode; anything else (in practice
//                      `main`) selects MAINLINE mode.
//   GITHUB_API_URL     API base (default https://api.github.com).
//   GITHUB_RUN_ID      this release run's id — resolved to its check_suite_id so
//                      the wait phase can skip our own (in-progress) suite.
//                      Auto-present in the Actions runtime; absent off-runner.
//   RELEASE_SELF_JOBS  comma-separated check-run names belonging to THIS release
//                      workflow, excluded from the gate.
//   RELEASE_REQUIRED_CHECKS
//                      comma-separated EXACT check-run names that MUST have
//                      posted a run on the commit. The expected set — not
//                      derived from the API response, which is the whole point.
//                      Empty/unset is a blocking problem.
//   RELEASE_INFORMATIONAL_CHECKS
//                      comma-separated name PREFIXES of explicitly de-gated
//                      informational lanes. An entry that swallows a
//                      RELEASE_REQUIRED_CHECKS name is a wiring error.
//
// `evaluate(...)` takes the branch HEAD sha, the pushed sha, the raw check-runs,
// the legacy statuses, the self-job name set, the REQUIRED name set, the mode,
// and the released-version tags, and returns a list of blocking problems (empty
// == release may proceed) plus the count of gated checks. It is a ONE-SHOT
// decision run AFTER the wait phase has confirmed the CI matrix settled. No
// network — exported so the self-test and release-preflight.test.mjs pin the
// exact pass/fail boundary. The support-window check is a pure helper
// (`supportWindowProblem`) folded into the same problems list.
//
// `allSuitesSettled(suites, ownSuiteId, workflowNames)` is the pure decision
// behind the wait loop: a check-suite is settled when `status === "completed"`;
// the release run's own suite (`ownSuiteId`) is ignored. `workflowNames` maps
// suite id -> workflow run name so `pending` names the workflow being waited on
// rather than the app that produced it (which is `github-actions` for all of
// them). Returns { done, pending } — side-effect free so the self-test pins it;
// the polling / sleep wrapper lives in main().
//
// `requiredChecksPending(checkRuns, required)` is its companion: the required
// names that have posted NO check-run (`missing`) and those whose latest run is
// not yet `completed` (`running`). The wait loop keeps polling while either is
// non-empty, so a required lane that never starts times out loudly instead of
// being read as absent-and-therefore-fine.
//
// argv `--self-test` runs the in-process assertion suite and exits. The same
// pure core is covered by `release-preflight.test.mjs` on the required `lint`
// lane — release.yml has no `pull_request:` trigger, so `--self-test` alone
// would leave every change here unverified until a release is cut.
//
// Imports only node: builtins (Node ships `fetch`). Run with
// `node .github/scripts/release-preflight.mjs`.

import process from 'node:process';

// A maintenance line is `release/<major>.<minor>.x` — `release/1.4.x`,
// `release/1.3.x`. It explicitly does NOT match a main release PR branch like
// `release/v1.5.0-chart-0.6.4` (those don't end in `.x`).
export const MAINTENANCE_BRANCH_RE = /^release\/(\d+)\.(\d+)\.x$/;

// The two evaluation modes. MAINTENANCE additionally enforces the branch-tip
// and support-window rules; MAINLINE does not (main legitimately moves under a
// release that is still waiting on CI, and the EOL window is a maintenance
// concept). Everything else — the required-set diff and the green gate — is
// identical, which is what makes the mainline path a real gate rather than the
// no-preflight hole it used to be.
export const MODE_MAINTENANCE = 'maintenance';
export const MODE_MAINLINE = 'mainline';

// A reusable workflow called from job `X` posts its child check-runs as
// "X / <child>". Self-job exclusion has to cover those children too: they are
// downstream of this preflight, so gating on them deadlocks the release.
export const REUSABLE_JOB_SEPARATOR = ' / ';

// Conclusions that count as a settled, non-blocking check-run.
const GREEN_CONCLUSIONS = new Set(['success', 'skipped', 'neutral']);

// Cerberus supports the latest N minor release lines: the current minor plus
// the two prior. A maintenance line that falls 3+ minors behind the highest
// released minor is end-of-life — no further hotfixes. See the "Release support
// window / EOL policy" subsection of docs/operations.md.
export const SUPPORTED_MINOR_LINES = 3;

// A released app tag, `v<major>.<minor>.<patch>` (stable only — a prerelease
// suffix like `-rc.1` does NOT establish a new supported line).
const APP_TAG_RE = /^v(\d+)\.(\d+)\.\d+$/;

// A just-published app version, `<major>.<minor>.<patch>` (no `v`, stable). The
// active-EOL retirement helper takes this shape (it is what the release gate
// publishes — `release-version-gate.mjs` emits `version` without the `v`).
const APP_VERSION_RE = /^(\d+)\.(\d+)\.(\d+)$/;

// Wait-phase bounds. The preflight polls until every non-own check-suite is
// `completed` AND every required lane has posted a completed run, then evaluates
// once. releaseWaitMinutes caps the total wall-clock wait (after which the
// preflight ABORTS — fail-safe, never publish on an unknown state);
// pollIntervalMs is the gap between polls.
//
// The ceiling has to outlast the SLOWEST required lane, which is now
// migration-e2e's serial path: setup (10) + tier1 (45) + tier2 (60) = 115 min
// of job timeouts, plus runner queueing. 150 leaves headroom without letting a
// wedged lane hold a release runner indefinitely.
const releaseWaitMinutes = 150;
const maxWaitMs = releaseWaitMinutes * 60 * 1000;
const pollIntervalMs = 30 * 1000; // 30 s between polls
// Polls to skip before re-stating an unchanged wait state, so a long quiet
// stretch still proves the job is alive without burying the log in repeats.
const quietHeartbeatPolls = 10;

// ---------------------------------------------------------------------------
// pure core (exported for the self-test — no network, no process.exit)
// ---------------------------------------------------------------------------

// allSuitesSettled — the pure decision behind the wait loop. Given the commit's
// check-suites and THIS release run's own suite id (which is necessarily
// in-progress and must be ignored — waiting on it deadlocks), return
// { done, pending } where `done` is true iff every other suite has
// `status === "completed"`, and `pending` lists the names of the suites still
// running. The name is only ever used for the abort/notice message, never for
// the gate decision.
//
// `workflowNames` (suite id -> workflow run name) is what makes that message
// legible. `app.slug` is NOT a usable name here: every suite produced by an
// Actions workflow reports the same slug, `github-actions`, so a message built
// from it renders as "12 check-suite(s) still running (github-actions,
// github-actions, ...)" — it names the app, and the app is always the same one.
// The workflow name is the thing a maintainer is actually waiting on ("e2e",
// "mutation"), so prefer it and fall back to the app slug only for suites no
// workflow run claims (third-party apps like GitGuardian, where the slug IS the
// distinguishing name).
//
// A suite with NO check-runs (`latest_check_runs_count === 0`) is ignored: an
// app can register a check-suite yet never post a run (e.g. GitGuardian leaves a
// perpetually-`queued` empty suite on commits it doesn't scan). Such a suite
// never reaches `completed`, so waiting on it would hang the preflight until
// timeout — and it contributes zero check-runs to the green-gate (`evaluate`),
// so it is irrelevant to settledness. Treat empty suites as already settled.
// parseCheckList reads one of the RELEASE_*_CHECKS / RELEASE_SELF_JOBS env
// vars. The separator is the NEWLINE, not a comma: check-run names are job
// display names and may legitimately contain commas — `property (PromQL +
// LogQL + TraceQL, rapid N=500)` is a branch-protection required context and
// splitting it on `,` yields two names that no lane will ever post, so the
// preflight would wait out its window and abort every release. release.yml
// therefore declares these as YAML block scalars, one name per line.
export function parseCheckList(raw) {
  return (raw ?? '')
    .split('\n')
    .map((s) => s.trim())
    .filter(Boolean);
}

export function allSuitesSettled(suites, ownSuiteId, workflowNames) {
  const pending = [];
  for (const s of suites ?? []) {
    if (ownSuiteId != null && s.id === ownSuiteId) continue;
    if ((s.latest_check_runs_count ?? 0) === 0) continue;
    if (s.status !== 'completed') {
      pending.push(workflowNames?.get(s.id) ?? s.app?.slug ?? s.app?.name ?? `suite#${s.id}`);
    }
  }
  return { done: pending.length === 0, pending };
}

// latestByName — re-runs leave several check-runs with the same name; the
// highest id is the check's current state, so a green re-run supersedes an
// earlier red. Shared by the wait phase and the one-shot evaluation so the two
// can never disagree about which run represents a lane.
function latestByName(checkRuns) {
  const latest = new Map();
  for (const cr of checkRuns ?? []) {
    const prev = latest.get(cr.name);
    if (!prev || cr.id > prev.id) latest.set(cr.name, cr);
  }
  return latest;
}

// requiredChecksPending — the wait-phase companion to allSuitesSettled, and the
// reason a lane that never starts cannot be mistaken for a lane that passed.
// Given the commit's check-runs and the EXPECTED required names, split them:
//   missing — no check-run with that name exists on the commit at all;
//   running — a run exists but is not yet `completed`.
// The wait loop keeps polling while either bucket is non-empty. Suite-level
// settledness alone is `done` the moment the suites that DID run finish, which
// is exactly the silence the required set exists to catch.
//
// Legacy combined statuses are deliberately NOT consulted here: the required
// set names check-runs (Actions jobs), and a required name that only ever
// appeared as a legacy status would sit in `missing` until the timeout — a loud
// wiring failure rather than a silent pass.
export function requiredChecksPending(checkRuns, required) {
  const latest = latestByName(checkRuns);
  const missing = [];
  const running = [];
  for (const name of required ?? []) {
    const cr = latest.get(name);
    if (!cr) {
      missing.push(name);
      continue;
    }
    if (cr.status !== 'completed') running.push(name);
  }
  return { missing, running };
}

// currentMinor — the highest released `<major>.<minor>` from the stable `v*`
// tag list, as a comparable [major, minor] tuple. null when no stable tag
// exists yet (pre-first-release — the support window can't be computed, so it
// is not enforced).
export function currentMinor(tags) {
  let best = null;
  for (const t of tags ?? []) {
    const m = APP_TAG_RE.exec(t);
    if (!m) continue;
    const v = [Number(m[1]), Number(m[2])];
    if (!best || v[0] > best[0] || (v[0] === best[0] && v[1] > best[1])) best = v;
  }
  return best;
}

// supportWindowProblem — given a maintenance branch and the released tag set,
// return a blocking-problem string iff the branch is end-of-life (its minor is
// SUPPORTED_MINOR_LINES or more behind the current minor on the same major), or
// null when the line is in-window (or the window can't be computed yet). Lines
// on an older major are always EOL once a newer major exists.
export function supportWindowProblem({ branch, tags, windowSize = SUPPORTED_MINOR_LINES }) {
  const m = MAINTENANCE_BRANCH_RE.exec(branch);
  if (!m) return null; // not a maintenance line — caller already guards this
  const line = [Number(m[1]), Number(m[2])];
  const cur = currentMinor(tags);
  if (!cur) return null; // no stable release yet — nothing to be behind of

  let behind;
  if (line[0] === cur[0]) {
    behind = cur[1] - line[1];
  } else if (line[0] < cur[0]) {
    // Older major: any newer major makes the line EOL. Treat as fully behind.
    behind = windowSize;
  } else {
    // Line ahead of the highest released minor (e.g. tip cut but not yet
    // tagged) — in-window by definition.
    return null;
  }

  if (behind >= windowSize) {
    return (
      `${branch} is end-of-life: minor ${line[0]}.${line[1]} is ${behind} minor(s) behind the current ` +
      `${cur[0]}.${cur[1]} (support window = latest ${windowSize} minor lines). An EOL line gets no ` +
      `further hotfixes — see the Release support window / EOL policy in docs/operations.md.`
    );
  }
  return null;
}

// retireLineForPublish — ACTIVE EOL. Given a just-published app version
// `X.Y.Z` and the released `v*` tag set, return the maintenance branch name of
// the line that just fell OUT of the support window (so the release job can
// delete it), or null when nothing should be retired.
//
// The math is the SAME window as `supportWindowProblem` — `SUPPORTED_MINOR_LINES`
// is the single source of truth. When a NEW minor opens (`Z == 0`), the window
// slides forward by one and the line `SUPPORTED_MINOR_LINES` behind the new
// minor drops off: publishing `X.Y.0` makes `X.(Y - SUPPORTED_MINOR_LINES).x`
// end-of-life. With a 3-line window, shipping 1.6.0 retires release/1.3.x.
//
// Conservative by construction — retires AT MOST one line, and only when:
//   - the version parses as a stable `X.Y.Z` (no prerelease suffix);
//   - it is a MINOR open (`Z == 0`) AND not a major bump (`Y > 0`). A patch
//     (`Z > 0`) leaves the window unchanged → null. A MAJOR bump (`Y == 0`)
//     is deliberately scoped OUT: retiring a whole prior major's lines on a
//     `X.0.0` is a bigger policy call, so this returns null and leaves those
//     lines to the maintainer (the passive `supportWindowProblem` gate still
//     refuses to PUBLISH on them). See docs/operations.md.
//   - the computed minor index is >= 0 (early minors like 1.0/1.1/1.2 under a
//     3-line window retire nothing — there is no line that far back yet);
//   - the just-published minor is actually the (new) HIGHEST released minor.
//     A stable BACKPORT cut after a newer minor — e.g. publishing 1.4.2 while
//     1.6.0 is already out — must NOT slide the window or retire anything; the
//     window is anchored to the current (highest) minor, not to whatever was
//     just published. Guarded by comparing against `currentMinor(tags)`.
//
// Returns the branch NAME only (`release/X.W.x`); the caller checks existence
// and performs the delete (idempotent when the branch is already gone).
export function retireLineForPublish({ version, tags, windowSize = SUPPORTED_MINOR_LINES }) {
  const m = APP_VERSION_RE.exec(version ?? '');
  if (!m) return null; // not a stable X.Y.Z (e.g. prerelease / chart / junk)
  const major = Number(m[1]);
  const minor = Number(m[2]);
  const patch = Number(m[3]);

  if (patch !== 0) return null; // patch release — window unchanged
  if (minor === 0) return null; // major bump (X.0.0) — out of scope, see note

  // Anchor to the highest released minor. If a NEWER minor than the one just
  // published already exists in the tag set, this publish is a backport and
  // must not move the window.
  const cur = currentMinor(tags);
  if (cur && (cur[0] > major || (cur[0] === major && cur[1] > minor))) return null;

  const retireMinor = minor - windowSize;
  if (retireMinor < 0) return null; // no line that far back exists yet

  return `release/${major}.${retireMinor}.x`;
}

// evaluate — given the branch tip sha, the pushed sha, the commit's raw
// check-runs + legacy statuses, the self-job names to exclude, the EXPECTED
// required names and the mode, return { problems, gated }. `problems` empty ==
// release may proceed.
export function evaluate({
  branchHead,
  pushedSha,
  checkRuns,
  statuses,
  selfJobs,
  branchLabel,
  tags,
  informational,
  required,
  mode = MODE_MAINTENANCE,
}) {
  const problems = [];
  const requiredNames = required ?? [];

  // A check is excluded from the gate if it is one of THIS release run's own
  // jobs (structural — gating on them would deadlock) or an explicitly de-gated
  // INFORMATIONAL lane. Informational lanes are matched by name PREFIX because
  // matrix children carry a "(shard-…)" suffix; e.g. "compose-smoke-shard-info"
  // matches "compose-smoke-shard-info (shard-crawl, …)". The required
  // `compose-smoke` aggregate already rolls up the gating shards, so the crawl
  // info shard is redundant — a flake there must not block a release. Everything
  // else that ran must be completed + green (a new lane gates by default).
  const isInformational = (name) => (informational ?? []).some((p) => p && name.startsWith(p));
  // Self-jobs match exactly, or as the parent of a reusable workflow's children
  // ("<job> / <child>"). Both are structurally downstream of this preflight.
  const isSelfJob = (name) =>
    selfJobs.has(name) || [...selfJobs].some((j) => name.startsWith(j + REUSABLE_JOB_SEPARATOR));

  if (mode === MODE_MAINTENANCE) {
    // Support-window / EOL gate — independent of the tip + green-check gates, so
    // it is evaluated even when the tip check below short-circuits. A push to an
    // end-of-life line is refused regardless of how green it is.
    const eol = supportWindowProblem({ branch: branchLabel, tags });
    if (eol) problems.push(eol);

    if (!branchHead) {
      problems.push(`could not resolve HEAD of ${branchLabel}`);
      return { problems, gated: 0 };
    }
    if (pushedSha !== branchHead) {
      problems.push(
        `pushed commit ${pushedSha.slice(0, 8)} is NOT the tip of ${branchLabel} ` +
          `(${branchHead.slice(0, 8)}) — a maintenance release may only be cut from the tip of its line`,
      );
      return { problems, gated: 0 };
    }
  }

  // Latest-per-name: the most recent run is the check's current state.
  const latest = latestByName(checkRuns);

  // --- the EXPECTED set ------------------------------------------------------
  // Everything below this point is observation-derived and therefore blind to a
  // lane that never ran. The required set is the only part of the gate that is
  // not, so it is checked first and it is checked against a set the caller
  // supplied, never against the API response.
  if (requiredNames.length === 0) {
    problems.push(
      'RELEASE_REQUIRED_CHECKS is empty — the gate would degrade to the observational deny-list, ' +
        'where a lane that never posted a check-run contributes zero problems and publishes silently',
    );
  }

  const present = new Set([...latest.keys(), ...(statuses ?? []).map((s) => s.context)]);
  for (const name of requiredNames) {
    if (isSelfJob(name)) {
      problems.push(
        `${name} is both REQUIRED and excluded by RELEASE_SELF_JOBS — a required lane cannot be one of ` +
          `this release run's own jobs; gating on it would deadlock. Fix the wiring, do not de-gate.`,
      );
      continue;
    }
    if (isInformational(name)) {
      problems.push(
        `${name} is both REQUIRED and de-gated by RELEASE_INFORMATIONAL_CHECKS (prefix match) — ` +
          `the informational prefix swallows a required lane, which is a hole, not a de-gate.`,
      );
      continue;
    }
    if (!present.has(name)) {
      problems.push(
        `${name}: REQUIRED lane posted no check-run on ${String(pushedSha).slice(0, 8)} — ` +
          `it never ran on this commit`,
      );
    }
  }

  let gated = 0;
  for (const cr of latest.values()) {
    if (isSelfJob(cr.name) || isInformational(cr.name)) continue;
    gated += 1;
    if (cr.status !== 'completed') {
      problems.push(`${cr.name}: still ${cr.status} (not completed)`);
    } else if (!GREEN_CONCLUSIONS.has(cr.conclusion)) {
      problems.push(`${cr.name}: ${cr.conclusion}`);
    }
  }

  // Legacy combined statuses (e.g. GitGuardian) — each gated context must succeed.
  for (const st of statuses ?? []) {
    if (isSelfJob(st.context) || isInformational(st.context)) continue;
    gated += 1;
    if (st.state !== 'success') {
      problems.push(`${st.context}: status ${st.state}`);
    }
  }

  return { problems, gated };
}

// ---------------------------------------------------------------------------
// self-test
// ---------------------------------------------------------------------------

function ghNotice(msg) {
  process.stdout.write(`::notice::${String(msg).replace(/\r?\n/g, '%0A')}\n`);
}

function ghError(msg) {
  process.stdout.write(`::error::${String(msg).replace(/\r?\n/g, '%0A')}\n`);
}

function selfTest() {
  const assert = (c, m) => {
    if (!c) throw new Error('self-test: ' + m);
  };
  const cr = (name, status, conclusion, id = 1) => ({ name, status, conclusion, id });
  const self = new Set(['gate', 'preflight', 'goreleaser', 'release-artifact-migration', 'publish', 'chart-release']);
  const label = 'release/1.4.x';
  // Every evaluate() world below supplies `required` explicitly: an EMPTY
  // required set is itself a blocking problem, so a world that omitted it would
  // pin the degradation guard rather than the detector it means to pin. These
  // sets are satisfied by construction in their world, so the only problems
  // reported are the ones each case is about.
  const requiredCheck = ['check'];

  // Branch-shape discrimination: maintenance lines match, main release PR
  // branches do NOT.
  assert(MAINTENANCE_BRANCH_RE.test('release/1.4.x'), 'release/1.4.x is a maintenance line');
  assert(MAINTENANCE_BRANCH_RE.test('release/1.3.x'), 'release/1.3.x is a maintenance line');
  assert(MAINTENANCE_BRANCH_RE.test('release/10.20.x'), 'multi-digit maintenance line');
  assert(!MAINTENANCE_BRANCH_RE.test('release/v1.5.0-chart-0.6.4'), 'main release PR branch is NOT a maintenance line');
  assert(!MAINTENANCE_BRANCH_RE.test('main'), 'main is not a maintenance line');
  assert(!MAINTENANCE_BRANCH_RE.test('release/1.4.0'), 'concrete patch is not a maintenance line');

  // --- wait phase: allSuitesSettled ----------------------------------------
  // A suite is settled when status === "completed"; the release run's own suite
  // (ownId) is ignored — it is necessarily in-progress while preflight polls.
  // runs defaults to 1 (a real suite that posts check-runs); pass 0 for an
  // empty suite an app registered but never populated (the GitGuardian case).
  //
  // Every suite an Actions workflow produces reports the SAME `app.slug`,
  // `github-actions` — so the fixtures below give them that slug rather than one
  // slug per workflow. Fake per-workflow slugs would let a pending-suite message
  // look informative in the tests while rendering as a row of identical
  // `github-actions` tokens in a real release log, which is what it did.
  const actionsSuite = (id, status, runs = 1) => ({
    id,
    status,
    app: { slug: 'github-actions' },
    latest_check_runs_count: runs,
  });
  const appSuite = (id, status, slug, runs = 1) => ({ id, status, app: { slug }, latest_check_runs_count: runs });
  const ownId = 99;
  const wfNames = new Map([
    [1, 'ci'],
    [2, 'e2e'],
    [3, 'mutation'],
    [ownId, 'release'],
  ]);

  // All non-own suites completed (own suite still in_progress) -> done.
  let s = allSuitesSettled(
    [actionsSuite(1, 'completed'), actionsSuite(2, 'completed'), actionsSuite(ownId, 'in_progress')],
    ownId,
    wfNames,
  );
  assert(s.done === true && s.pending.length === 0, 'all non-own suites completed -> done, no pending');

  // A queued non-own suite -> not done, named in pending; own suite ignored.
  s = allSuitesSettled(
    [actionsSuite(1, 'completed'), actionsSuite(2, 'queued'), actionsSuite(ownId, 'in_progress')],
    ownId,
    wfNames,
  );
  assert(s.done === false, 'a queued non-own suite -> not done');
  assert(s.pending.length === 1 && s.pending[0] === 'e2e', 'pending names the queued suite, ignores own');

  // An in_progress non-own suite -> not done.
  s = allSuitesSettled([actionsSuite(3, 'in_progress'), actionsSuite(ownId, 'in_progress')], ownId, wfNames);
  assert(s.done === false && s.pending[0] === 'mutation', 'an in_progress non-own suite -> not done, named');

  // The own suite being in_progress alone -> done (it is the only suite and is ignored).
  s = allSuitesSettled([actionsSuite(ownId, 'in_progress')], ownId, wfNames);
  assert(s.done === true && s.pending.length === 0, 'own suite alone is ignored -> done');

  // Multiple pending suites are named DISTINCTLY. Without the workflow-name map
  // they all collapse to `github-actions` and the message names nothing — the
  // regression this pins.
  s = allSuitesSettled([actionsSuite(1, 'queued'), actionsSuite(2, 'in_progress')], ownId, wfNames);
  assert(s.done === false && s.pending.length === 2, 'multiple pending suites -> all named');
  assert(new Set(s.pending).size === 2, 'pending suites are distinguishable, not a row of app slugs');
  assert(s.pending.includes('ci') && s.pending.includes('e2e'), 'pending names the workflows, not the app');

  // A suite no workflow run claims falls back to the app slug, which for a
  // third-party app IS the distinguishing name.
  s = allSuitesSettled([appSuite(7, 'in_progress', 'gitguardian')], ownId, wfNames);
  assert(s.pending[0] === 'gitguardian', 'unclaimed suite falls back to the app slug');

  // No name map at all (resolution failed) -> still gates correctly, just less
  // legibly. Diagnostics must never be able to change the decision.
  s = allSuitesSettled([actionsSuite(1, 'in_progress')], ownId, undefined);
  assert(s.done === false && s.pending[0] === 'github-actions', 'missing name map degrades the label, not the gate');

  // An empty suite (0 check-runs) that never completes is ignored -> done.
  // (GitGuardian's perpetually-queued empty suite hung the maintenance preflight.)
  s = allSuitesSettled([actionsSuite(1, 'completed', 1), appSuite(2, 'queued', 'gitguardian', 0)], ownId, wfNames);
  assert(s.done === true && s.pending.length === 0, 'empty (0-run) non-completing suite is ignored -> done');

  // No own suite id (e.g. off-runner) -> every non-completed suite counts.
  s = allSuitesSettled([actionsSuite(1, 'completed')], null, wfNames);
  assert(s.done === true, 'null ownId: a completed suite is still settled');
  s = allSuitesSettled([actionsSuite(1, 'in_progress')], null, wfNames);
  assert(s.done === false && s.pending[0] === 'ci', 'null ownId: an in_progress suite is pending');

  // All-green tip -> pass. 5 non-self check-runs + 1 status.
  let r = evaluate({
    branchHead: 'abc',
    pushedSha: 'abc',
    selfJobs: self,
    branchLabel: label,
    checkRuns: [
      cr('check', 'completed', 'success'),
      cr('lint', 'completed', 'success'),
      cr('forbid-skip', 'completed', 'success'),
      cr('compatibility/prometheus', 'completed', 'success'),
      cr('compose-smoke', 'completed', 'skipped'),
      cr('gate', 'in_progress', null), // self-job, excluded even mid-run
    ],
    statuses: [{ context: 'GitGuardian', state: 'success' }],
    required: requiredCheck,
  });
  assert(r.problems.length === 0, 'all-green tip should pass: ' + r.problems.join('; '));
  assert(r.gated === 6, `expected 6 gated (self-jobs excluded), got ${r.gated}`);

  // --- informational-lane exclusion (the headline fix) -----------------------
  const info = ['compose-smoke-shard-info', 'dashboard'];
  // A flaky de-gated INFORMATIONAL lane (the compose-smoke-shard-info crawl
  // shard, matched by name prefix despite its "(shard-…)" suffix; a dashboard
  // smoke) does NOT block the release. Its required roll-up (compose-smoke)
  // being green is enough.
  r = evaluate({
    branchHead: 'abc', pushedSha: 'abc', selfJobs: self, branchLabel: label, informational: info,
    checkRuns: [
      cr('check', 'completed', 'success'),
      cr('compose-smoke', 'completed', 'success'),
      cr('compose-smoke-shard-info (shard-crawl, crawl/crawl.spec.ts)', 'completed', 'failure'),
      cr('dashboard-shard (shard-smoke-b)', 'completed', 'failure'),
    ],
    statuses: [],
    required: requiredCheck,
  });
  assert(r.problems.length === 0, 'informational red must NOT block: ' + r.problems.join('; '));
  assert(r.gated === 2, `informational lanes excluded from the gate, got ${r.gated}`);

  // A RED gating (non-informational) check still blocks.
  r = evaluate({
    branchHead: 'abc', pushedSha: 'abc', selfJobs: self, branchLabel: label, informational: info,
    checkRuns: [cr('check', 'completed', 'success'), cr('compose-smoke', 'completed', 'failure')],
    statuses: [],
    required: requiredCheck,
  });
  assert(r.problems.some((p) => /compose-smoke: failure/.test(p)), 'a red gating check must still block');

  // A gating check that is NOT in the informational list gates by default
  // (the exclusion is opt-in; a new lane is conservative).
  r = evaluate({
    branchHead: 'abc', pushedSha: 'abc', selfJobs: self, branchLabel: label, informational: info,
    checkRuns: [cr('check', 'completed', 'success'), cr('some-new-lane', 'completed', 'failure')],
    statuses: [],
    required: requiredCheck,
  });
  assert(r.problems.some((p) => /some-new-lane: failure/.test(p)), 'an unlisted lane must gate by default');

  // Pushed commit is NOT the branch tip -> reject (stale re-drive).
  r = evaluate({
    branchHead: 'def', pushedSha: 'abc', selfJobs: self, branchLabel: label,
    checkRuns: [], statuses: [], required: requiredCheck,
  });
  assert(r.problems.length === 1 && /NOT the tip/.test(r.problems[0]), 'non-tip commit must fail');

  // Unresolved branch head -> reject.
  r = evaluate({
    branchHead: null, pushedSha: 'abc', selfJobs: self, branchLabel: label,
    checkRuns: [], statuses: [], required: requiredCheck,
  });
  assert(r.problems.length === 1 && /could not resolve HEAD/.test(r.problems[0]), 'unresolved head must fail');

  // A still-running NON-self check -> reject (no "running" allowed).
  r = evaluate({
    branchHead: 'abc',
    pushedSha: 'abc',
    selfJobs: self,
    branchLabel: label,
    checkRuns: [cr('check', 'in_progress', null)],
    statuses: [],
    required: requiredCheck,
  });
  assert(r.problems.some((p) => /check: still in_progress/.test(p)), 'running lane must block');

  // A failure / cancellation -> reject. No exclusions.
  r = evaluate({
    branchHead: 'abc',
    pushedSha: 'abc',
    selfJobs: self,
    branchLabel: label,
    checkRuns: [
      cr('compose-smoke', 'completed', 'failure'),
      cr('compatibility/loki', 'completed', 'cancelled'),
    ],
    statuses: [],
    required: ['compose-smoke'],
  });
  assert(r.problems.length === 2, 'two reds must BOTH block (no exclusion)');

  // Re-run: an earlier failure superseded by a later success -> green.
  r = evaluate({
    branchHead: 'abc',
    pushedSha: 'abc',
    selfJobs: self,
    branchLabel: label,
    checkRuns: [
      cr('check', 'completed', 'failure', 1),
      cr('check', 'completed', 'success', 2),
    ],
    statuses: [],
    required: requiredCheck,
  });
  assert(r.problems.length === 0, 'green re-run should supersede earlier fail');

  // Legacy status failure -> reject.
  r = evaluate({
    branchHead: 'abc',
    pushedSha: 'abc',
    selfJobs: self,
    branchLabel: label,
    checkRuns: [],
    statuses: [{ context: 'GitGuardian', state: 'failure' }],
    required: ['GitGuardian'],
  });
  assert(r.problems.length === 1 && /GitGuardian: status failure/.test(r.problems[0]), 'legacy status red must block');

  // --- support-window / EOL gate -------------------------------------------
  // Worked example from the policy: at v1.5.x current, 1.4.x and 1.3.x are
  // supported; 1.2.x and older are EOL.
  const releasedTags = ['v1.5.0', 'v1.4.0', 'v1.4.1', 'v1.3.0', 'v1.2.0', 'v1.0.0', 'v1.5.0-rc.1'];
  assert(currentMinor(releasedTags)[0] === 1 && currentMinor(releasedTags)[1] === 5, 'current minor is 1.5');
  assert(currentMinor(['v1.5.0-rc.1', 'v1.4.0'])[1] === 4, 'prerelease does not advance the current minor');
  assert(currentMinor([]) === null, 'no stable tag -> no current minor');

  const sw = (b, tags = releasedTags) => supportWindowProblem({ branch: b, tags });
  assert(sw('release/1.5.x') === null, '1.5.x (current) is in-window');
  assert(sw('release/1.4.x') === null, '1.4.x (current-1) is in-window');
  assert(sw('release/1.3.x') === null, '1.3.x (current-2) is in-window');
  assert(/end-of-life/.test(sw('release/1.2.x')), '1.2.x (current-3) is EOL');
  assert(/end-of-life/.test(sw('release/1.0.x')), '1.0.x (current-5) is EOL');
  assert(/end-of-life/.test(sw('release/0.9.x')), 'older major is EOL once a newer major exists');
  assert(sw('release/1.6.x') === null, 'a line ahead of the highest tagged minor is in-window');
  assert(sw('release/1.2.x', []) === null, 'no stable release yet -> window not enforced');

  // The EOL gate is independent of green checks: an all-green tip on an EOL line
  // is still refused.
  r = evaluate({
    branchHead: 'abc',
    pushedSha: 'abc',
    selfJobs: self,
    branchLabel: 'release/1.2.x',
    tags: releasedTags,
    checkRuns: [cr('check', 'completed', 'success')],
    statuses: [],
    required: requiredCheck,
  });
  assert(r.problems.some((p) => /end-of-life/.test(p)), 'all-green EOL line must still be blocked');

  // An in-window line with a green tip and tags present -> pass.
  r = evaluate({
    branchHead: 'abc',
    pushedSha: 'abc',
    selfJobs: self,
    branchLabel: 'release/1.4.x',
    tags: releasedTags,
    checkRuns: [cr('check', 'completed', 'success')],
    statuses: [],
    required: requiredCheck,
  });
  assert(r.problems.length === 0, 'in-window green line should pass: ' + r.problems.join('; '));

  // --- active EOL: retireLineForPublish ------------------------------------
  // The window math is shared with supportWindowProblem (SUPPORTED_MINOR_LINES),
  // so a minor open retires the line exactly SUPPORTED_MINOR_LINES behind it.
  const retire = (version, tags = []) => retireLineForPublish({ version, tags });

  // Worked example from the brief: publish 1.6.0 -> retire release/1.3.x.
  assert(retire('1.6.0', ['v1.6.0', 'v1.5.0', 'v1.4.0', 'v1.3.0']) === 'release/1.3.x', '1.6.0 retires release/1.3.x');
  // 1.5.0 -> retire release/1.2.x (matches the docs worked example).
  assert(retire('1.5.0', ['v1.5.0', 'v1.4.0', 'v1.3.0', 'v1.2.0']) === 'release/1.2.x', '1.5.0 retires release/1.2.x');
  // A patch release retires nothing — window unchanged.
  assert(retire('1.5.1', ['v1.5.1', 'v1.5.0', 'v1.4.0']) === null, '1.5.1 (patch) retires nothing');
  // No prior line that far back yet (early minors under a 3-line window).
  assert(retire('1.0.0', ['v1.0.0']) === null, '1.0.0 has no line 3 minors back -> noop');
  assert(retire('1.2.0', ['v1.2.0', 'v1.1.0', 'v1.0.0']) === null, '1.2.0: would-be -1.x line does not exist -> noop');
  // A MAJOR bump is out of scope — conservative, retires nothing here.
  assert(retire('2.0.0', ['v2.0.0', 'v1.6.0', 'v1.5.0']) === null, 'major bump 2.0.0 retires nothing (scoped out)');
  // A stable BACKPORT cut after a newer minor must NOT slide the window.
  assert(retire('1.4.0', ['v1.6.0', 'v1.5.0', 'v1.4.0']) === null, 'backport 1.4.0 behind current 1.6 retires nothing');
  // The retire-line is idempotent at the helper level: same inputs, same name
  // (existence/deletion is the caller's job).
  assert(retire('1.6.0', ['v1.6.0', 'v1.5.0']) === 'release/1.3.x', 'retire-line is deterministic regardless of branch presence');
  // Non-stable / non-version inputs are ignored.
  assert(retire('1.6.0-rc.1', ['v1.6.0-rc.1']) === null, 'prerelease publish retires nothing');
  assert(retire('chart-v0.6.3', []) === null, 'chart version is not an app version -> noop');
  assert(retire('', []) === null, 'empty version -> noop');
  // The retired line is, by construction, the line supportWindowProblem now
  // calls EOL — cross-check the two share the window.
  {
    const tags = ['v1.6.0', 'v1.5.0', 'v1.4.0', 'v1.3.0'];
    const line = retireLineForPublish({ version: '1.6.0', tags });
    assert(/end-of-life/.test(supportWindowProblem({ branch: line, tags })), 'the retired line is EOL per supportWindowProblem');
    assert(supportWindowProblem({ branch: 'release/1.4.x', tags }) === null, 'the line kept (1.4.x) is still in-window');
  }

  // --- the EXPECTED set: absence must FAIL ---------------------------------
  // The whole point of `required`. A lane that never ran contributes nothing to
  // `checkRuns`, so every observation-derived detector above is blind to it.
  const fullRequired = ['check', 'compose-smoke', 'migration-e2e'];
  const greenWorld = (names) => ({
    branchHead: 'abcdef1234',
    pushedSha: 'abcdef1234',
    selfJobs: self,
    branchLabel: label,
    checkRuns: names.map((n) => cr(n, 'completed', 'success')),
    statuses: [],
  });

  // Positive control: every required lane ran green -> no problems.
  r = evaluate({ ...greenWorld(fullRequired), required: fullRequired });
  assert(r.problems.length === 0, 'all required lanes green -> pass: ' + r.problems.join('; '));

  // migration-e2e never posted a run: green everywhere else, still BLOCKED.
  r = evaluate({ ...greenWorld(['check', 'compose-smoke']), required: fullRequired });
  assert(
    r.problems.length === 1 && /migration-e2e: REQUIRED lane posted no check-run/.test(r.problems[0]),
    'an absent REQUIRED lane must block: ' + r.problems.join('; '),
  );
  // Negative control: the SAME world with migration-e2e out of `required` is
  // green — so the detector fires on absence, not unconditionally.
  r = evaluate({ ...greenWorld(['check', 'compose-smoke']), required: ['check', 'compose-smoke'] });
  assert(r.problems.length === 0, 'the absence detector must fire on absence only: ' + r.problems.join('; '));

  // Empty required set -> the degradation guard, not a silent pass.
  r = evaluate({ ...greenWorld(fullRequired), required: [] });
  assert(
    r.problems.some((p) => /RELEASE_REQUIRED_CHECKS is empty/.test(p)),
    'an empty required set must be a blocking problem',
  );

  // An informational PREFIX that swallows a required name is a wiring error,
  // reported as such rather than as mere absence.
  r = evaluate({ ...greenWorld(fullRequired), required: fullRequired, informational: ['migration'] });
  assert(
    r.problems.length === 1 && /migration-e2e is both REQUIRED and de-gated/.test(r.problems[0]),
    'a required lane swallowed by an informational prefix must be a wiring problem: ' + r.problems.join('; '),
  );

  // A required name that is also a self-job is the other wiring error.
  r = evaluate({ ...greenWorld(fullRequired), required: [...fullRequired, 'goreleaser'] });
  assert(
    r.problems.some((p) => /goreleaser is both REQUIRED and excluded by RELEASE_SELF_JOBS/.test(p)),
    'a required lane that is a self-job must be a wiring problem',
  );

  // A reusable workflow's children ("<self-job> / <child>") are self-jobs too.
  r = evaluate({
    branchHead: 'abc', pushedSha: 'abc', selfJobs: self, branchLabel: label,
    checkRuns: [cr('release-artifact-migration / migration-tier1', 'in_progress', null)],
    statuses: [], required: [],
  });
  assert(r.gated === 0, `reusable-workflow children of a self-job must not gate, got ${r.gated}`);
  assert(
    !r.problems.some((p) => /release-artifact-migration/.test(p)),
    "a self-job's reusable children must raise no problem: " + r.problems.join('; '),
  );

  // --- wait phase: requiredChecksPending ------------------------------------
  {
    const runs = [cr('check', 'completed', 'success'), cr('compose-smoke', 'in_progress', null)];
    const pending = requiredChecksPending(runs, fullRequired);
    assert(pending.missing.length === 1 && pending.missing[0] === 'migration-e2e', 'a never-posted lane is MISSING');
    assert(pending.running.length === 1 && pending.running[0] === 'compose-smoke', 'an incomplete lane is RUNNING');
    const settled = requiredChecksPending(greenWorld(fullRequired).checkRuns, fullRequired);
    assert(settled.missing.length === 0 && settled.running.length === 0, 'all required completed -> nothing pending');
  }

  // --- mode split -----------------------------------------------------------
  // The branch-tip and EOL rules are MAINTENANCE-only; the required set and the
  // green gate are not.
  r = evaluate({
    branchHead: 'def', pushedSha: 'abc', selfJobs: self, branchLabel: 'main',
    checkRuns: [cr('check', 'completed', 'success')], statuses: [],
    required: requiredCheck, mode: MODE_MAINLINE,
  });
  assert(r.problems.length === 0, 'mainline mode does not enforce the branch tip: ' + r.problems.join('; '));
  r = evaluate({
    branchHead: 'def', pushedSha: 'abc', selfJobs: self, branchLabel: label,
    checkRuns: [cr('check', 'completed', 'success')], statuses: [],
    required: requiredCheck, mode: MODE_MAINTENANCE,
  });
  assert(r.problems.some((p) => /NOT the tip/.test(p)), 'maintenance mode DOES enforce the branch tip');
  r = evaluate({
    branchHead: 'abc', pushedSha: 'abc', selfJobs: self, branchLabel: 'release/1.2.x', tags: releasedTags,
    checkRuns: [cr('check', 'completed', 'success')], statuses: [],
    required: requiredCheck, mode: MODE_MAINLINE,
  });
  assert(!r.problems.some((p) => /end-of-life/.test(p)), 'mainline mode does not enforce the EOL window');

  ghNotice('release-preflight --self-test: all assertions passed');
}

// ---------------------------------------------------------------------------
// driver
// ---------------------------------------------------------------------------

async function main() {
  const repo = process.env.GITHUB_REPOSITORY;
  const pushedSha = process.env.GITHUB_SHA;
  const branch = process.env.GITHUB_REF_NAME ?? '';
  const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';
  const token = process.env.GITHUB_TOKEN;
  const runId = process.env.GITHUB_RUN_ID;
  const selfJobs = new Set(parseCheckList(process.env.RELEASE_SELF_JOBS));
  // Name-prefix list of explicitly de-gated INFORMATIONAL lanes (e.g. the
  // compose-smoke-shard-info crawl shard, the dashboard smoke). A flake in one
  // of these must not block a maintenance release; everything else still gates.
  const informational = parseCheckList(process.env.RELEASE_INFORMATIONAL_CHECKS);
  // The EXPECTED set — exact check-run names that MUST have posted a run on the
  // commit. Parsed the same way as the informational list, but consumed as EXACT
  // names, not prefixes: a required lane is a specific job, not a family.
  const required = parseCheckList(process.env.RELEASE_REQUIRED_CHECKS);

  // Mode is derived from the branch, not passed in: a `release/*.x` push is the
  // maintenance path (tip + support-window rules apply), anything else is the
  // mainline path. There is no "not applicable" third state — the preflight
  // always gates.
  const mode = MAINTENANCE_BRANCH_RE.test(branch) ? MODE_MAINTENANCE : MODE_MAINLINE;
  const label = mode === MODE_MAINTENANCE ? 'maintenance preflight' : 'mainline preflight';

  if (!repo || !pushedSha || !token) {
    ghError('GITHUB_REPOSITORY, GITHUB_SHA, and GITHUB_TOKEN are all required');
    process.exit(1);
  }

  const headers = {
    Authorization: `Bearer ${token}`,
    Accept: 'application/vnd.github+json',
    'X-GitHub-Api-Version': '2022-11-28',
  };

  async function getJSON(url) {
    const res = await fetch(url, { headers });
    if (!res.ok) {
      throw new Error(`GET ${url} -> ${res.status} ${res.statusText}`);
    }
    return res.json();
  }

  // The pushed commit must be the current tip of the maintenance branch.
  async function branchHead() {
    const b = await getJSON(`${apiBase}/repos/${repo}/branches/${branch}`);
    return b.commit?.sha ?? null;
  }

  async function allCheckRuns() {
    const out = [];
    let page = 1;
    for (;;) {
      const data = await getJSON(
        `${apiBase}/repos/${repo}/commits/${pushedSha}/check-runs?per_page=100&page=${page}`,
      );
      const runs = data.check_runs ?? [];
      out.push(...runs);
      if (runs.length < 100) break;
      page += 1;
    }
    return out;
  }

  async function combinedStatus() {
    return getJSON(`${apiBase}/repos/${repo}/commits/${pushedSha}/status?per_page=100`);
  }

  // Every tag name — the support-window gate derives the current minor from the
  // stable `v<major>.<minor>.<patch>` subset. Listed via the API (not git) so
  // the preflight job needs no fetch-depth.
  async function allTags() {
    const out = [];
    let page = 1;
    for (;;) {
      const data = await getJSON(`${apiBase}/repos/${repo}/tags?per_page=100&page=${page}`);
      const names = (data ?? []).map((t) => t.name);
      out.push(...names);
      if (names.length < 100) break;
      page += 1;
    }
    return out;
  }

  // All check-suites on the pushed commit. The wait phase polls this until every
  // suite EXCEPT this release run's own is `completed`.
  async function allCheckSuites() {
    const out = [];
    let page = 1;
    for (;;) {
      const data = await getJSON(
        `${apiBase}/repos/${repo}/commits/${pushedSha}/check-suites?per_page=100&page=${page}`,
      );
      const suites = data.check_suites ?? [];
      out.push(...suites);
      if (suites.length < 100) break;
      page += 1;
    }
    return out;
  }

  // suite id -> workflow run name, for the wait loop's progress message. Every
  // Actions check-suite carries the same `app.slug`, so the run name is the only
  // thing that tells "e2e" apart from "mutation". Diagnostics only: a failure to
  // resolve a name degrades the message to the app slug, never the gate.
  async function workflowNamesBySuite() {
    const names = new Map();
    let page = 1;
    for (;;) {
      const data = await getJSON(
        `${apiBase}/repos/${repo}/actions/runs?head_sha=${pushedSha}&per_page=100&page=${page}`,
      );
      const runs = data.workflow_runs ?? [];
      for (const r of runs) {
        if (r.check_suite_id != null && r.name) names.set(r.check_suite_id, r.name);
      }
      if (runs.length < 100) break;
      page += 1;
    }
    return names;
  }

  // Resolve THIS release run's own check-suite id via the run -> check_suite_id
  // link, so the wait loop can skip it (it is necessarily in-progress while we
  // poll — waiting on it deadlocks). null when GITHUB_RUN_ID is absent (e.g. a
  // local run) — the wait then treats no suite as "own", which is still correct
  // because the release suite isn't a CI lane the maintainer is waiting on.
  async function ownSuiteId() {
    if (!runId) return null;
    const run = await getJSON(`${apiBase}/repos/${repo}/actions/runs/${runId}`);
    return run.check_suite_id ?? null;
  }

  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  // --- WAIT PHASE: poll until the CI matrix on the commit settles -------------
  // release.yml fires on the same push as CI, so a one-shot snapshot here would
  // see everything queued/in_progress and false-abort. Poll until BOTH every
  // non-own check-SUITE is completed AND every REQUIRED lane has posted a
  // completed check-run, bounded by maxWaitMs. The second condition is what
  // makes a lane that never started time out loudly: suite settledness alone is
  // satisfied by the suites that DID run, so silence would read as "settled".
  // On timeout, ABORT naming missing and running lanes separately (fail-safe —
  // never publish on an unknown state).
  const ownId = await ownSuiteId();
  const waitDeadline = Date.now() + maxWaitMs;
  let lastProgress = null;
  let pollsSinceNotice = 0;
  for (;;) {
    const suites = await allCheckSuites();
    const { done, pending } = allSuitesSettled(suites, ownId, await workflowNamesBySuite());
    const { missing, running } = requiredChecksPending(await allCheckRuns(), required);
    if (done && missing.length === 0 && running.length === 0) {
      ghNotice(
        `${label}: CI matrix on ${pushedSha.slice(0, 8)} has settled ` +
          `(${suites.length} check-suite(s)) and all ${required.length} required lane(s) have completed; ` +
          `evaluating green gate.`,
      );
      break;
    }
    if (Date.now() >= waitDeadline) {
      if (missing.length > 0) {
        ghError(
          `${label}: REQUIRED lane(s) never posted a check-run on ${branch}@${pushedSha.slice(0, 8)}: ` +
            `${missing.join(', ')}. Either the workflow does not trigger on this branch, or it failed to ` +
            `start. A lane that did not run has not passed — refusing to publish.`,
        );
      }
      ghError(
        `${label}: timed out after ${releaseWaitMinutes} min waiting for CI on ` +
          `${branch}@${pushedSha.slice(0, 8)} to finish — required still running: ` +
          `${running.join(', ') || '(none)'}; suites still running: ${pending.join(', ') || '(none)'}. ` +
          `Refusing to publish on an incomplete state; re-run once CI settles.`,
      );
      process.exit(1);
    }
    // Log on state change, plus a heartbeat so a long quiet stretch still shows
    // the job is alive. Emitting every poll unconditionally buried the one line
    // that mattered under dozens of byte-identical repeats — a 33-min wait on a
    // single slow lane produced 66 notices, 31 of them the same sentence.
    const progress =
      `${pending.length} check-suite(s) still running (${pending.join(', ') || 'none'}); ` +
      `required missing: ${missing.join(', ') || 'none'}; required running: ${running.join(', ') || 'none'}`;
    if (progress !== lastProgress || pollsSinceNotice >= quietHeartbeatPolls) {
      ghNotice(`${label}: ${progress}; re-polling in ${Math.round(pollIntervalMs / 1000)}s.`);
      lastProgress = progress;
      pollsSinceNotice = 0;
    } else {
      pollsSinceNotice += 1;
    }
    await sleep(pollIntervalMs);
  }

  const head = await branchHead();
  const checkRuns = await allCheckRuns();
  const combined = await combinedStatus();
  const statuses = combined.statuses ?? [];
  const tags = await allTags();

  const { problems, gated } = evaluate({
    branchHead: head,
    pushedSha,
    checkRuns,
    statuses,
    selfJobs,
    branchLabel: branch,
    tags,
    informational,
    required,
    mode,
  });

  if (problems.length > 0) {
    for (const p of problems) ghError(`${label}: ${p}`);
    ghError(
      `release of ${branch}@${pushedSha.slice(0, 8)} BLOCKED: ${problems.length} problem(s) across ` +
        `${gated} gated check(s) and ${required.length} required lane(s). ` +
        `Fix the red/running/absent checks and re-push.`,
    );
    process.exit(1);
  }

  ghNotice(
    `${label} OK: ${branch}@${pushedSha.slice(0, 8)} has all ${required.length} required lane(s) present and ` +
      `all ${gated} gated check(s) settled green.`,
  );
  process.exit(0);
}

// ---------------------------------------------------------------------------
// active-EOL driver — `eol-retire-line`
// ---------------------------------------------------------------------------
//
// Runs POST-publish, after a NEW app version actually shipped. Computes the
// line that just fell out of the support window via `retireLineForPublish`
// (shared window math) and, if its `release/X.W.x` branch EXISTS, DELETES it.
//
// FAIL-OPEN by contract: the release already published before this runs, so a
// deletion failure must NEVER fail the workflow. Every error path logs loudly
// (`::error::` / `::notice::`) and still exits 0. The only non-zero exit is a
// gross wiring error (missing repo/token) BEFORE any publish-affecting work —
// but the workflow gates this step on a successful publish, so even that is
// observability, not a release-failing condition (the step is `if:`-guarded and
// could be marked continue-on-error too; we keep exit 0 to be safe regardless).
//
// Mechanism: deletes the ref via the Git refs API with the token in
// GITHUB_TOKEN. The workflow passes RELEASE_PAT (fine-grained, contents:write)
// when present, else the default github.token — both can delete an UNPROTECTED
// `release/*.x` branch (verified: no ruleset / classic protection covers
// `release/*`, only `main`). If a future ruleset protects `release/*`, wire a
// bypass for the PAT identity; the fail-open contract means a 403 just logs.
//
// Env contract:
//   GITHUB_TOKEN       token with contents:write (RELEASE_PAT or github.token).
//   GITHUB_REPOSITORY  "owner/name".
//   RELEASE_APP_VERSION the just-published app version, `X.Y.Z` (no `v`).
//   GITHUB_API_URL     API base (default https://api.github.com).
async function retireLine() {
  const repo = process.env.GITHUB_REPOSITORY;
  const token = process.env.GITHUB_TOKEN;
  const version = process.env.RELEASE_APP_VERSION ?? '';
  const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';

  if (!repo || !token) {
    ghError('eol-retire-line: GITHUB_REPOSITORY and GITHUB_TOKEN are required');
    process.exit(1);
  }

  const headers = {
    Authorization: `Bearer ${token}`,
    Accept: 'application/vnd.github+json',
    'X-GitHub-Api-Version': '2022-11-28',
  };

  // All tags — the window anchors to the highest released stable minor. Fetched
  // via the API so the step needs no fetch-depth. Fail-open: if we cannot list
  // tags we cannot safely compute the window, so we retire nothing and return.
  async function allTags() {
    const out = [];
    let page = 1;
    for (;;) {
      const res = await fetch(`${apiBase}/repos/${repo}/tags?per_page=100&page=${page}`, { headers });
      if (!res.ok) throw new Error(`GET tags -> ${res.status} ${res.statusText}`);
      const data = await res.json();
      const names = (data ?? []).map((t) => t.name);
      out.push(...names);
      if (names.length < 100) break;
      page += 1;
    }
    return out;
  }

  let tags;
  try {
    tags = await allTags();
  } catch (e) {
    ghError(`eol-retire-line: could not list tags (${e.message}) — retiring nothing this run (fail-open)`);
    process.exit(0);
  }

  const line = retireLineForPublish({ version, tags });
  if (!line) {
    ghNotice(
      `eol-retire-line: publishing ${version || '(none)'} retires no line ` +
        `(patch / major / early-minor / backport / non-stable — window unchanged).`,
    );
    process.exit(0);
  }

  const branch = line; // `release/X.W.x`
  const ref = `heads/${branch}`;

  // Does the branch exist? A 404 means it was already retired — idempotent noop.
  let exists;
  try {
    const res = await fetch(`${apiBase}/repos/${repo}/git/ref/${ref}`, { headers });
    if (res.status === 404) exists = false;
    else if (res.ok) exists = true;
    else throw new Error(`GET ref ${ref} -> ${res.status} ${res.statusText}`);
  } catch (e) {
    ghError(`eol-retire-line: could not check ${branch} existence (${e.message}) — skipping delete (fail-open)`);
    process.exit(0);
  }

  if (!exists) {
    ghNotice(`eol-retire-line: ${branch} is out-of-window but already absent — nothing to delete (idempotent).`);
    process.exit(0);
  }

  // SAFETY BACKSTOP: never delete a line the support-window math considers
  // in-window. `retireLineForPublish` already guarantees this, but cross-check
  // against `supportWindowProblem` before the destructive call — a line that is
  // NOT EOL must never be deleted, full stop.
  if (!supportWindowProblem({ branch, tags })) {
    ghError(
      `eol-retire-line: refusing to delete ${branch} — supportWindowProblem says it is IN-WINDOW. ` +
        `This is a logic contradiction; retiring nothing (fail-open).`,
    );
    process.exit(0);
  }

  // Delete the ref. Fail-open on any error (403 protected, 422, network …).
  try {
    const res = await fetch(`${apiBase}/repos/${repo}/git/refs/${ref}`, { method: 'DELETE', headers });
    if (res.status === 204) {
      ghNotice(
        `eol-retire-line: deleted ${branch} — it fell out of the latest-${SUPPORTED_MINOR_LINES} support window ` +
          `when ${version} shipped (active EOL). Tags / Releases for that line are retained.`,
      );
      process.exit(0);
    }
    const body = await res.text().catch(() => '');
    ghError(
      `eol-retire-line: DELETE ${branch} -> ${res.status} ${res.statusText} ${body} — ` +
        `branch NOT deleted. Release already published; delete it manually: ` +
        `git push origin --delete ${branch}. (fail-open)`,
    );
    process.exit(0);
  } catch (e) {
    ghError(
      `eol-retire-line: DELETE ${branch} failed (${e.message}) — branch NOT deleted. ` +
        `Release already published; delete it manually: git push origin --delete ${branch}. (fail-open)`,
    );
    process.exit(0);
  }
}

// ---------------------------------------------------------------------------
// dispatcher
// ---------------------------------------------------------------------------

// Only dispatch when run as a script — importing the pure core for
// release-preflight.test.mjs must not fire the network driver or exit the test
// runner. (Same idiom as migration-e2e.mjs / compose-smoke-matrix.mjs.)
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;

if (invokedDirectly) {
  if (process.argv.includes('--self-test')) {
    selfTest();
    process.exit(0);
  }

  const cmd = process.argv[2];
  if (cmd === 'eol-retire-line') {
    retireLine().catch((e) => {
      // Even an unexpected throw is fail-open: the release already shipped.
      ghError(`eol-retire-line: unexpected failure (${e.message}) — retiring nothing (fail-open)`);
      process.exit(0);
    });
  } else {
    main().catch((e) => {
      ghError(`release-preflight failed: ${e.message}`);
      process.exit(1);
    });
  }
}
