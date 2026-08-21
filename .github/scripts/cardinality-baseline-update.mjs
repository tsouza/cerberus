// cardinality-baseline-update.mjs — the runner behind
// `just update-cardinality-baseline`.
//
// Regenerates test/perf/cardinality-baseline/ by fanning the profile pass out
// across N processes over disjoint slices of the corpus, then asserting the
// slices together covered it.
//
// # Why a fan-out at all
//
// `TestCardinalityRatchet`'s update path profiles every executable TXTAR fixture
// through chDB — EXPLAIN plus a per-subquery-level count() decomposition each —
// and its runtime is a straight line in corpus size. At ~950 fixtures that is a
// multi-minute-to-tens-of-minutes serial pass a maintainer has to sit through
// whenever a fixture is added, removed or re-lowered (#2122).
//
// It cannot be parallelised inside the process: profile.NewProfiler calls
// chdb-go's package-level chdb.NewSession, which caches one session for the
// whole process regardless of DSN and is not safe for concurrent use (#1987).
// So the parallelism is N separate OS processes, exactly as CI's
// `perf-guards-shard` matrix already runs the GATING pass.
//
// # Why the legs cannot corrupt each other
//
// Writing the baseline prunes rows the profile no longer produced, so a leg that
// profiled 1/N of the corpus and pruned the whole tree would delete the other
// legs' rows. baselineShards.writeShard (test/perf/baseline_shards_test.go)
// scopes the prune to the leg's own slice — a leg deletes a stale file only when
// the file's key hashes to the leg's index — so leg i's blast radius is exactly
// the paths leg i is also the only writer of. Legs touch disjoint path sets, and
// their union at count N is byte-for-byte the single serial pass.
//
// # Why the seal step exists
//
// Safety per leg is not completeness across legs: no leg can see whether its
// siblings ran. A leg that was never dispatched leaves its slice at whatever the
// last regeneration wrote, which is a tree that still parses and still looks
// regenerated. The closing step re-derives the corpus roster (a TXTAR walk, no
// profiling) and asserts the committed tree holds exactly one row per profilable
// fixture, so an absent or crashed leg is a named missing id rather than a quiet
// staleness. That check is also what makes re-running the fan-out at a DIFFERENT
// N than last time safe to reason about: N legs at count N always partition the
// whole key space, whatever N was before, and the seal proves it rather than
// assuming it.
//
// Env inputs:
//   CHDB_INSTALL_PATH               (optional) libchdb.so path, checked first.
//   CARDINALITY_BASELINE_TIMEOUT    (optional) per-leg `go test -timeout`.
//   CARDINALITY_BASELINE_FANOUT     (optional) leg count, for measuring the
//                                   split on a machine shaped unlike the one
//                                   BASELINE_FANOUT was chosen on. The plan
//                                   printed without it is what the regression
//                                   pin reads.
//   CARDINALITY_UPDATE_PRINT_PLAN   (optional) `1` prints the ordered plan
//                                   (`<label> <ENV=v>... <command>` per line)
//                                   and exits without regenerating anything.
//   GOMAXPROCS                      (optional) overrides the per-leg default
//                                   of `hostCores / fanOut` (see
//                                   perLegGOMAXPROCS) — set this yourself only
//                                   if you deliberately want every leg
//                                   scheduling against the full core count
//                                   again.
//
// Exits 1 on a missing libchdb.so, on any leg failing, or on the seal step
// reporting a tree that does not match the corpus.
//
// # MODE — CI matrix jobs, not local processes
//
// Everything above is the LOCAL entrypoint behind `just update-cardinality-
// baseline`: N processes fanned out on one machine, in one job. #2341 adds a
// second entrypoint that runs the identical partition across separate CI
// MATRIX JOBS (their own runner each), because a process fan-out inside one
// job still pays that job's wall-clock as ONE number — mirroring how
// mutation.yml splits gremlins into `phase*` matrix jobs rather than N
// processes inside one `mutate` job.
//
//   MODE=leg   run exactly ONE leg — the same `go test -run TestCardinality
//              Ratchet` a local process leg runs, with
//              UPDATE_CARDINALITY_BASELINE=1 — against a CI-supplied
//              PERF_SHARD_INDEX / PERF_SHARD_COUNT (required). Unlike a local
//              leg, GOMAXPROCS is left untouched: the CI runner is dedicated
//              to this one process, so there is no sibling to divide cores
//              against (perLegGOMAXPROCS is a LOCAL multi-process concern).
//
//   MODE=seal  run TestCardinalityBaselineCoversTheCorpus once — the same
//              closing assertion the local seal step runs — against whatever
//              is already on disk. In CI this is the tree produced by
//              downloading and applying every leg's own patch first (see
//              update-golden.yml's `cardinality-seal` job): no single leg can
//              see whether its siblings ran, so this is what turns "every
//              matrix job exited 0" into "the legs together covered the
//              corpus" — the cross-JOB analogue of the local seal step, and
//              the thing #2341 asked to relocate.
//
// Both modes inherit stdio (one process, one runner, nothing to tag) and exit
// with the `go test` child's own status code.

import { spawnSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import os from 'node:os';
import process from 'node:process';

import { error, log } from './lib/gh.mjs';
import { runAllTagged } from './lib/spawn-tagged.mjs';

/**
 * How many processes the regeneration fans out to.
 *
 * The same count `perf-guards-shard` fans the GATING pass out to in
 * .github/workflows/chdb.yml, and the count test/perf/profile/shard_test.go's
 * `productionShardCount` asserts the partition's total-cover and balance
 * properties at. Reusing it is not just tidiness: those two properties are the
 * evidence that N legs divide the corpus into N non-empty, roughly equal pieces,
 * and they are evidence about THIS fan-out only while the counts agree —
 * test/regression/cardinality_baseline_fanout_test.go pins them together from
 * the Go side.
 *
 * The two counts do not HAVE to match for correctness: every key belongs to
 * exactly one leg at any N, so N legs at count N always cover the tree, whatever
 * N the tree was last written at. Matching is what keeps the balance floor
 * measured at a split somebody actually runs.
 *
 * What this asks of the host is N concurrent in-process ClickHouse engines —
 * each leg stands up its own chdb session, where the serial pass stood up one.
 * A host too small for that loses a leg to the OOM killer, which fails loudly
 * and is safe to recover from: a leg only ever rewrites its own slice, so
 * re-running the recipe is idempotent. CARDINALITY_BASELINE_FANOUT is the lever
 * for a smaller machine.
 */
const BASELINE_FANOUT = 8;

/** Per-leg `go test -timeout` when the recipe passes none. */
const DEFAULT_LEG_TIMEOUT = '60m';

/** The env pair a leg declares its corpus slice through, read by
 * profile.ShardFromEnv (test/perf/profile/shard.go). Deliberately NOT the spec
 * lane's SPEC_SHARD_* pair: the two partitions cover different corpora at
 * different counts, and one shared pair would let either narrow the other. */
const PERF_SHARD_INDEX_ENV = 'PERF_SHARD_INDEX';
const PERF_SHARD_COUNT_ENV = 'PERF_SHARD_COUNT';

/** The env var that switches the ratchet from asserting to regenerating. */
const UPDATE_ENV = 'UPDATE_CARDINALITY_BASELINE';

const RATCHET_TEST = '^TestCardinalityRatchet$';
const SEAL_TEST = '^TestCardinalityBaselineCoversTheCorpus$';
const PERF_PKG = './test/perf/';

/** True when `n` is a positive integer written as a base-10 string. */
function isPositiveIntegerString(n) {
  return /^[1-9][0-9]*$/.test(String(n ?? ''));
}

/** Reads and validates one required positive-integer env var for MODE=leg. */
function requiredPositiveInt(name) {
  const raw = process.env[name];
  if (!isPositiveIntegerString(raw)) {
    error(`${name}=${JSON.stringify(raw)} must be a positive integer`);
    process.exit(1);
  }
  return Number.parseInt(raw, 10);
}

/**
 * Runs one `go test` invocation to completion with inherited stdio, and exits
 * this process with its status. There is exactly one child and nothing else
 * running concurrently on this runner, so there is nothing to tag or buffer —
 * unlike runAllTagged's local multi-process fan-out, streaming straight
 * through is both simpler and loses nothing.
 */
function runToCompletion(argv, env, timeout) {
  const chdb = process.env.CHDB_INSTALL_PATH;
  if (chdb && !existsSync(chdb)) {
    error(
      `${chdb} not found — run 'just chdb-install' first; the cardinality package is chdb-tagged ` +
        'and its test binary cannot even start without the shared library on the runner.',
    );
    process.exit(1);
  }
  log(`==> ${Object.entries(env)
    .map(([k, v]) => `${k}=${v}`)
    .join(' ')} ${argv.join(' ')}`.replace(/\s+/g, ' '));
  const result = spawnSync(argv[0], argv.slice(1), {
    cwd: repoRoot,
    stdio: 'inherit',
    env: { ...process.env, ...env },
  });
  if (result.error) {
    error(`cannot execute ${argv.join(' ')}: ${result.error.message}`);
    process.exit(1);
  }
  process.exit(result.status === null ? 1 : result.status);
}

/**
 * MODE=leg — one CI matrix job's slice of the profiling pass.
 *
 * PERF_SHARD_INDEX / PERF_SHARD_COUNT come from the caller (a matrix entry
 * and `strategy.job-total` in update-golden.yml's `cardinality-leg` job),
 * not from CARDINALITY_BASELINE_FANOUT — that env var is the LOCAL override,
 * and the two are validated against each other only through the Go-side pin
 * (test/regression), never merged into one input here.
 *
 * GOMAXPROCS is deliberately left alone: perLegGOMAXPROCS divides the host's
 * core count across N SIBLING processes sharing one machine, which is exactly
 * what a CI matrix job does NOT have — it owns its runner outright.
 */
function runCiLeg() {
  const index = requiredPositiveInt(PERF_SHARD_INDEX_ENV);
  const count = requiredPositiveInt(PERF_SHARD_COUNT_ENV);
  if (index > count) {
    error(`${PERF_SHARD_INDEX_ENV}=${index} exceeds ${PERF_SHARD_COUNT_ENV}=${count}`);
    process.exit(1);
  }
  const timeout = process.env.CARDINALITY_BASELINE_TIMEOUT || DEFAULT_LEG_TIMEOUT;
  runToCompletion(goTest(RATCHET_TEST, timeout), {
    [UPDATE_ENV]: '1',
    [PERF_SHARD_INDEX_ENV]: String(index),
    [PERF_SHARD_COUNT_ENV]: String(count),
  });
}

/**
 * MODE=seal — the cross-job coverage closure. Run once, after every leg's
 * patch has been downloaded and applied to this checkout (update-golden.yml's
 * `cardinality-seal` job) — the exact assertion the local seal step runs, just
 * relocated from "after every sibling PROCESS finished" to "after every
 * sibling JOB finished". Asserts against the corpus roster alone (no chDB
 * session, though the package still needs the chdb tag to compile at all), so
 * it does not need UPDATE_CARDINALITY_BASELINE — it only ever reads.
 */
function runCiSeal() {
  const timeout = process.env.CARDINALITY_BASELINE_TIMEOUT || DEFAULT_LEG_TIMEOUT;
  runToCompletion(goTest(SEAL_TEST, timeout), {});
}

const repoRoot = process.cwd();

function fanOut() {
  const raw = process.env.CARDINALITY_BASELINE_FANOUT;
  if (!raw) return BASELINE_FANOUT;
  const n = Number.parseInt(raw, 10);
  if (!Number.isInteger(n) || n < 1) {
    error(`CARDINALITY_BASELINE_FANOUT=${raw} is not a positive integer`);
    process.exit(1);
  }
  return n;
}

function goTest(run, timeout) {
  return ['go', 'test', '-tags', 'chdb', '-count=1', '-timeout', timeout, '-run', run, PERF_PKG];
}

/**
 * Per-leg GOMAXPROCS: the host's core count divided evenly across the N
 * concurrent legs, floored at 1.
 *
 * Each leg is a separate `go test` process, and the Go runtime defaults an
 * unset GOMAXPROCS to the FULL host core count regardless of how many
 * sibling processes are already competing for it. N legs left at that
 * default therefore each try to schedule as if they owned every core, which
 * oversubscribes an N-core-or-smaller host by a factor of N — measured on an
 * 8-core dev box running the default 8-way fan-out as ~50% CPU utilisation
 * per leg instead of ~100%, roughly doubling wall time. Dividing the core
 * count up front is what the fan-out itself already assumes (`count` legs
 * partitioning `count`-ways), so this just makes the scheduler's per-process
 * budget agree with that assumption instead of over-committing it.
 *
 * A caller that has deliberately set GOMAXPROCS in its own environment (a
 * CI runner tuned for a different oversubscription strategy, say) is left
 * alone — this only fills the gap the Go runtime's own default leaves.
 */
function perLegGOMAXPROCS(count) {
  return String(Math.max(1, Math.floor(os.cpus().length / count)));
}

/**
 * The regeneration plan: N profiling legs over disjoint slices, then the seal.
 *
 * The leg INDEX is 1-based and the legs are contiguous 1..N, because that is
 * what the partition on the Go side divides by: an index outside [1, N] names a
 * slice that does not exist, so that leg regenerates nothing while the slice it
 * should have owned is owned by nobody — and every leg still exits 0.
 */
function plan(count, timeout) {
  const gomaxprocs = process.env.GOMAXPROCS || perLegGOMAXPROCS(count);
  const legs = Array.from({ length: count }, (_, i) => ({
    label: `cardinality[${i + 1}/${count}]`,
    argv: goTest(RATCHET_TEST, timeout),
    env: {
      [UPDATE_ENV]: '1',
      [PERF_SHARD_INDEX_ENV]: String(i + 1),
      [PERF_SHARD_COUNT_ENV]: String(count),
      GOMAXPROCS: gomaxprocs,
    },
  }));

  const seal = {
    label: 'cardinality[seal]',
    argv: goTest(SEAL_TEST, timeout),
    env: {},
  };

  return { legs, seal };
}

function planLine({ label, argv, env }) {
  const vars = Object.entries(env)
    .map(([k, v]) => `${k}=${v}`)
    .join(' ');
  return `${label} ${vars} ${argv.join(' ')}`.replace(/\s+/g, ' ');
}

async function main() {
  const mode = (process.env.MODE || '').trim();
  if (mode === 'leg') return runCiLeg();
  if (mode === 'seal') return runCiSeal();
  if (mode !== '') {
    error(`cardinality-baseline-update: MODE must be "leg", "seal", or unset (got ${JSON.stringify(mode)})`);
    process.exit(1);
  }

  const timeout = process.env.CARDINALITY_BASELINE_TIMEOUT || DEFAULT_LEG_TIMEOUT;
  const { legs, seal } = plan(fanOut(), timeout);

  if (process.env.CARDINALITY_UPDATE_PRINT_PLAN === '1') {
    for (const step of [...legs, seal]) log(planLine(step));
    return;
  }

  const chdb = process.env.CHDB_INSTALL_PATH;
  if (chdb && !existsSync(chdb)) {
    error(
      `${chdb} not found — run 'just chdb-install' first; the ratchet baseline is generated by an ` +
        'in-process chDB profile pass',
    );
    process.exit(1);
  }

  for (const step of legs) log(`==> ${planLine(step)}`);
  const legCode = await runAllTagged(legs, repoRoot, log);
  if (legCode !== 0) {
    error(
      'a cardinality baseline leg failed — the tree now holds a MIX of freshly profiled and ' +
        'previously committed rows. Re-run the recipe once the failure is fixed; a leg only ever ' +
        'rewrites its own slice, so re-running is safe and idempotent.',
    );
    process.exit(legCode);
  }

  log(`==> ${planLine(seal)}`);
  const sealCode = await runAllTagged([seal], repoRoot, log);
  if (sealCode !== 0) {
    error(
      'the regenerated baseline does not match the corpus — see the missing/orphaned fixture ids ' +
        'above. Every leg reported success, so this is a tree that was not fully covered rather ' +
        'than a profiling failure.',
    );
    process.exit(sealCode);
  }
}

await main();
