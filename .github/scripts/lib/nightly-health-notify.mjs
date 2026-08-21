// lib/nightly-health-notify.mjs — the stable-title tracking-issue lifecycle
// #1861's notify-nightly-failure.mjs (e2e) established, generalised so a
// second `schedule`-triggered lane (#2370's perf-nightly) can reuse the
// exact same mechanism rather than re-deriving it.
//
// A `schedule` trigger has no PR, no reviewer, no default notification
// path — a red run reports into a place nobody looks unless something
// files/refreshes a tracking issue for it. This is that mechanism,
// factored so it does not know which lane is calling it: every lane-
// specific fact (the tracking title, which jobs matter, the body text's
// lane label) is a parameter, never a constant baked in here.
//
// node: builtins only (via ./gh.mjs) + the `gh` CLI already authenticated
// by the calling workflow's GH_TOKEN.

import process from 'node:process';
import { capture, error, notice } from './gh.mjs';

/**
 * Pure roll-up over a schedule run's terminal job results. Anything other
 * than `success` — a real `failure`, a `cancelled` kill, or an unexpected
 * `skipped` — counts against a clean night, mirroring each lane's own
 * terminal-aggregator rule.
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
 * Pure decision over (current health) x (an existing tracking issue or
 * not). Four cells, all named explicitly rather than derived, so a guard
 * test can drive every one directly.
 */
export function decideNotifyAction({ ok, existingIssueNumber }) {
  const hasExisting = existingIssueNumber !== null && existingIssueNumber !== undefined;
  if (!ok) {
    return hasExisting ? { action: 'comment', number: existingIssueNumber } : { action: 'create' };
  }
  return hasExisting ? { action: 'close', number: existingIssueNumber } : { action: 'noop' };
}

/** laneLabel names the lane in the body text (e.g. "e2e", "perf-nightly");
 * issueRef is the design-rationale issue/PR this mechanism traces back to
 * for THIS lane (not necessarily #1861 — a lane adopting this mechanism
 * later should cite its own adoption issue). */
export function buildFailureBody({ laneLabel, failed, runUrl, runId, issueRef }) {
  const list = failed.map((f) => `- \`${f}\``).join('\n');
  return [
    `The nightly \`${laneLabel}\` schedule run did not reach a clean pass.`,
    '',
    `Run: ${runUrl} (id ${runId})`,
    '',
    'Non-success jobs (anything but `success` — a real failure, a `cancelled`',
    'kill, or an unexpected `skipped`):',
    list,
    '',
    `Filed/updated automatically by this lane's notify script — see ${issueRef} for why this mechanism` +
      ' exists: a nightly that reports red into a place nobody looks is as silent as one that never' +
      ' reports at all. This issue closes itself on the next clean nightly.',
  ].join('\n');
}

export function buildRecoveryBody({ laneLabel, runUrl, runId }) {
  return [
    `The nightly \`${laneLabel}\` schedule run reached a clean pass.`,
    '',
    `Run: ${runUrl} (id ${runId})`,
    '',
    'Closing automatically.',
  ].join('\n');
}

function ghOrDie(args, failMessage, contextTitle) {
  const res = capture('gh', args);
  if (res.status !== 0) {
    error(`${failMessage}: ${res.stderr.trim() || res.stdout.trim()}`, { title: contextTitle });
    process.exit(1);
  }
  return res.stdout;
}

/**
 * The shared create/comment/close/noop orchestration every lane's `main()`
 * drives. Exits the process itself (1 on a non-clean night or a `gh`
 * failure, implicit 0 otherwise) — matches notify-nightly-failure.mjs's
 * original behaviour, which the lane's own required-check step relies on
 * to fail loudly.
 *
 * trackingLabels: the `gh issue list --label` filter (both ANDed).
 * trackingTitle: the exact, stable dedup title — never interpolate the run
 *   id or date into it, or every bad night opens a NEW issue instead of
 *   refreshing the one that already tracks the incident.
 * laneLabel / issueRef: passed straight through to buildFailureBody /
 *   buildRecoveryBody.
 * contextTitle: the `error()`/`notice()` title tag for this lane's log
 *   lines (e.g. "nightly-health-notify", "perf-nightly-health-notify").
 * failureNoticeTitle: the title used specifically on the create/comment
 *   error() call (e.g. "nightly e2e run failed").
 */
export function runNotifyMain({
  repo,
  runId,
  runUrl,
  jobResults,
  trackingLabels,
  trackingTitle,
  laneLabel,
  issueRef,
  contextTitle,
  failureNoticeTitle,
}) {
  const health = classifyNightlyHealth(jobResults);

  const labelArgs = trackingLabels.flatMap((l) => ['--label', l]);
  const listOut = ghOrDie(
    ['issue', 'list', '--repo', repo, '--state', 'open', ...labelArgs, '--json', 'number,title', '--limit', '30'],
    'gh issue list failed',
    contextTitle,
  );
  let issues;
  try {
    issues = JSON.parse(listOut);
  } catch (err) {
    error(`gh issue list returned unparsable JSON: ${err.message}`, { title: contextTitle });
    process.exit(1);
  }
  const existingIssueNumber = findTrackingIssue(issues, trackingTitle);
  const decision = decideNotifyAction({ ok: health.ok, existingIssueNumber });

  switch (decision.action) {
    case 'create': {
      const body = buildFailureBody({ laneLabel, failed: health.failed, runUrl, runId, issueRef });
      const out = ghOrDie(
        ['issue', 'create', '--repo', repo, '--title', trackingTitle, '--body', body, ...labelArgs],
        'gh issue create failed',
        contextTitle,
      );
      error(`nightly ${laneLabel} run did not reach a clean pass (${health.failed.join('; ')}); filed ${out.trim()}`, {
        title: failureNoticeTitle,
      });
      process.exit(1);
      break;
    }
    case 'comment': {
      const body = buildFailureBody({ laneLabel, failed: health.failed, runUrl, runId, issueRef });
      ghOrDie(
        ['issue', 'comment', String(decision.number), '--repo', repo, '--body', body],
        'gh issue comment failed',
        contextTitle,
      );
      error(
        `nightly ${laneLabel} run did not reach a clean pass (${health.failed.join('; ')}); updated #${decision.number}`,
        { title: failureNoticeTitle },
      );
      process.exit(1);
      break;
    }
    case 'close': {
      const body = buildRecoveryBody({ laneLabel, runUrl, runId });
      ghOrDie(
        ['issue', 'close', String(decision.number), '--repo', repo, '--comment', body],
        'gh issue close failed',
        contextTitle,
      );
      notice(`nightly ${laneLabel} run reached a clean pass; closed tracking issue #${decision.number}.`);
      break;
    }
    case 'noop':
    default:
      notice(`nightly ${laneLabel} run reached a clean pass; no tracking issue open.`);
      break;
  }
}
