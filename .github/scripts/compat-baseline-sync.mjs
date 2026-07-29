// compat-baseline-sync.mjs — rewrite one head's entry in
// compatibility/parity-baseline.json from a run's compat-cases.json.
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
//   BASELINE  path to the baseline JSON to rewrite in place
//             (default: compatibility/parity-baseline.json).
//
// Exit codes:
//   0  the baseline entry now matches the run's roster (or already did).
//   1  bad arguments, unreadable/malformed input, or the run contains a
//      failing case.

import { readFileSync, writeFileSync } from 'node:fs';
import process from 'node:process';
import { error, log } from './lib/gh.mjs';
import { runRoster } from './compat-ratchet.mjs';

const DEFAULT_BASELINE = 'compatibility/parity-baseline.json';

function fail(message) {
  error(message, { title: 'compat baseline sync' });
  process.exit(1);
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

  let baseline;
  try {
    baseline = JSON.parse(readFileSync(baselinePath, 'utf8'));
  } catch (e) {
    fail(`could not read ${baselinePath}: ${e.message}`);
  }
  if (!baseline?.heads?.[head]) {
    fail(`${baselinePath} has no heads.${head} entry to rewrite`);
  }

  const ids = [...cases.keys()].sort();
  baseline.heads[head] = { passed: ids.length, total: ids.length, cases: ids };
  writeFileSync(baselinePath, `${JSON.stringify(baseline, null, 2)}\n`);

  log(`compat-baseline-sync: heads.${head} <- ${ids.length} passing case(s) from ${casesPath}`);
  process.exit(0);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main();
}
