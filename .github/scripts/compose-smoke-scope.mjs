// compose-smoke-scope.mjs — keeps the exhaustive Compose/Playwright sweep on
// release qualification while the dedicated quickstart workflow owns the fast
// pull-request proof of the repository-root stack.
//
// The old policy booted one Compose stack per Playwright shard whenever an
// ordinary pull request touched a stack-visible path. The required `quickstart`
// context now covers the published startup, readiness, provisioning, and
// Grafana-root contract with one stack. Paying for the browser-depth matrix on
// the same pull request would test the same startup repeatedly and put the
// release sweep back on the merge critical path.
//
// This selector therefore has an event-only policy:
//
//   - ordinary pull_request and merge_group: omit. The required quickstart
//     context gates the projected tree instead;
//   - release/* pull_request, push, and schedule: run the full sweep;
//   - workflow_dispatch: omit unless it explicitly requests regeneration of
//     the Compose crawl inventory;
//   - an unfamiliar event: run, so a future trigger cannot silently de-gate
//     release evidence.
//
// Modes (MODE, or argv[2]; default `verify`):
//   - verify: validate the module can be loaded and the mode is known.
//   - emit: decide and append `in_scope=true|false` to GITHUB_OUTPUT.
//
// Environment:
//   MODE                    `emit` | `verify` (also argv[2]).
//   EVENT_NAME              github.event_name.
//   HEAD_REF                github.head_ref (release-head detection).
//   UPDATE_CRAWL_INVENTORY  workflow_dispatch selection: none, k3d, compose,
//                           or both.
//   GITHUB_OUTPUT           emit destination.
//
// node: builtins only — no npm dependencies or setup-node step.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { runsFullLane } from './lib/scope-gate.mjs';
import { error, log, notice, setOutput } from './lib/gh.mjs';

export const NON_BOOTING_EVENTS = Object.freeze(['merge_group', 'workflow_dispatch']);

export function regeneratesCompose(updateCrawlInventory) {
  return updateCrawlInventory === 'compose' || updateCrawlInventory === 'both';
}

export function decide({ eventName, headRef, updateCrawlInventory = 'none' }) {
  const event = String(eventName ?? '').trim();
  const head = String(headRef ?? '').trim();

  if (event === 'workflow_dispatch') {
    if (regeneratesCompose(updateCrawlInventory)) {
      return {
        inScope: true,
        reason: `dispatch requests the compose crawl-inventory regen (update_crawl_inventory=${updateCrawlInventory})`,
      };
    }
    return { inScope: false, reason: 'manual dispatch requested no Compose inventory regeneration' };
  }

  if (event === 'merge_group') {
    return {
      inScope: false,
      reason: 'the required one-stack quickstart context owns projected-trunk Compose validation',
    };
  }

  if (event === 'pull_request' && !runsFullLane({ eventName: event, headRef: head })) {
    return {
      inScope: false,
      reason: 'ordinary pull requests use the required one-stack quickstart context',
    };
  }

  if (runsFullLane({ eventName: event, headRef: head })) {
    return { inScope: true, reason: `event "${event || '<unknown>'}" runs full release qualification` };
  }

  // The only diff-scoped event not handled above would be one added to the
  // shared helper without being enrolled here. Run rather than silently omit.
  return { inScope: true, reason: `unrecognised event policy for "${event || '<blank>'}"; running fail closed` };
}

function main() {
  const mode = (process.env.MODE || process.argv[2] || 'verify').trim();
  if (mode !== 'verify' && mode !== 'emit') {
    error(`compose-smoke-scope: MODE must be "verify" or "emit" (got "${mode}")`);
    process.exit(1);
  }

  if (mode === 'verify') {
    log('compose-smoke-scope: release-only event policy loaded.');
    return;
  }

  const verdict = decide({
    eventName: process.env.EVENT_NAME,
    headRef: process.env.HEAD_REF,
    updateCrawlInventory: (process.env.UPDATE_CRAWL_INVENTORY || 'none').trim() || 'none',
  });
  notice(`compose-smoke-scope: ${verdict.inScope ? 'running' : 'omitting'} — ${verdict.reason}`);
  setOutput('in_scope', String(verdict.inScope));
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) main();
