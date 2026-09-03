// coverage-verdict.mjs — say, in the required `coverage` context's OWN output,
// whether this run measured anything, tsouza/cerberus#2991.
//
// The hole this closes
// --------------------
// `coverage` is a required context. On an ordinary pull request every job that
// produces a profile is skipped (`coverage-plan`'s RUN_HEAVY decision — a
// deliberate latency choice, see coverage-run-heavy.mjs), the aggregator runs
// anyway so the required context cannot go missing, and it reported
// **success**. A reader of that green tick had no way to tell "the floors were
// compared and every package cleared them" from "nothing was measured at all",
// and for the four days this issue documents the second was the truth on every
// PR while `main`'s own heavy run was red.
//
// Skipping the lanes is legitimate. Reporting a measurement that did not happen
// is not.
//
// Why the PR does not just measure something cheap instead
// --------------------------------------------------------
// The obvious alternative is to run the default-tag lane on pull requests and
// compare THAT. It is not rejected on cost — `coverage-default` finishes in
// about three minutes, against `coverage-chdb`'s 35-50 — it is rejected on
// soundness. The floors in test/coverage-floor/ are measured from the MERGED
// `default+chdb` profile, and a chdb-tagged run compiles and reaches code the
// default-tag run cannot, so a default-only profile is not merely lower, it is
// not comparable: every package whose coverage comes from chdb-tagged tests
// would report a drop that is not real. coverage-summary.mjs already refuses
// that comparison for exactly this reason (resolveLanes against FULL_LANES).
// A sound PR-time measurement would need a SECOND, default-only ledger with its
// own ratchet, doubling the floor surface — so the honest answer on a pull
// request is to say plainly that nothing was measured. So the aggregator now states its verdict explicitly, as the first
// thing in its step summary and as a log annotation on the check itself:
//
//   MEASURED     — the merged default+chdb profile was compared to the ledger
//                  and every package cleared its floor.
//   NOT MEASURED — no profile was produced for this commit. The green tick
//                  covers package-floor ENROLLMENT and the lane roll-up, and
//                  nothing else.
//   (red)        — a floor was missed, or the run's own claim about itself and
//                  the evidence on disk disagree.
//
// Why the verdict is derived from the profile, not from RUN_HEAVY
// --------------------------------------------------------------
// RUN_HEAVY is a CLAIM made by an earlier job. A verdict that trusted it would
// print "MEASURED" for any run that merely asserted it meant to measure — the
// same shape tsouza/cerberus#2992 removed from the floor-UPDATE path, where an
// environment variable used to stand in for evidence that a profile carried
// both lanes. So this reads the merged profile and the digest-bound lane record
// coverage-summary.mjs writes beside it (`<profile>.lanes.json`: `lanes` plus
// the SHA-256 of the profile's own bytes), and reports MEASURED only when that
// record proves THOSE bytes carry the full `default+chdb` lane set.
//
// The two are then cross-checked, and any disagreement is a hard failure rather
// than a downgrade: a heavy run with no provable profile, or a non-heavy run
// with one, means the job-level gates and the evidence have drifted apart, and
// the honest answer to "which of the two is lying" is a red check.
//
// Trunk measurement staleness
// ---------------------------
// The other half of #2991 is that `main` itself went unmeasured: coverage.yml's
// concurrency group used to cancel the in-progress run on every new push, and
// with a ~52-minute run against a median 38-minute push cadence most runs died
// mid-measurement (71 of the last 120 pushes, against 6 successes). Cancelling
// is no longer how that group behaves, but coalescing still means SOME commits
// go unmeasured, and a lane that is simply broken produces the same silence.
//
// So every verdict — on a PR as much as on main — also reports when `main` was
// last MEASURED and how many commits have landed since, and warns past
// MAX_UNMEASURED_TRUNK_COMMITS. "Last measured" is itself evidence-derived: a
// successful trunk run is only credited once it is seen to carry the
// `coverage-profile` artifact, because a `release/*`-headed PR's merge commit
// produces a push run that succeeds having measured nothing, and crediting one
// would reset the counter on a non-measurement. That number is the signal nobody had: it is
// what turns "coverage has not been measured on trunk for four days" from
// something you must go looking for into something every pull request says out
// loud. The lookup is best-effort — no token, no network, or an API error
// degrades to "unavailable" with a warning, never to a failed required context,
// because a GitHub API hiccup is not evidence about this commit's coverage.
//
// Env:
//   RUN_HEAVY           `needs.coverage-plan.outputs.run_heavy` — 'true' | 'false'.
//   GATE_OUTCOME        the `outcome` of the aggregator's floor-gate step
//                       ('success' | 'failure' | 'skipped'). Empty or unset is
//                       read as 'skipped' — a step that reported no outcome did
//                       not run.
//   COVERAGE_PROFILE    merged profile to read as evidence (default: cover-merged.out).
//   EVENT_NAME          github.event_name, for the "why not measured" line.
//   TRUNK_BRANCH        branch whose measurement staleness is reported (default: main).
//   CURRENT_SHA         the commit this run is about; excluded from the trunk lookup.
//   GITHUB_REPOSITORY   "owner/name". Absent -> trunk lookup skipped.
//   GITHUB_TOKEN        token with actions:read. Absent -> trunk lookup skipped.
//   GITHUB_API_URL      API base, default https://api.github.com.
//   GITHUB_STEP_SUMMARY appended to when present (set by Actions).
//
// Exit: 0 for MEASURED-and-clear and for NOT MEASURED; 1 for a missed floor and
// for any claim/evidence disagreement.
//
// node: builtins only — no npm dependencies or setup-node step.

import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { DEFAULT_PROFILE, laneRecordPath, resolveUpdateLanes } from './coverage-summary.mjs';
import { appendStepSummary, error, log, notice, warning } from './lib/gh.mjs';

// The workflow file whose runs answer "when was the trunk last measured".
const COVERAGE_WORKFLOW_FILE = 'coverage.yml';

// How many recent trunk runs to scan for the last successful measurement. One
// page: the outage this closes ran ~90 runs deep, and a gap wider than a full
// page is reported as "not in the last N runs", which is the same alarm.
export const TRUNK_RUN_SCAN_PAGE_SIZE = 100;

// Commits allowed to land on the trunk between measurements before the verdict
// warns. The group coalesces rather than cancels, so a burst legitimately
// leaves a couple of commits measured only by their successor; a backlog deeper
// than this is the lane failing to keep up, or failing outright.
export const MAX_UNMEASURED_TRUNK_COMMITS = 5;

// How many successful trunk runs to probe for a profile artifact before giving
// up. Each probe is one API call, and the newest successful run is nearly
// always the answer; the bound exists so a long stretch of non-measuring
// successes cannot turn one verdict into an unbounded API walk.
export const MAX_TRUNK_CANDIDATE_PROBES = 10;

// The artifact a heavy run uploads and a non-heavy run does not. Its presence
// is the EVIDENCE that a trunk run measured; the run's event and conclusion are
// only the claim (a `release/*`-headed PR's merge commit produces a push run
// that succeeds having measured nothing — coverage-run-heavy.mjs's redundant
// case). Uploaded on `always()` for a heavy run, so a run that measured and
// then MISSED a floor still carries it — which is why the conclusion filter
// below stays as well.
const MEASURED_RUN_ARTIFACT = 'coverage-profile';

// Commit shas are rendered at this width, matching `git log --abbrev=9`.
const SHORT_SHA_LENGTH = 9;

// Events whose coverage.yml run actually measures. A `pull_request` /
// `merge_group` run is skipped by design and its success says nothing about the
// trunk, so crediting one as "the trunk was measured here" would reintroduce
// exactly the false green this script exists to remove.
const MEASURING_EVENTS = new Set(['push', 'schedule', 'workflow_dispatch']);

export const VERDICT_MEASURED = 'MEASURED';
export const VERDICT_NOT_MEASURED = 'NOT MEASURED';
export const VERDICT_BROKEN = 'BROKEN';

/**
 * inspectEvidence — what the filesystem can PROVE about this run's measurement.
 *
 * `present` is "a merged profile exists at all"; `provable` is the stronger
 * "and a lane record bound to these exact bytes says both lanes produced it".
 * The split matters: a present-but-unprovable profile is a wiring failure to
 * report, not an absence to shrug at.
 *
 * The lane-record validation itself is NOT reimplemented here: it delegates to
 * coverage-summary.mjs's `resolveUpdateLanes`, the same five checks (readable
 * JSON, the `{lanes, profileSha256}` shape, the digest binding, the full lane
 * set) that decide whether a profile may become floors. A second copy of the
 * digest check would be the exact drift the digest exists to prevent — two
 * validators disagreeing about which bytes count as proven.
 *
 * `readFile` is injectable so the unit suite can drive every branch without
 * staging files.
 */
export function inspectEvidence(profilePath, readFile = (p) => readFileSync(p, 'utf8')) {
  let profileText;
  try {
    profileText = readFile(profilePath);
  } catch (e) {
    return { present: false, provable: false, reason: `${profilePath} is not present (${e.code ?? e.message})` };
  }
  if (!profileText.trim()) {
    return { present: true, provable: false, reason: `${profilePath} is empty` };
  }

  const recordPath = laneRecordPath(profilePath);
  let recordText = null;
  try {
    recordText = readFile(recordPath);
  } catch {
    recordText = null;
  }

  const resolved = resolveUpdateLanes(recordText, profileText, recordPath);
  if (resolved.err) return { present: true, provable: false, reason: resolved.err };
  return { present: true, provable: true, lanes: resolved.lanes, digest: profileDigestOf(profileText), reason: null };
}

// The digest is reported in the MEASURED banner so a reader can tie the verdict
// to the exact bytes it judged. resolveUpdateLanes has already proven the record
// carries this same value; recomputing it here is a render step, not a check.
function profileDigestOf(profileText) {
  return createHash('sha256').update(profileText).digest('hex');
}

/**
 * classifyMeasurement — the pure verdict over the run's own claim (`runHeavy`),
 * the floor gate's outcome, and what `inspectEvidence` found on disk.
 *
 * Returns `{ verdict, ok, headline, detail }`. `ok === false` exits 1.
 */
export function classifyMeasurement({ runHeavy, gateOutcome, evidence, eventName }) {
  const heavy = runHeavy === 'true';

  if (heavy && !evidence.provable) {
    return {
      verdict: VERDICT_BROKEN,
      ok: false,
      headline: 'this run claimed to measure coverage and produced no profile it can prove',
      detail:
        `coverage-plan set RUN_HEAVY=true, so every lane job should have run and the aggregator should be ` +
        `holding a merged default+chdb profile — but ${evidence.reason}. A green context here would certify ` +
        `a measurement that did not happen, which is the whole failure tsouza/cerberus#2991 records, so this ` +
        `fails instead. Open the lane jobs and the merge step above.`,
    };
  }

  if (heavy) {
    if (gateOutcome === 'success') {
      return {
        verdict: VERDICT_MEASURED,
        ok: true,
        headline: 'the merged profile was compared to the floor ledger and every package cleared its floor',
        detail:
          `Lane set \`${evidence.lanes}\`, profile sha256 \`${evidence.digest}\` — the digest-bound record ` +
          `coverage-summary.mjs wrote beside the bytes it actually gated on.`,
      };
    }
    return {
      verdict: VERDICT_MEASURED,
      ok: false,
      headline: `the merged profile was compared to the floor ledger and the gate reported "${gateOutcome}"`,
      detail:
        `Lane set \`${evidence.lanes}\`, profile sha256 \`${evidence.digest}\`. Read the floor gate's own ` +
        `\`::error::\` above for the packages below their floor. Floors come UP with ` +
        `\`just update-coverage-floor\` and never come down to meet a drop — restore the tests.`,
    };
  }

  if (evidence.present) {
    return {
      verdict: VERDICT_BROKEN,
      ok: false,
      headline: 'this run claimed NOT to measure coverage, yet a merged profile is here',
      detail:
        `RUN_HEAVY was "${runHeavy}", so no lane job should have produced a profile and no merge should have ` +
        `run — but a profile is on disk. The job-level gates and the evidence disagree about what this run ` +
        `did, and a verdict cannot be trusted while they do.`,
    };
  }

  if (gateOutcome !== 'skipped') {
    return {
      verdict: VERDICT_BROKEN,
      ok: false,
      headline: `the floor gate reported "${gateOutcome}" on a run that was not supposed to measure`,
      detail:
        `RUN_HEAVY was "${runHeavy}", so the merge-and-gate step should have been skipped outright. Its ` +
        `condition and coverage-plan's decision have drifted apart.`,
    };
  }

  return {
    verdict: VERDICT_NOT_MEASURED,
    ok: true,
    headline: 'no coverage was measured on this commit',
    detail:
      `The \`${eventName || 'unknown'}\` event runs the package-floor ENROLLMENT scan and the lane roll-up ` +
      `check only; the three measuring lanes are skipped to keep pull-request latency down (coverage-chdb ` +
      `alone runs 35-50 minutes). Measured coverage runs on every push to \`main\`, on the nightly schedule, ` +
      `on \`workflow_dispatch\`, and on \`release/*\`-headed pull requests — see the \`coverage-plan\` job's ` +
      `"decide run_heavy" step for THIS run's decision. A green \`coverage\` check here certifies package ` +
      `enrollment and the lane roll-up, and no coverage percentage whatsoever.`,
  };
}

/**
 * selectMeasuredRunCandidates — trunk runs that COULD have measured, newest
 * first, from the API's newest-first run list.
 *
 * This is a pre-filter, not the answer. A successful run of a non-measuring
 * event proves nothing about coverage, and the run for THIS commit is excluded
 * so a push never reports itself as the previous measurement — but a successful
 * `push` run on the trunk still need not have measured anything: a
 * `release/*`-headed PR's merge commit produces exactly that (RUN_HEAVY=false,
 * because the identical tree was already measured on the PR — see
 * coverage-run-heavy.mjs's redundant case). Crediting one would reset the
 * staleness counter on a run that measured nothing, which is the same
 * claim-for-evidence substitution the verdict above exists to remove. The
 * caller settles it by probing each candidate for the profile artifact only a
 * measuring run uploads.
 */
export function selectMeasuredRunCandidates(runs, { currentSha, trunkBranch }) {
  const candidates = [];
  for (const run of runs ?? []) {
    if (!run || run.conclusion !== 'success') continue;
    if (!MEASURING_EVENTS.has(run.event)) continue;
    if (trunkBranch && run.head_branch !== trunkBranch) continue;
    if (currentSha && run.head_sha === currentSha) continue;
    candidates.push({ id: run.id, sha: run.head_sha, at: run.updated_at ?? run.created_at, url: run.html_url });
  }
  return candidates;
}

/**
 * describeTrunk — pure rendering of the trunk-staleness row plus whether it
 * warrants a warning. Kept separate from the fetch so the suite can pin the
 * threshold behaviour without a network.
 */
export function describeTrunk(trunk, { trunkBranch, scanned = TRUNK_RUN_SCAN_PAGE_SIZE } = {}) {
  if (!trunk) {
    return { line: 'not looked up (no repository/token in this environment)', stale: false };
  }
  if (trunk.error) {
    return { line: `unavailable — ${trunk.error}`, stale: false, degraded: true };
  }
  const window = trunk.scanned ?? scanned;
  if (!trunk.run) {
    return {
      line:
        `**no run in the last ${window} \`${COVERAGE_WORKFLOW_FILE}\` run(s) on \`${trunkBranch}\` both ` +
        `succeeded and uploaded a \`${MEASURED_RUN_ARTIFACT}\` artifact** — the trunk's coverage is ` +
        `currently unknown`,
      stale: true,
    };
  }
  const { sha, at, url } = trunk.run;
  const link = url ? `[\`${String(sha).slice(0, SHORT_SHA_LENGTH)}\`](${url})` : `\`${String(sha).slice(0, SHORT_SHA_LENGTH)}\``;
  if (typeof trunk.aheadBy !== 'number') {
    // An unknown distance must not read as a fresh one. The staleness warning
    // is the whole point of this line, and silently disarming it on a failed
    // /compare call would be the same hollow green in miniature.
    return {
      line: `last measured at ${link} (${at}), an **unknown number of commits behind** \`${trunkBranch}\``,
      stale: false,
      degraded: true,
    };
  }
  return {
    line: `last measured at ${link} (${at}), **${trunk.aheadBy} commit(s) behind** \`${trunkBranch}\``,
    stale: trunk.aheadBy > MAX_UNMEASURED_TRUNK_COMMITS,
  };
}

async function getJSON(url, token, fetchImpl) {
  const res = await fetchImpl(url, {
    headers: {
      Authorization: `Bearer ${token}`,
      Accept: 'application/vnd.github+json',
      'X-GitHub-Api-Version': '2022-11-28',
    },
  });
  if (!res.ok) throw new Error(`${res.status} ${res.statusText} for ${url}`);
  return res.json();
}

/**
 * fetchTrunkMeasurement — network wrapper. Returns `{ run, aheadBy }`, or
 * `{ error }` for anything that went wrong; never throws, because the trunk's
 * measurement history is context for this commit's verdict, not evidence about
 * it, and a required context must not go red on an API hiccup.
 */
export async function fetchTrunkMeasurement({
  repo,
  apiBase = 'https://api.github.com',
  token,
  currentSha,
  trunkBranch = 'main',
  fetchImpl = fetch,
}) {
  try {
    const listURL =
      `${apiBase}/repos/${repo}/actions/workflows/${COVERAGE_WORKFLOW_FILE}/runs` +
      `?branch=${encodeURIComponent(trunkBranch)}&per_page=${TRUNK_RUN_SCAN_PAGE_SIZE}`;
    const data = await getJSON(listURL, token, fetchImpl);
    const runs = data.workflow_runs ?? [];
    const scanned = runs.length;
    const candidates = selectMeasuredRunCandidates(runs, { currentSha, trunkBranch });

    let run = null;
    for (const candidate of candidates.slice(0, MAX_TRUNK_CANDIDATE_PROBES)) {
      const artifacts = await getJSON(
        `${apiBase}/repos/${repo}/actions/runs/${candidate.id}/artifacts?per_page=${TRUNK_RUN_SCAN_PAGE_SIZE}`,
        token,
        fetchImpl,
      );
      if ((artifacts.artifacts ?? []).some((artifact) => artifact?.name === MEASURED_RUN_ARTIFACT)) {
        run = candidate;
        break;
      }
    }
    if (!run) return { run: null, aheadBy: null, scanned };

    let aheadBy = null;
    try {
      const cmp = await getJSON(
        `${apiBase}/repos/${repo}/compare/${run.sha}...${encodeURIComponent(trunkBranch)}`,
        token,
        fetchImpl,
      );
      aheadBy = typeof cmp.ahead_by === 'number' ? cmp.ahead_by : null;
    } catch {
      // The run is the answer that matters; describeTrunk reports an unknown
      // distance as degraded rather than as fresh.
      aheadBy = null;
    }
    return { run, aheadBy, scanned };
  } catch (e) {
    return { error: e.message };
  }
}

/** renderBanner — the markdown a reader of the `coverage` check sees first. */
export function renderBanner({ verdict, headline, detail, trunkLine, trunkBranch }) {
  return [
    `## coverage measurement: ${verdict}`,
    '',
    `**${headline}.**`,
    '',
    detail,
    '',
    `- \`${trunkBranch}\` coverage: ${trunkLine}`,
    '',
  ].join('\n');
}

async function main() {
  const runHeavy = process.env.RUN_HEAVY ?? '';
  // A step that reported no outcome did not run. GitHub sets `outcome` to
  // `skipped` for a step its `if:` excluded, but an unset or empty value must
  // reach the same conclusion rather than the BROKEN branch below — otherwise
  // one runner-side surprise turns the required `coverage` context red on every
  // pull request in the repository, the exact inverse of this script's purpose.
  const gateOutcome = process.env.GATE_OUTCOME || 'skipped';
  const profilePath = process.env.COVERAGE_PROFILE || DEFAULT_PROFILE;
  const trunkBranch = process.env.TRUNK_BRANCH || 'main';

  const evidence = inspectEvidence(profilePath);
  const { verdict, ok, headline, detail } = classifyMeasurement({
    runHeavy,
    gateOutcome,
    evidence,
    eventName: process.env.EVENT_NAME,
  });

  const repo = process.env.GITHUB_REPOSITORY;
  const token = process.env.GITHUB_TOKEN;
  const trunk =
    repo && token
      ? await fetchTrunkMeasurement({
          repo,
          apiBase: process.env.GITHUB_API_URL || 'https://api.github.com',
          token,
          currentSha: process.env.CURRENT_SHA,
          trunkBranch,
        })
      : null;
  const trunkStatus = describeTrunk(trunk, { trunkBranch, scanned: trunk?.scanned });

  appendStepSummary(renderBanner({ verdict, headline, detail, trunkLine: trunkStatus.line, trunkBranch }));

  const message = `coverage measurement: ${verdict} — ${headline}. ${detail}`;
  if (!ok) {
    error(message, { title: `coverage measurement: ${verdict}` });
  } else if (verdict === VERDICT_NOT_MEASURED) {
    // A warning, not a notice: this annotation is the ONE thing standing
    // between a reader and mistaking this green tick for a measured pass.
    warning(message, { title: 'coverage NOT measured on this commit' });
  } else {
    notice(message, { title: 'coverage measured' });
  }

  if (trunkStatus.degraded) {
    warning(`coverage could not read ${trunkBranch}'s measurement history: ${trunk.error}`, {
      title: 'trunk coverage staleness unknown',
    });
  } else if (trunkStatus.stale) {
    warning(
      `${trunkBranch} coverage is stale: ${trunkStatus.line.replace(/\*\*|\[|\]\([^)]*\)/g, '')}. More than ` +
        `${MAX_UNMEASURED_TRUNK_COMMITS} commits have landed since the trunk was last measured, so the floors ` +
        `are being enforced against a tree nobody has checked recently.`,
      { title: `${trunkBranch} coverage is stale` },
    );
  }

  log(message);
  process.exit(ok ? 0 : 1);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await main();
}
