// compat-baseline-sync.mjs — rewrite one head's deterministic buckets in
// compatibility/parity-baseline/ from a run's compat-cases.json.
//
// The ratchet (compat-ratchet.mjs) gates on the exact, sorted roster of
// case IDs, so moving the baseline by hand means transcribing hundreds
// of IDs in sort order — a step that fails closed but wastes a CI cycle
// every time a character slips. This writes the entry from the artefact
// the harness actually produced, which is the only roster that can be
// correct by construction.
//
// It refuses to record a divergence. If any case in the run failed, the
// baseline it would write could not satisfy `passed == total ==
// cases.length`, and a tool that quietly wrote a smaller roster would be
// an allow-list generator — so it exits 1 and names the failing cases
// instead. Fix them at the source, then re-run.
//
// Usage:
//   node .github/scripts/compat-baseline-sync.mjs <compat-cases.json>
//
// Env contract:
//   BASELINE  path to the baseline shard directory to update
//             (default: compatibility/parity-baseline/).
//
// Exit codes:
//   0  the baseline entry now matches the run's roster (or already did).
//   1  bad arguments, unreadable/malformed input, or the run contains a
//      failing case.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { error, log } from './lib/gh.mjs';
import {
  DEFAULT_BASELINE,
  SUPPORTED_HEADS,
  syncParityBaselineHead,
} from './lib/compat-baseline.mjs';
import { runRoster } from './compat-ratchet.mjs';

class SyncFailure extends Error {}

function fail(message) {
  error(message, { title: 'compat baseline sync' });
  throw new SyncFailure();
}

function main() {
  const casesPath = process.argv[2];
  if (!casesPath) {
    fail('usage: node .github/scripts/compat-baseline-sync.mjs <compat-cases.json>');
  }
  const baselinePath = process.env.BASELINE || DEFAULT_BASELINE;

  let casesDoc;
  try {
    casesDoc = JSON.parse(readFileSync(casesPath, 'utf8'));
  } catch (e) {
    fail(`could not read ${casesPath}: ${e.message}`);
  }
  const head = casesDoc?.head;
  if (typeof head !== 'string' || head.trim() === '') {
    fail(`${casesPath} has no 'head' field — cannot tell which baseline entry to rewrite`);
  }
  if (!SUPPORTED_HEADS.includes(head)) {
    fail(`${casesPath} names unknown parity head ${JSON.stringify(head)}`);
  }
  const { cases, err } = runRoster(casesDoc, head);
  if (err) {
    fail(`bad ${casesPath}: ${err}`);
  }

  const failing = [...cases].filter(([, passed]) => !passed).map(([id]) => id).sort();
  if (failing.length) {
    fail(
      `${casesPath} records ${failing.length} failing case(s) for '${head}'. The baseline records ` +
        `FULL parity only — writing a roster that omits them would be an allow-list. Fix them at ` +
        `the source first:\n${failing.map((id) => `  - ${id}`).join('\n')}`,
    );
  }

  const ids = [...cases.keys()].sort();
  let result;
  try {
    result = syncParityBaselineHead(baselinePath, head, ids);
  } catch (e) {
    fail(`could not sync ${baselinePath}: ${e.message}`);
  }

  log(
    `compat-baseline-sync: heads.${head} <- ${ids.length} passing case(s) from ${casesPath}; ` +
      `${result.changed.length} bucket(s) rewritten, ${result.removed.length} stale file(s) pruned`,
  );
  process.exitCode = 0;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  try {
    main();
  } catch (e) {
    if (e instanceof SyncFailure) process.exitCode = 1;
    else throw e;
  }
}
