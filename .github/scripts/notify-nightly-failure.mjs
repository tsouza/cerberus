// notify-nightly-failure.mjs — file (or refresh) a single tracking issue when
// the nightly `e2e` schedule run does not reach a clean pass, and close it
// automatically on the next clean night.
//
// Why this exists
// ---------------
// #1861 named two failures. The first — the nightly `shard-crawl` job being
// KILLED (`cancelled`) instead of reaching a real verdict — is fixed:
// crawl-terminal / dashboard-crawl-terminal (assert-crawl-terminal.mjs) now
// fail the run loudly on anything but `success`/`failure`, and real nightly
// runs since have reached `failure`, a genuine terminal result.
//
// That fix uncovered the second failure the issue's own acceptance text asked
// for and a first pass at this issue did not deliver: a `failure` that
// reports into a place nobody looks is exactly as silent as a `cancelled`
// that reports nowhere. The nightly `schedule` trigger has no PR, no
// reviewer, no default notification path — `e2e.yml`'s two exhaustive lanes
// are release gates, not PR gates (see the file header), so nothing reads a
// nightly run's result unless a human goes looking. The nightly ran
// `failure` for FOUR consecutive nights (2026-08-15 .. 2026-08-18) before
// anyone noticed, and it was noticed by accident — a release-staging PR
// happened to be the first `release/*` PR since the regression, and that
// lane's own required check is what surfaced it (see #1861's reopening
// comment).
//
// What this does
// ---------------
// Runs as the last job of a `schedule`-triggered e2e.yml run, downstream of
// every terminal aggregator/leaf job the lane has. Rolls their `needs.*.result`
// facts up with the same strict `!== 'success'` rule the aggregators
// themselves use (catches `failure` AND `cancelled` AND an unexpected
// `skipped` in one comparison — see perf-guards-aggregate.mjs / dashboard's
// own gate step for the same rule applied one level up):
//
//   - not all clean -> open or update the ONE nightly-health tracking issue
//     (matched by an exact, stable title so consecutive bad nights update the
//     same issue instead of piling up duplicates) and exit 1, so the job
//     itself is loud too.
//   - all clean, and a tracking issue is open -> comment + close it. A
//     tracking issue nobody ever closes is its own kind of noise: the next
//     person to look would not be able to tell a live incident from a stale
//     one.
//   - all clean, no tracking issue -> nothing to do.
//
// Uses the existing `automated` + `area/ci` labels (both already exist in the
// repo) rather than a new label, so there is no separate label-provisioning
// step that could itself go stale.
//
// Env:
//   REPO                        `owner/repo` (github.repository).
//   RUN_ID / RUN_URL            identify the run in the issue body.
//   RESULT_COMPOSE_SMOKE, RESULT_CRAWL_TERMINAL, RESULT_DASHBOARD,
//   RESULT_DASHBOARD_CRAWL_TERMINAL, RESULT_STARTUP_BENCH, RESULT_CHAOS,
//   RESULT_BWC_MINIO            `needs.<job>.result` for every terminal job
//                                the nightly run fans out to (e2e.yml's job
//                                graph — see the `nightly-health-notify` job).
//
// Exit: 0 when the nightly reached a clean pass (whether or not a stale
// tracking issue needed closing); 1 when it did not, or when the gh calls
// themselves fail.
//
// node: builtins only (via lib/gh.mjs) + the `gh` CLI already authenticated
// by the workflow's GH_TOKEN.

import process from 'node:process';
import { capture, error, notice } from './lib/gh.mjs';

/** The one label pair used to find (and file) the tracking issue. Both
 * already exist as repo labels; `gh issue list --label` ANDs multiple
 * `--label` flags, so this narrows to issues carrying BOTH. */
export const NIGHTLY_TRACKING_LABELS = ['automated', 'area/ci'];

/** Exact, stable title — the dedup key. Never interpolate the run id or date
 * into it: doing so would make every bad night open a NEW issue instead of
 * refreshing the one that already tracks the incident. */
export const NIGHTLY_TRACKING_TITLE = 'nightly e2e run did not reach a clean pass';

/**
 * Pure roll-up over the terminal job results a schedule run produces.
 * Mirrors the aggregators' own rule: anything other than `success` — a real
 * `failure`, a `cancelled` kill, or an unexpected `skipped` — counts against
 * a clean night. Every job named here is unconditional on `schedule` (see
 * each job's own `if:` in e2e.yml), so `skipped` is never the benign case it
 * would be on another trigger.
 */
export function classifyNightlyHealth(jobResults) {
  const failed = Object.entries(jobResults)
    .filter(([, result]) => result !== 'success')
    .map(([name, result]) => `${name}: ${result || '(missing)'}`);
  return { ok: failed.length === 0, failed };
}

/** Find the open tracking issue, if any, by its exact stable title. Pure so
 * the dedup logic is tested without a `gh` round-trip. */
export function findTrackingIssue(issues, title) {
  const match = (issues || []).find((issue) => issue.title === title);
  return match ? match.number : null;
}

/**
 * Pure decision over (current health) x (an existing tracking issue or not).
 * Four cells, all named explicitly rather than derived, so a guard test can
 * drive every one directly.
 */
export function decideNotifyAction({ ok, existingIssueNumber }) {
  const hasExisting = existingIssueNumber !== null && existingIssueNumber !== undefined;
  if (!ok) {
    return hasExisting ? { action: 'comment', number: existingIssueNumber } : { action: 'create' };
  }
  return hasExisting ? { action: 'close', number: existingIssueNumber } : { action: 'noop' };
}

export function buildFailureBody({ failed, runUrl, runId }) {
  const list = failed.map((f) => `- \`${f}\``).join('\n');
  return [
    'The nightly `e2e` schedule run did not reach a clean pass.',
    '',
    `Run: ${runUrl} (id ${runId})`,
    '',
    'Non-success jobs (anything but `success` — a real failure, a `cancelled`',
    'kill, or an unexpected `skipped`):',
    list,
    '',
    'Filed/updated automatically by `.github/scripts/notify-nightly-failure.mjs`' +
      ' — see tsouza/cerberus#1861 for why this exists: a nightly that reports' +
      ' red into a place nobody looks is as silent as one that never reports at' +
      ' all. This issue closes itself on the next clean nightly.',
  ].join('\n');
}

export function buildRecoveryBody({ runUrl, runId }) {
  return [
    'The nightly `e2e` schedule run reached a clean pass.',
    '',
    `Run: ${runUrl} (id ${runId})`,
    '',
    'Closing automatically — see `.github/scripts/notify-nightly-failure.mjs`.',
  ].join('\n');
}

function readJobResults(env) {
  return {
    'compose-smoke': env.RESULT_COMPOSE_SMOKE ?? '',
    'crawl-terminal': env.RESULT_CRAWL_TERMINAL ?? '',
    dashboard: env.RESULT_DASHBOARD ?? '',
    'dashboard-crawl-terminal': env.RESULT_DASHBOARD_CRAWL_TERMINAL ?? '',
    'startup-bench': env.RESULT_STARTUP_BENCH ?? '',
    chaos: env.RESULT_CHAOS ?? '',
    'bwc-minio': env.RESULT_BWC_MINIO ?? '',
  };
}

function ghOrDie(args, failMessage) {
  const res = capture('gh', args);
  if (res.status !== 0) {
    error(`${failMessage}: ${res.stderr.trim() || res.stdout.trim()}`, { title: 'nightly-health-notify' });
    process.exit(1);
  }
  return res.stdout;
}

function main() {
  const repo = process.env.REPO ?? '';
  const runId = process.env.RUN_ID ?? '';
  const runUrl = process.env.RUN_URL ?? '';
  const jobResults = readJobResults(process.env);

  const health = classifyNightlyHealth(jobResults);

  const labelArgs = NIGHTLY_TRACKING_LABELS.flatMap((l) => ['--label', l]);
  const listOut = ghOrDie(
    ['issue', 'list', '--repo', repo, '--state', 'open', ...labelArgs, '--json', 'number,title', '--limit', '30'],
    'gh issue list failed',
  );
  let issues;
  try {
    issues = JSON.parse(listOut);
  } catch (err) {
    error(`gh issue list returned unparsable JSON: ${err.message}`, { title: 'nightly-health-notify' });
    process.exit(1);
  }
  const existingIssueNumber = findTrackingIssue(issues, NIGHTLY_TRACKING_TITLE);
  const decision = decideNotifyAction({ ok: health.ok, existingIssueNumber });

  switch (decision.action) {
    case 'create': {
      const body = buildFailureBody({ failed: health.failed, runUrl, runId });
      const out = ghOrDie(
        [
          'issue',
          'create',
          '--repo',
          repo,
          '--title',
          NIGHTLY_TRACKING_TITLE,
          '--body',
          body,
          ...labelArgs,
        ],
        'gh issue create failed',
      );
      error(`nightly e2e run did not reach a clean pass (${health.failed.join('; ')}); filed ${out.trim()}`, {
        title: 'nightly e2e run failed',
      });
      process.exit(1);
      break;
    }
    case 'comment': {
      const body = buildFailureBody({ failed: health.failed, runUrl, runId });
      ghOrDie(
        ['issue', 'comment', String(decision.number), '--repo', repo, '--body', body],
        'gh issue comment failed',
      );
      error(
        `nightly e2e run did not reach a clean pass (${health.failed.join('; ')}); updated #${decision.number}`,
        { title: 'nightly e2e run failed' },
      );
      process.exit(1);
      break;
    }
    case 'close': {
      const body = buildRecoveryBody({ runUrl, runId });
      ghOrDie(
        ['issue', 'close', String(decision.number), '--repo', repo, '--comment', body],
        'gh issue close failed',
      );
      notice(`nightly e2e run reached a clean pass; closed tracking issue #${decision.number}.`);
      break;
    }
    case 'noop':
    default:
      notice('nightly e2e run reached a clean pass; no tracking issue open.');
      break;
  }
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner or shell out to `gh`.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
