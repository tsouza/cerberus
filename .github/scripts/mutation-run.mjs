// mutation-run.mjs — run one mutation phase with safe changed-line fallback,
// under a per-mutant budget MEASURED from this leg's own recompile+link+run
// cycle on this runner.
//
// Required env: SCOPE, REPORT, MUTANT_TIMEOUT_MIN, MUTANT_TIMEOUT_MAX.
// Optional env: WORKERS, EXCLUDE_FILES, DIFF_REF.
//
// A non-empty DIFF_REF first invokes gremlins' native merge-base line filter.
// If that report contains zero executable mutants, the script deletes it and
// reruns the phase without --diff. This makes comment-only or otherwise
// mutation-free edits conservative full checks instead of hollow greens.
//
// Both invocations pin --timeout-coefficient so the derived budget is the sole
// per-mutant budget; see perMutantBudgetIsTheCeilingCoefficient below.

import { spawnSync } from 'node:child_process';
import { existsSync, readdirSync, readFileSync, statSync, unlinkSync, writeFileSync } from 'node:fs';
import { availableParallelism } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

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
// budget silently drops to 5s — under `internal/promql`'s real per-mutant
// recompile+link+run cost, so every mutant times out and the leg reports 0.00%
// efficacy (#2692). Declaring the coefficient as `ceil(budget / 1s floor)`
// makes `coefficient * max(elapsed, 1s)` at least the budget for any elapsed
// time, so the budget is exactly the value derived below on every invocation.
const gremlinsCoverageBaselineFloorSeconds = 1;

// perMutantRunnerVarianceHeadroom is the multiple applied over the measured
// cycle to absorb how much slower this runner may get AFTER the probe ran.
//
// Not a guess: runs 33542904091 and 33551271099 are the same leg an hour apart,
// and the second runner took 53.2s to gather coverage where the first took
// 36.8s — 44% slower on identical work. Doubling clears that with margin.
//
// Erring high is the cheap direction. A budget larger than a mutant needs costs
// wall clock only on a mutant that genuinely never terminates, and the job's own
// `timeout-minutes` is the backstop for that (a leg that hits it reports
// cancelled, which the `mutation` aggregator already fails). A budget SMALLER
// than a mutant needs used to cost efficacy points in cerberus's favour, which
// is what #2903 fixed in gremlins-threshold.mjs — and still costs them, now in
// the honest direction.
const perMutantRunnerVarianceHeadroom = 2;

// perMutantResidentMemoryMax is the ceiling on ONE mutant's test binary,
// enforced by .github/scripts/mutant-memory-guard.mjs (which see for the
// mechanism and for why the breach is recorded as TIMED OUT rather than as a
// kill).
//
// The wall-clock budget above bounds TIME and nothing else, and the two bounds
// cannot be collapsed into one: `internal/logql/logpattern`'s
// REMOVE_SELF_ASSIGNMENTS mutant at pattern.go:129 allocates at ~1.5 GiB/s
// (measured: 5 GiB of RSS in 3.3s), so it exhausts a 16 GB runner in about ten
// seconds — under MUTANT_TIMEOUT_MIN, and far under the ~63s its own compile
// cycle makes the derivation ask for. Lowering the leash until it is memory-safe
// would starve every honest mutant on the leg, which is the failure #2903 fixed
// (#2919).
//
// The value is measured, not guessed. Peak RSS of an UNMUTATED test binary,
// per mutation-phases.mjs scope, on an 8-core machine:
//
//   internal/promql 112MiB   internal/chsql 44MiB   internal/engine 43MiB
//   internal/logql   35MiB   internal/qlcommon 30MiB   internal/traceql 28MiB
//   internal/optimizer 23MiB   internal/chplan 22MiB   internal/logql/lsyntax 10MiB
//
// 1 GiB is ~9x the heaviest of those, so no honest mutant can approach it, and
// it is small enough that even gremlins' default fan-out (runtime.NumCPU()
// concurrent mutants) cannot add up to the runner's ceiling.
const perMutantResidentMemoryMax = '1GiB';

// perMutantMemoryHoldGraceSeconds is how long the guard holds PAST the mutant's
// own deadline before giving up and exiting. gremlins kills the whole `go test`
// process group when its deadline fires, so the guard is normally reaped well
// inside this; the grace exists only so a breach on a path with no deadline
// behind it (gremlins' unmutated coverage run) cannot wedge the job.
const perMutantMemoryHoldGraceSeconds = 30;

const goDurationUnitSeconds = { ns: 1e-9, us: 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };

function required(name) {
  const value = String(process.env[name] ?? '').trim();
  if (value === '') throw new Error(`${name} is required`);
  return value;
}

// Parses the subset of Go's duration grammar a timeout bound can use: one or
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

function positiveDurationSeconds(name) {
  const spec = required(name);
  const seconds = goDurationSeconds(name, spec);
  if (!(seconds > 0)) throw new Error(`${name} must be a positive duration: ${spec}`);
  return seconds;
}

function perMutantBudgetIsTheCeilingCoefficient(seconds) {
  return String(Math.ceil(seconds / gremlinsCoverageBaselineFloorSeconds));
}

// concurrentMutants is how many `go test` children gremlins runs at once, which
// is what each one's own compile has to share the machine with. `workers: 0`
// (the phase table's DEFAULT_WORKERS) means gremlins' own default of
// runtime.NumCPU().
function concurrentMutants() {
  const workers = String(process.env.WORKERS ?? '').trim();
  if (workers !== '' && workers !== '0') return Number(workers);
  return availableParallelism();
}

// probeFile picks the file the probe below edits: the largest non-test .go file
// directly in the scope directory. Largest because it is the closest stand-in
// for the worst mutant this leg can draw, and deterministic so two runs of the
// same leg measure the same thing.
function probeFile(scope) {
  const dir = resolve(scope);
  let chosen = null;
  for (const name of readdirSync(dir).sort()) {
    if (!name.endsWith('.go') || name.endsWith('_test.go')) continue;
    const path = join(dir, name);
    const { size } = statSync(path);
    if (chosen === null || size > chosen.size) chosen = { path, size };
  }
  return chosen?.path ?? null;
}

// measurePerMutantCycleSeconds times ONE mutant's worth of work: change a byte
// of production source in the scope, then run the scope's tests exactly as
// gremlins runs them for a mutant (`-count=1` to defeat the test result cache,
// `-failfast` because that is what gremlins passes). The edit is what makes the
// measurement honest — Go's build cache is keyed on content, so timing an
// unchanged package measures nothing a mutant will ever pay.
//
// The source file is restored in a finally, so the tree gremlins then copies is
// byte-identical to the checkout.
//
// Any failure to measure — no probe file, a build error, a `go` that is not
// there — yields null, and the caller falls back to MUTANT_TIMEOUT_MIN, which
// is the value the lane ran on before this measurement existed.
function measurePerMutantCycleSeconds(scope, capSeconds) {
  const path = probeFile(scope);
  if (path === null) {
    notice(`no non-test .go file directly in ${scope}: cannot measure the per-mutant cycle`);
    return null;
  }
  const original = readFileSync(path);
  const started = process.hrtime.bigint();
  try {
    writeFileSync(path, Buffer.concat([original, Buffer.from('\n// per-mutant budget probe\n')]));
    const result = spawnSync(
      'go',
      ['test', '-count=1', '-failfast', '-timeout', `${capSeconds}s`, scope],
      { stdio: 'inherit', timeout: capSeconds * 1000 * perMutantRunnerVarianceHeadroom },
    );
    // A probe killed by its own watchdog still measured something real — a
    // cycle longer than any budget this script may hand out — so its elapsed
    // time is kept and clamps to MUTANT_TIMEOUT_MAX below. Only a `go` that
    // could not be launched at all measured nothing.
    if (result.error && result.error.code !== 'ETIMEDOUT') {
      notice(`cannot measure the per-mutant cycle: ${result.error.message}`);
      return null;
    }
  } finally {
    writeFileSync(path, original);
  }
  return Number(process.hrtime.bigint() - started) / 1e9;
}

// perMutantBudgetSeconds is the deadline handed to gremlins, in seconds.
//
// It is measured rather than declared because the thing it has to cover is a
// COMPILE, and a compile's cost is a property of the package and the runner,
// not of the test suite. The flat 15s this replaced was picked, and measurement
// says it was under the cost it was meant to bound: `internal/chsql` recompiles
// and links in 12.7-14.4s per mutant on an 8-core machine and runs its tests in
// 0.31s, so nearly the whole budget went to the compiler and every contended
// runner pushed mutants over (#2903).
//
// MIN is the floor a measurement may not lower the budget below, so a probe
// that fails to measure anything leaves the lane exactly where it was. MAX is
// the ceiling a measurement may not raise it above, so one pathological probe
// cannot spend the job's whole wall-clock allowance on hung mutants.
function perMutantBudgetSeconds(scope) {
  const min = positiveDurationSeconds('MUTANT_TIMEOUT_MIN');
  const max = positiveDurationSeconds('MUTANT_TIMEOUT_MAX');
  if (max < min) throw new Error(`MUTANT_TIMEOUT_MAX is below MUTANT_TIMEOUT_MIN: ${max}s < ${min}s`);

  const cycle = measurePerMutantCycleSeconds(scope, max);
  if (cycle === null) {
    notice(`per-mutant budget ${min}s (the floor; the cycle could not be measured)`);
    return min;
  }
  const fanOut = concurrentMutants();
  const derived = Math.ceil(cycle * fanOut * perMutantRunnerVarianceHeadroom);
  const budget = Math.min(max, Math.max(min, derived));
  notice(
    `measured per-mutant cycle ${cycle.toFixed(1)}s for ${scope}; ` +
      `x${fanOut} concurrent mutants x${perMutantRunnerVarianceHeadroom} headroom = ${derived}s, ` +
      `clamped to [${min}s, ${max}s] -> per-mutant budget ${budget}s`,
  );
  return budget;
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

// memoryGuardedEnv returns the environment gremlins runs under: this process's
// own, plus the `go test -exec` hook that puts mutant-memory-guard.mjs in front
// of every test binary gremlins launches, plus the two bounds the guard reads.
//
// -exec is reached through GOFLAGS because gremlins builds the `go test` argv
// itself and takes no pass-through for extra flags. GOFLAGS is SPACE-separated
// with no quoting, so the guard's path may not contain whitespace, and an
// existing GOFLAGS is appended to rather than replaced.
//
// The guard reads the child's RSS from /proc, so a platform without it cannot
// enforce the bound. That is a hard failure rather than a silent unguarded run:
// an unenforced memory ceiling is exactly the state #2919 describes, and it is
// worse than no ceiling because it looks like one.
function memoryGuardedEnv(budgetSeconds, ledger) {
  const guard = resolve(dirname(fileURLToPath(import.meta.url)), 'mutant-memory-guard.mjs');
  if (/\s/.test(guard)) {
    throw new Error(`the memory guard's path contains whitespace, which GOFLAGS cannot express: ${guard}`);
  }
  if (!existsSync('/proc/self/status')) {
    throw new Error('no /proc: mutant-memory-guard.mjs cannot bound a mutant\'s memory on this platform');
  }
  const goFlags = String(process.env.GOFLAGS ?? '').trim();
  return {
    ...process.env,
    GOFLAGS: `${goFlags === '' ? '' : `${goFlags} `}-exec=${guard}`,
    MUTANT_MEMORY_MAX: perMutantResidentMemoryMax,
    MUTANT_MEMORY_HOLD: `${budgetSeconds + perMutantMemoryHoldGraceSeconds}s`,
    MUTANT_MEMORY_LEDGER: ledger,
  };
}

// reportMemoryBreaches turns the guard's ledger into the one log line a reader
// needs: how many mutants hit the ceiling, and how far past it they got before
// they were stopped. Reported as a ::notice::, not an error — a breach is the
// bound WORKING, and the mutant it stopped is already counted against the leg's
// efficacy as TIMED OUT.
function reportMemoryBreaches(ledger) {
  if (!existsSync(ledger)) return;
  const breaches = readFileSync(ledger, 'utf8')
    .split('\n')
    .filter((line) => line.trim() !== '')
    .map((line) => JSON.parse(line));
  if (breaches.length === 0) return;
  const worst = Math.max(...breaches.map((b) => b.resident_bytes));
  notice(
    `${breaches.length} mutant(s) hit the ${perMutantResidentMemoryMax} per-mutant memory ceiling and ` +
      `were stopped there (worst observed ${(worst / 1024 ** 2).toFixed(0)}MiB); each is recorded as ` +
      'TIMED OUT — unadjudicated, counted in the denominator, credited to nobody',
  );
}

function runGremlins({ report, budgetSeconds, ledger, diffRef = '' }) {
  const args = [
    'unleash',
    '--output',
    report,
    '--on-shutdown-status=not-run',
    '--timeout-max',
    `${budgetSeconds}s`,
    '--timeout-coefficient',
    perMutantBudgetIsTheCeilingCoefficient(budgetSeconds),
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

  const result = spawnSync('gremlins', args, {
    stdio: 'inherit',
    env: memoryGuardedEnv(budgetSeconds, ledger),
  });
  if (result.error) throw new Error(`cannot execute gremlins: ${result.error.message}`);
  if (result.status !== 0) throw new Error(`gremlins exited ${result.status}`);
  if (!existsSync(report)) throw new Error(`gremlins produced no report at ${report}`);
}

function main() {
  const report = required('REPORT');
  const scope = required('SCOPE');
  const diffRef = String(process.env.DIFF_REF ?? '').trim();
  if (diffRef !== '' && !/^[0-9a-f]{40,64}$/i.test(diffRef)) {
    throw new Error(`DIFF_REF is invalid: ${diffRef}`);
  }
  if (existsSync(report)) throw new Error(`refusing stale gremlins report ${report}`);

  // Measured once, before either invocation, and reused. Both invocations run
  // the same mutants on the same runner, so a second measurement would only add
  // a second compile — and would take it against a cache the first invocation
  // warmed, which is the exact mismeasurement #2692 was.
  const budgetSeconds = perMutantBudgetSeconds(scope);
  // One ledger for both invocations: a breach is a property of the leg, not of
  // which of the two gremlins runs happened to draw the mutant.
  const ledger = resolve(`${report}.memory-breaches.jsonl`);
  if (existsSync(ledger)) unlinkSync(ledger);

  runGremlins({ report, budgetSeconds, ledger, diffRef });
  if (diffRef !== '' && reportTotal(report) === 0) {
    notice('changed-line mutation found zero executable mutants; rerunning the full phase');
    unlinkSync(report);
    runGremlins({ report, budgetSeconds, ledger });
  }
  reportMemoryBreaches(ledger);
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
