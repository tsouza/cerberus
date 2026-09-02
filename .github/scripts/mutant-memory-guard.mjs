#!/usr/bin/env node
// mutant-memory-guard.mjs — a `go test -exec` supervisor that bounds ONE
// mutant's resident memory, so an allocating non-terminating mutant is RECORDED
// instead of reaping the runner.
//
// Required env: MUTANT_MEMORY_MAX (a Go-style byte size, e.g. `1GiB`),
//               MUTANT_MEMORY_HOLD (a Go duration, e.g. `150s`),
//               MUTANT_MEMORY_LEDGER (a path this script APPENDS one JSON line
//               to per breach).
// argv: the test binary and its arguments, exactly as `go test -exec` passes
//       them.
//
// The ledger is how a breach becomes visible. `go test` buffers a test binary's
// output and prints it only when the package FAILS, so a ::notice:: written to
// stdout from here is discarded on exactly the path this guard creates — the
// child is killed, the package is never reported as failing, and the annotation
// would vanish. mutation-run.mjs reads the ledger after gremlins exits and
// reports every breach itself.
//
// WHY A SEPARATE BOUND EXISTS AT ALL
// ----------------------------------
// gremlins leashes each mutant with ONE number — a context deadline wrapping
// the whole `go test` child (resolve, compile, link, then run). That deadline
// is the lane's only bound, and .github/scripts/mutation-run.mjs derives it
// from the leg's measured recompile+link+run cycle, because the compile is what
// dominates it (#2903).
//
// A wall-clock bound does not bound ALLOCATION. Measured on
// `internal/logql/logpattern`'s REMOVE_SELF_ASSIGNMENTS mutant at
// pattern.go:129 (`i += size` -> `i = size`, which pins the scanner index and
// appends a rune per iteration for ever), the mutated test binary reaches
// 5 GiB of RSS in 3.3 s — about 1.5 GiB/s. So on a 16 GB runner the mutant
// exhausts the machine in roughly ten seconds, BELOW the lane's 15 s budget
// floor and far below the ~63 s its compile cycle requires. There is no leash
// value that is both long enough for the compile and short enough for the
// memory: the window is empty, not merely narrow (#2919).
//
// GOMEMLIMIT does not close it either. It is a soft limit, and Go's GC CPU
// limiter guarantees the mutator at least half the CPU, so a mutant whose heap
// is LIVE keeps growing. Measured on the same binary: 5 GiB in 8.5 s at
// GOMEMLIMIT=512MiB against 3.3 s unbounded — a 2.6x slowdown, not a bound.
//
// So the bound has to be memory itself, applied to the process that allocates.
//
// WHAT HAPPENS WHEN THE BOUND IS BREACHED
// ---------------------------------------
// The child is killed immediately, which returns its pages to the runner, and
// the guard then HOLDS — it does not exit. That is deliberate, and it is the
// only honest verdict this script can produce:
//
//   - gremlins reads a mutant's outcome from the `go test` EXIT CODE (1 ->
//     KILLED, 2 -> NOT VIABLE, anything else -> LIVED) or from its own deadline
//     firing (-> TIMED OUT). `go test` collapses every test-binary failure to
//     its own exit 1, so ANY exit the guard forces after killing the child
//     would be recorded as a KILL — crediting the suite for a mutant it never
//     adjudicated, which is exactly the accounting #2903 removed.
//   - Exiting 0 would record LIVED, which gremlins' own --on-shutdown-status
//     work rejected for unadjudicated mutants: "reporting these as LIVED
//     misrepresents the data — they were never tested".
//   - Holding lets gremlins' existing deadline fire and record TIMED OUT, which
//     .github/scripts/gremlins-threshold.mjs already scores as UNADJUDICATED:
//     counted in the denominator, credited to nobody.
//
// The hold costs no wall clock the lane was not already paying: the mutants
// that trip this bound are the mutants that were already burning their whole
// budget before dying. What changes is the peak RSS while they do it.
//
// MUTANT_MEMORY_HOLD is a backstop for the one path with no deadline behind it
// — gremlins' initial coverage run, which is UNMUTATED and so cannot trip the
// bound, but must not be able to wedge the job if that ever stops being true.
// On expiry the guard exits 0 (LIVED, a survivor). The ordering is deliberate:
// prefer TIMED OUT, fall back to LIVED, never KILLED.

import { spawn } from 'node:child_process';
import { appendFileSync, readFileSync } from 'node:fs';
import process from 'node:process';

import { error, notice } from './lib/gh.mjs';

// residentSampleIntervalMs is how often the child's RSS is read from
// /proc/<pid>/status. It bounds the OVERSHOOT past MUTANT_MEMORY_MAX: at the
// 1.5 GiB/s allocation rate measured above, a 50 ms gap lets a runaway add at
// most ~77 MiB after crossing the line. Reading one small procfs file twenty
// times a second is not measurable against a `go test` child.
const residentSampleIntervalMs = 50;

// holdPollIntervalMs is how often the hold loop re-checks its own deadline.
// Nothing observes it but the deadline itself, so it is coarse on purpose.
const holdPollIntervalMs = 250;

const byteUnitMultipliers = {
  B: 1,
  KiB: 1024,
  MiB: 1024 ** 2,
  GiB: 1024 ** 3,
  KB: 1000,
  MB: 1000 ** 2,
  GB: 1000 ** 3,
};

const goDurationUnitSeconds = { ns: 1e-9, us: 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };

function required(name) {
  const value = String(process.env[name] ?? '').trim();
  if (value === '') throw new Error(`${name} is required`);
  return value;
}

// Parses `<number><unit>` with an explicit unit — no bare byte counts, because
// a limit whose unit has to be guessed is a limit nobody can review.
export function byteSize(name, spec) {
  const match = /^([0-9]+(?:\.[0-9]+)?)(B|KiB|MiB|GiB|KB|MB|GB)$/.exec(spec);
  if (match === null) {
    throw new Error(`${name} is not a byte size (e.g. 1GiB): ${spec}`);
  }
  const bytes = Number(match[1]) * byteUnitMultipliers[match[2]];
  if (!(bytes > 0)) throw new Error(`${name} must be positive: ${spec}`);
  return bytes;
}

// Parses the subset of Go's duration grammar a hold bound can use: one or more
// unsigned `<number><unit>` terms, e.g. `150s` or `2m30s`.
export function goDurationSeconds(name, spec) {
  const terms = spec.match(/[0-9]+(?:\.[0-9]+)?(?:ns|us|ms|s|m|h)/g);
  if (terms === null || terms.join('') !== spec) {
    throw new Error(`${name} is not a Go duration: ${spec}`);
  }
  const seconds = terms.reduce((total, term) => {
    const [, value, unit] = /^([0-9]+(?:\.[0-9]+)?)([a-z]+)$/.exec(term);
    return total + Number(value) * goDurationUnitSeconds[unit];
  }, 0);
  if (!(seconds > 0)) throw new Error(`${name} must be a positive duration: ${spec}`);
  return seconds;
}

// residentBytes reads VmRSS for a live pid. A pid whose procfs entry is gone
// has exited — a race with the exit handler, not a failure — so it reports 0
// rather than throwing.
export function residentBytes(pid, readStatus = (p) => readFileSync(`/proc/${p}/status`, 'utf8')) {
  let status;
  try {
    status = readStatus(pid);
  } catch {
    return 0;
  }
  const match = /^VmRSS:\s+([0-9]+)\s+kB$/m.exec(status);
  return match === null ? 0 : Number(match[1]) * 1024;
}

function formatMiB(bytes) {
  return `${(bytes / 1024 ** 2).toFixed(0)}MiB`;
}

function main() {
  const limitBytes = byteSize('MUTANT_MEMORY_MAX', required('MUTANT_MEMORY_MAX'));
  const holdSeconds = goDurationSeconds('MUTANT_MEMORY_HOLD', required('MUTANT_MEMORY_HOLD'));
  const ledger = required('MUTANT_MEMORY_LEDGER');
  const [binary, ...args] = process.argv.slice(2);
  if (binary === undefined) throw new Error('no test binary to run: this script is a `go test -exec` wrapper');

  const child = spawn(binary, args, { stdio: 'inherit' });
  let breached = false;

  const sampler = setInterval(() => {
    if (child.pid === undefined) return;
    const rss = residentBytes(child.pid);
    if (rss <= limitBytes) return;
    breached = true;
    clearInterval(sampler);
    child.kill('SIGKILL');
    // Recorded BEFORE the hold, because the hold normally ends in this process
    // being killed by gremlins' deadline and never running another line.
    appendFileSync(
      ledger,
      `${JSON.stringify({ binary, resident_bytes: rss, limit_bytes: limitBytes })}\n`,
    );
    notice(
      `mutant memory bound breached: ${binary} reached ${formatMiB(rss)} against a ` +
        `${formatMiB(limitBytes)} ceiling; killed, and holding so gremlins' own deadline records it ` +
        'as TIMED OUT (unadjudicated) rather than as a kill',
    );
  }, residentSampleIntervalMs);

  child.on('error', (cause) => {
    clearInterval(sampler);
    error(`cannot execute ${binary}: ${cause.message}`);
    process.exit(1);
  });

  child.on('exit', (code, signal) => {
    clearInterval(sampler);
    if (!breached) {
      // Faithful passthrough: an unbreached mutant must reach gremlins exactly
      // as it would have without this wrapper in the path.
      process.exit(code === null ? 128 + (signal === null ? 0 : 1) : code);
    }
    const deadline = Date.now() + holdSeconds * 1000;
    const holder = setInterval(() => {
      if (Date.now() < deadline) return;
      clearInterval(holder);
      notice(
        `held ${holdSeconds}s after the memory bound was breached and no deadline reaped this ` +
          'process; exiting 0, which records the mutant as LIVED (a survivor, never a kill)',
      );
      process.exit(0);
    }, holdPollIntervalMs);
  });
}

// Importing this module for its parsers must not run a supervisor.
if (process.argv[1] !== undefined && process.argv[1].endsWith('mutant-memory-guard.mjs')) {
  try {
    main();
  } catch (cause) {
    error(cause instanceof Error ? cause.message : String(cause));
    process.exit(1);
  }
}
