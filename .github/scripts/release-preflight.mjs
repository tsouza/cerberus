// release-preflight.mjs — the green-check guard for the MAINTENANCE-line
// release path (a push to a `release/<major>.<minor>.x` hotfix branch).
//
// Why this exists ONLY for the maintenance path:
//   A push to `main` is implicitly green — branch protection refuses to merge a
//   PR whose required checks are red or whose tree is behind main, so the merge
//   commit is releasable by construction. The publish-on-merge release.yml
//   trusts that and runs no preflight on the main path.
//
//   A push to a `release/*.x` maintenance branch has NO PR gate: a maintainer
//   cherry-picks a hotfix straight onto the branch and pushes. Publishing that
//   unguarded would risk shipping a RED commit. So before the publish jobs run,
//   this preflight re-reads the pushed commit's check-runs + legacy statuses and
//   refuses to release unless EVERY required check is settled GREEN.
//
// The rule, with no softening:
//   1. The pushed commit MUST be the current tip of the `release/*.x` branch
//      it was pushed to. You release the tip of a maintenance line, never an
//      older/side commit. (For a branch push GITHUB_SHA is normally already the
//      tip; the check defends against a stale re-drive racing a newer push.)
//   2. EVERY check-run + legacy status on the commit must be COMPLETED (nothing
//      still running / queued) AND green (success / skipped / neutral). One
//      running check, one failure, one cancelled/timed-out lane -> release
//      ABORTS. No flaky-lane exclusions, no "informational" passes.
//   3. The maintenance line must be INSIDE the support window — the latest
//      SUPPORTED_MINOR_LINES minor lines (current + the two prior). A push to a
//      line that is end-of-life (3+ minors behind the highest released minor)
//      is REFUSED: an EOL line gets no further hotfixes. See the "Release
//      support window / EOL policy" subsection of docs/operations.md.
//
// The ONLY exclusion is THIS release run's own jobs (gate / preflight /
// goreleaser / chart-release): they are necessarily in-progress while the
// preflight runs, so gating on them would deadlock. They are identified by name
// via RELEASE_SELF_JOBS — structural, not a flakiness heuristic.
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
//   GITHUB_SHA         the pushed (maintenance) commit SHA.
//   GITHUB_REF_NAME    the pushed branch name, e.g. `release/1.4.x`. Must match
//                      the `release/<major>.<minor>.x` shape — the preflight is
//                      ONLY meaningful on the maintenance path and refuses to
//                      run otherwise (a wiring guard, not a silent pass).
//   GITHUB_API_URL     API base (default https://api.github.com).
//   RELEASE_SELF_JOBS  comma-separated check-run names belonging to THIS release
//                      workflow, excluded from the gate.
//
// `evaluate(...)` takes the branch HEAD sha, the pushed sha, the raw check-runs,
// the legacy statuses, the self-job name set, and the released-version tags, and
// returns a list of blocking problems (empty == release may proceed) plus the
// count of gated checks. No network, no exclusions beyond self-jobs — exported
// so the self-test pins the exact pass/fail boundary. The support-window check
// is a pure helper (`supportWindowProblem`) folded into the same problems list.
//
// argv `--self-test` runs the in-process assertion suite and exits.
//
// Imports only node: builtins (Node ships `fetch`). Run with
// `node .github/scripts/release-preflight.mjs`.

import process from 'node:process';

// A maintenance line is `release/<major>.<minor>.x` — `release/1.4.x`,
// `release/1.3.x`. It explicitly does NOT match a main release PR branch like
// `release/v1.5.0-chart-0.6.4` (those don't end in `.x`).
export const MAINTENANCE_BRANCH_RE = /^release\/(\d+)\.(\d+)\.x$/;

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

// ---------------------------------------------------------------------------
// pure core (exported for the self-test — no network, no process.exit)
// ---------------------------------------------------------------------------

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
// check-runs + legacy statuses, and the set of self-job names to exclude,
// return { problems, gated }. `problems` empty == release may proceed.
export function evaluate({ branchHead, pushedSha, checkRuns, statuses, selfJobs, branchLabel, tags }) {
  const problems = [];

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

  // Latest-per-name: the most recent run is the check's current state.
  const latest = new Map();
  for (const cr of checkRuns) {
    const prev = latest.get(cr.name);
    if (!prev || cr.id > prev.id) latest.set(cr.name, cr);
  }

  let gated = 0;
  for (const cr of latest.values()) {
    if (selfJobs.has(cr.name)) continue; // this release run's own jobs (structural)
    gated += 1;
    if (cr.status !== 'completed') {
      problems.push(`${cr.name}: still ${cr.status} (not completed)`);
    } else if (!GREEN_CONCLUSIONS.has(cr.conclusion)) {
      problems.push(`${cr.name}: ${cr.conclusion}`);
    }
  }

  // Legacy combined statuses (e.g. GitGuardian) — each context must be success.
  for (const st of statuses ?? []) {
    if (selfJobs.has(st.context)) continue;
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
  const self = new Set(['gate', 'preflight', 'goreleaser', 'chart-release']);
  const label = 'release/1.4.x';

  // Branch-shape discrimination: maintenance lines match, main release PR
  // branches do NOT.
  assert(MAINTENANCE_BRANCH_RE.test('release/1.4.x'), 'release/1.4.x is a maintenance line');
  assert(MAINTENANCE_BRANCH_RE.test('release/1.3.x'), 'release/1.3.x is a maintenance line');
  assert(MAINTENANCE_BRANCH_RE.test('release/10.20.x'), 'multi-digit maintenance line');
  assert(!MAINTENANCE_BRANCH_RE.test('release/v1.5.0-chart-0.6.4'), 'main release PR branch is NOT a maintenance line');
  assert(!MAINTENANCE_BRANCH_RE.test('main'), 'main is not a maintenance line');
  assert(!MAINTENANCE_BRANCH_RE.test('release/1.4.0'), 'concrete patch is not a maintenance line');

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
  });
  assert(r.problems.length === 0, 'all-green tip should pass: ' + r.problems.join('; '));
  assert(r.gated === 6, `expected 6 gated (self-jobs excluded), got ${r.gated}`);

  // Pushed commit is NOT the branch tip -> reject (stale re-drive).
  r = evaluate({ branchHead: 'def', pushedSha: 'abc', selfJobs: self, branchLabel: label, checkRuns: [], statuses: [] });
  assert(r.problems.length === 1 && /NOT the tip/.test(r.problems[0]), 'non-tip commit must fail');

  // Unresolved branch head -> reject.
  r = evaluate({ branchHead: null, pushedSha: 'abc', selfJobs: self, branchLabel: label, checkRuns: [], statuses: [] });
  assert(r.problems.length === 1 && /could not resolve HEAD/.test(r.problems[0]), 'unresolved head must fail');

  // A still-running NON-self check -> reject (no "running" allowed).
  r = evaluate({
    branchHead: 'abc',
    pushedSha: 'abc',
    selfJobs: self,
    branchLabel: label,
    checkRuns: [cr('check', 'in_progress', null)],
    statuses: [],
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
  const selfJobs = new Set(
    (process.env.RELEASE_SELF_JOBS ?? '')
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean),
  );

  if (!MAINTENANCE_BRANCH_RE.test(branch)) {
    ghError(
      `release-preflight is the MAINTENANCE-path guard and must only run on a release/<major>.<minor>.x branch; ` +
        `got GITHUB_REF_NAME="${branch}". This is a workflow-wiring error.`,
    );
    process.exit(1);
  }
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
  });

  if (problems.length > 0) {
    for (const p of problems) ghError(`maintenance preflight: ${p}`);
    ghError(
      `maintenance release of ${branch}@${pushedSha.slice(0, 8)} BLOCKED: ` +
        `${problems.length} problem(s) across ${gated} gated check(s). Fix the red/running checks and re-push.`,
    );
    process.exit(1);
  }

  ghNotice(
    `maintenance preflight OK: ${branch}@${pushedSha.slice(0, 8)} is the branch tip and all ${gated} gated check(s) are settled green.`,
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
