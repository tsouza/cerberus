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
//
// Exits 1 on a missing libchdb.so, on any leg failing, or on the seal step
// reporting a tree that does not match the corpus.

import { existsSync } from 'node:fs';
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
 * The regeneration plan: N profiling legs over disjoint slices, then the seal.
 *
 * The leg INDEX is 1-based and the legs are contiguous 1..N, because that is
 * what the partition on the Go side divides by: an index outside [1, N] names a
 * slice that does not exist, so that leg regenerates nothing while the slice it
 * should have owned is owned by nobody — and every leg still exits 0.
 */
function plan(count, timeout) {
  const legs = Array.from({ length: count }, (_, i) => ({
    label: `cardinality[${i + 1}/${count}]`,
    argv: goTest(RATCHET_TEST, timeout),
    env: {
      [UPDATE_ENV]: '1',
      [PERF_SHARD_INDEX_ENV]: String(i + 1),
      [PERF_SHARD_COUNT_ENV]: String(count),
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
