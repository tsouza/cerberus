// regen-diff.mjs — the closing "review this diff before committing" step
// every `update-*-baseline` recipe (plus `gen-config-docs`, `bench-report`
// and `migration-golden`) repeated as:
//
//     @echo
//     @echo "Diff of regenerated <thing>:"
//     @git --no-pager diff --stat <path...> || true
//
// twelve times over (`update-nightly-perf-baseline`, `update-coverage-floor`,
// `update-parity-enrolment-baseline`, `update-parity-ledgers`,
// `update-cardinality-baseline`, `update-solver-decision-baseline`,
// `update-scale-wall-baseline`, `update-perf-smoke-baseline`,
// `update-metadata-query-size-baseline`, `bench-report`, `gen-config-docs`,
// `migration-golden`). `update-parity-enrolment-baseline` is the one
// exception in that set: it prints the bare diff-stat with no banner at all
// — `printDiffStat` matches that by treating an absent `label` as "no
// banner", rather than forcing a thirteenth recipe to carry a label nothing
// else needed. Issue #3095, epic #3091, CLAUDE.md invariant 15.
//
// `|| true` in the replaced idiom is deliberate and preserved here: the
// diff-stat is purely informational, never a gate. Each recipe's own
// regeneration step already exits before reaching this one on a real
// failure, so `printDiffStat` never reflects git's own exit status and this
// module's CLI always exits 0.
//
// CLI: node .github/scripts/lib/regen-diff.mjs [--label <label>] <path...>

import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { git, log } from './gh.mjs';

// printDiffStat — print `git --no-pager diff --stat <paths>`, optionally
// preceded by a blank line and a "Diff of regenerated <label>:" banner.
// `paths` is a path string or an array of them (`update-parity-ledgers`
// diffs two directories at once). Never throws.
export function printDiffStat(paths, label) {
  const pathList = Array.isArray(paths) ? paths : [paths];
  if (label) {
    log('');
    log(`Diff of regenerated ${label}:`);
  }
  const res = git(['--no-pager', 'diff', '--stat', ...pathList]);
  const out = res.stdout.replace(/\n$/, '');
  if (out) log(out);
}

function main() {
  const argv = process.argv.slice(2);
  let label;
  let paths = argv;
  if (argv[0] === '--label') {
    label = argv[1];
    paths = argv.slice(2);
  }
  if (paths.length === 0) {
    console.error('regen-diff: usage: node regen-diff.mjs [--label <label>] <path...>');
    process.exit(1);
  }
  printDiffStat(paths, label);
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) main();
