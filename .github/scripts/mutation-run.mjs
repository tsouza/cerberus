// mutation-run.mjs — run one mutation phase with safe changed-line fallback.
//
// Required env: SCOPE, REPORT, MUTANT_TIMEOUT_MAX.
// Optional env: WORKERS, EXCLUDE_FILES, DIFF_REF.
//
// A non-empty DIFF_REF first invokes gremlins' native merge-base line filter.
// If that report contains zero executable mutants, the script deletes it and
// reruns the phase without --diff. This makes comment-only or otherwise
// mutation-free edits conservative full checks instead of hollow greens.
//
// Both invocations pin --timeout-coefficient so MUTANT_TIMEOUT_MAX is the sole
// per-mutant budget; see perMutantBudgetIsTheCeilingCoefficient below.

import { spawnSync } from 'node:child_process';
import { existsSync, readFileSync, unlinkSync } from 'node:fs';
import process from 'node:process';

import { error, notice } from './lib/gh.mjs';

// gremlins derives each mutant's deadline as
// `min(timeout-coefficient * max(coverage_elapsed, 1s), timeout-max)`, and that
// deadline wraps the WHOLE `go test` child — recompile, link and run. But
// `coverage_elapsed` is the wall time of gremlins' own coverage pass, which is
// dominated by COMPILATION, not by test time. So the derived budget tracks how
// warm the Go build cache happened to be rather than anything about the tests:
// the zero-mutant fallback below re-invokes gremlins in the same job with the
// cache already warmed by the first invocation, the coverage pass collapses
// from ~50s to ~0.4s, `baseTime` clamps to gremlins' own 1s floor, and the
// budget silently drops to 5s — under `internal/promql`'s real ~6.6s
// recompile+link+run cost, so every mutant times out and the leg reports 0.00%
// efficacy. Declaring the coefficient as `ceil(ceiling / 1s floor)` makes
// `coefficient * max(elapsed, 1s)` at least the ceiling for any elapsed time,
// so the budget is exactly MUTANT_TIMEOUT_MAX on every invocation.
const gremlinsCoverageBaselineFloorSeconds = 1;

const goDurationUnitSeconds = { ns: 1e-9, us: 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };

function required(name) {
  const value = String(process.env[name] ?? '').trim();
  if (value === '') throw new Error(`${name} is required`);
  return value;
}

// Parses the subset of Go's duration grammar a timeout ceiling can use: one or
// more unsigned `<number><unit>` terms, e.g. `15s` or `1m30s`.
function goDurationSeconds(name, spec) {
  const terms = spec.match(/[0-9]+(?:\.[0-9]+)?(?:ns|us|ms|s|m|h)/g);
  if (terms === null || terms.join('') !== spec) {
    throw new Error(`${name} is not a Go duration: ${spec}`);
  }
  return terms.reduce((total, term) => {
    const [, value, unit] = /^([0-9]+(?:\.[0-9]+)?)([a-z]+)$/.exec(term);
    return total + Number(value) * goDurationUnitSeconds[unit];
  }, 0);
}

function perMutantBudgetIsTheCeilingCoefficient(name, ceiling) {
  const seconds = goDurationSeconds(name, ceiling);
  if (!(seconds > 0)) throw new Error(`${name} must be a positive duration: ${ceiling}`);
  return String(Math.ceil(seconds / gremlinsCoverageBaselineFloorSeconds));
}

function reportTotal(path) {
  let document;
  try {
    document = JSON.parse(readFileSync(path, 'utf8'));
  } catch (cause) {
    throw new Error(`cannot read gremlins report ${path}: ${cause.message}`);
  }
  if (!Number.isSafeInteger(document.mutants_total) || document.mutants_total < 0) {
    throw new Error(`gremlins report ${path} has invalid mutants_total`);
  }
  return document.mutants_total;
}

function runGremlins({ report, diffRef = '' }) {
  const ceiling = required('MUTANT_TIMEOUT_MAX');
  const args = [
    'unleash',
    '--output',
    report,
    '--on-shutdown-status=not-run',
    '--timeout-max',
    ceiling,
    '--timeout-coefficient',
    perMutantBudgetIsTheCeilingCoefficient('MUTANT_TIMEOUT_MAX', ceiling),
  ];
  const workers = String(process.env.WORKERS ?? '').trim();
  if (workers !== '' && workers !== '0') {
    if (!/^[1-9][0-9]*$/.test(workers)) throw new Error(`WORKERS is invalid: ${workers}`);
    args.push('--workers', workers);
  }
  const exclude = String(process.env.EXCLUDE_FILES ?? '').trim();
  if (exclude !== '') args.push('--exclude-files', exclude);
  if (diffRef !== '') args.push('--diff', diffRef);
  args.push(required('SCOPE'));

  const result = spawnSync('gremlins', args, { stdio: 'inherit' });
  if (result.error) throw new Error(`cannot execute gremlins: ${result.error.message}`);
  if (result.status !== 0) throw new Error(`gremlins exited ${result.status}`);
  if (!existsSync(report)) throw new Error(`gremlins produced no report at ${report}`);
}

function main() {
  const report = required('REPORT');
  const diffRef = String(process.env.DIFF_REF ?? '').trim();
  if (diffRef !== '' && !/^[0-9a-f]{40,64}$/i.test(diffRef)) {
    throw new Error(`DIFF_REF is invalid: ${diffRef}`);
  }
  if (existsSync(report)) throw new Error(`refusing stale gremlins report ${report}`);

  runGremlins({ report, diffRef });
  if (diffRef !== '' && reportTotal(report) === 0) {
    notice('changed-line mutation found zero executable mutants; rerunning the full phase');
    unlinkSync(report);
    runGremlins({ report });
  }
  const total = reportTotal(report);
  if (total <= 0) throw new Error('mutation phase executed zero mutants after full fallback');
  notice(`mutation phase executed ${total} mutant(s)${diffRef === '' ? ' in full' : ''}`);
}

try {
  main();
} catch (cause) {
  error(cause instanceof Error ? cause.message : String(cause));
  process.exit(1);
}
