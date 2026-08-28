// assert-gate-suites-wired — every `.github/scripts/**/*.test.mjs` must be
// invoked by at least one workflow.
//
// WHY THIS EXISTS. A test suite no workflow runs is indistinguishable, from the
// outside, from one that passes: the file is there, it is well written, and
// nothing reports that it never executed. It reads as coverage in a review and
// provides none. That is the same failure mode as a gate that cannot fail —
// the green means something other than what a reader takes it to mean.
//
// It had happened three times before this gate existed, silently:
// `gremlins-threshold.test.mjs` (added with the timed-out/nothing-completed
// gate in this very release cycle), `merge-crawl-slices.test.mjs` and
// `property-fanout.test.mjs`. Each was written, committed, reviewed and never
// run. Wiring those three fixed the instance; this fixes the class.
//
// The check is deliberately structural rather than a list: it enumerates the
// suites on disk and the `node --test <path>` invocations across the workflow
// files, and reports the difference. There is no allow-list — a suite that
// genuinely should not run in CI has no reason to exist.
//
// ENV CONTRACT
//   REPO_ROOT     — repository root. Default `process.cwd()`.
//   SCRIPTS_DIR   — suites to require. Default `.github/scripts`.
//   WORKFLOW_DIR  — workflows to scan. Default `.github/workflows`.
//
// Exit: 0 when every suite is invoked, 1 otherwise.

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { isAbsolute, join, relative } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { error, log, notice } from './lib/gh.mjs';

const suiteSuffix = '.test.mjs';

// walk — every file under dir, recursively, as repo-relative paths.
function walk(dir, root, out = []) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) walk(full, root, out);
    else out.push(relative(root, full));
  }
  return out;
}

// invokedSuites — every path named by a `node --test <path>` in the workflows.
// Matched on the literal path because that is what a workflow actually runs; a
// glob or a variable would make this gate unable to answer its own question,
// and is worth failing on rather than guessing about.
export function invokedSuites(workflowText) {
  const found = new Set();
  for (const m of workflowText.matchAll(/node\s+--test\s+(\S+\.test\.mjs)/g)) {
    found.add(m[1]);
  }
  return found;
}

export function scan({ root = process.cwd(), scriptsDir = '.github/scripts', workflowDir = '.github/workflows' } = {}) {
  const abs = (p) => (isAbsolute(p) ? p : join(root, p));

  const suites = walk(abs(scriptsDir), root).filter((p) => p.endsWith(suiteSuffix));

  const invoked = new Set();
  for (const file of readdirSync(abs(workflowDir))) {
    if (!file.endsWith('.yml') && !file.endsWith('.yaml')) continue;
    for (const p of invokedSuites(readFileSync(join(abs(workflowDir), file), 'utf8'))) invoked.add(p);
  }

  return { suites, invoked: [...invoked], unwired: suites.filter((s) => !invoked.has(s)) };
}

function main() {
  const { suites, unwired } = scan();
  if (suites.length === 0) {
    // A gate that passes because it found nothing to check reports the same
    // green as a satisfied one.
    error('assert-gate-suites-wired: found no *.test.mjs suites at all — the scan is broken, not the tree');
    return 1;
  }
  if (unwired.length > 0) {
    error(
      `${unwired.length} gate test suite(s) are never invoked by any workflow:\n` +
        unwired.map((s) => `  - ${s}`).join('\n') +
        `\n\nAdd a \`node --test <path>\` step for each. A suite nothing runs reads as coverage ` +
        `in review and provides none.`,
    );
    return 1;
  }
  log(`assert-gate-suites-wired: ${suites.length} suite(s), all invoked by a workflow.`);
  notice(`${suites.length} gate test suites wired`);
  return 0;
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exit(main());
}
