// chdb-roundtrip.mjs — runs one head's `roundtrip (<ql>)` leg of chdb.yml:
// the chDB-executing TXTAR walk over `./test/spec/<ql>/` (pre-optimizer,
// TestRoundTripChDB) and `./internal/<ql>/` (post-optimizer RunRoundTripSQL +
// the reference-engine RunParity, TestLower).
//
// Why this is a script rather than the one-line `go test` it replaces
// ---------------------------------------------------------------------------
// Two facts about that `go test` were invisible in YAML and cost a red lane:
//
//   1. NO EXPLICIT `-timeout`. `go test` applies its own DEFAULT 10-minute
//      watchdog PER TEST BINARY, entirely independently of the job's
//      `timeout-minutes: 20`. The promql leg had been finishing in 550-600s of
//      that 600s budget for weeks (job wall-clock 648-683s on main), so it was
//      one slow runner away from failing — and then did, on run
//      31710525983: `FAIL github.com/tsouza/cerberus/internal/promql 600.049s`,
//      with the panic's own header naming a subtest that had been running for
//      ONE SECOND. That is a walk still making forward progress when the
//      alarm fired, not a hang: it had reached fixture 475 of 582 and needed
//      roughly 735s. The lane was measuring the watchdog, not the code.
//
//   2. ONE PROCESS. The promql corpus is 582 fixtures against logql's 189 and
//      traceql's 222, and the internal/<ql> walk is the expensive half of the
//      two (it seeds chDB, executes the post-optimizer SQL, and answers the
//      parity-enrolled fixtures with a real Prometheus TSDB engine). The leg's
//      wall-clock therefore grows with every fixture merged, and raising a
//      number would only move the next failure out by a few months.
//
// Both are fixed here rather than in YAML because the fix is a fan-out with a
// failure-propagation contract, which is exactly the "non-trivial step logic"
// invariant 15 keeps out of workflow files.
//
// The partition is the one that already exists: spec.ShardFromEnv
// (test/spec/shard.go) reads SPEC_SHARD_INDEX / SPEC_SHARD_COUNT, and both
// walks this leg runs honour it. `just update-golden promql` fans the
// round-trip generator out over the same pair
// (.github/scripts/lib/golden-shards.mjs), where it was MEASURED at 629s in
// one process against 231s at four legs.
//
// Env:
//   QL       head name — `promql` | `logql` | `traceql`. Required.
//   TAGS     go build tags for the leg (`chdb`, or the AGPL-oracle triple for
//            logql/traceql). Required.
//   GO       go executable; default `go`. Test seam.
//
// Exit: 0 when every leg passed; 1 on a failed leg, a bad QL, or a missing env.
//
// node: builtins only — no npm deps, no setup-node needed.

import process from 'node:process';
import { error, notice, log, group } from './lib/gh.mjs';
import { runLegBuffered } from './lib/spawn-tagged.mjs';

/**
 * How many PROCESSES each head's leg fans out to.
 *
 * A GitHub-hosted `ubuntu-latest` runner has 4 cores, and each leg runs its two
 * packages with `-p 1` (below), so the fan-out count IS the number of test
 * binaries competing for those cores — no hidden second multiplier.
 *
 * promql is the only head that needs one. Its leg takes ~700s where logql's and
 * traceql's whole JOBS — checkout, toolchain, libchdb install and all — finish
 * in 173-181s, so those two have an order of magnitude of headroom and a
 * fan-out would buy them nothing but N-times-repeated compilation of the
 * non-corpus tests in the same packages.
 *
 * Three rather than four for promql, because chDB threads WITHIN a single query
 * as well: a leg already uses more than one core, so a fan-out matching the core
 * count oversubscribes rather than spreads. That is the same effect measured on
 * the update-golden fan-out, where 8 legs on an 8-core box (252s) came out
 * WORSE than 4 (231s) — the useful margin is over the serial walk, not at the
 * top of the range. Three legs leave a core for the runner's own work and still
 * cut the walk to roughly a third.
 */
const FANOUT = {
  promql: 3,
  logql: 1,
  traceql: 1,
};

/**
 * The per-process `go test -timeout`, in minutes.
 *
 * It has one job the job-level `timeout-minutes` cannot do: when a walk really
 * does wedge in a libchdb downcall, `go test`'s own alarm panics with the full
 * goroutine dump that names the wedged frame — the artefact #2096 was diagnosed
 * from. A runner-level kill produces no such dump, so this bound MUST stay
 * strictly below chdb.yml's `timeout-minutes`, and
 * chdb-roundtrip.test.mjs asserts that ordering against the workflow file.
 *
 * Twelve minutes is ~5x the ~250s a fanned-out promql leg should take and ~2x
 * the ~700s an UNFANNED leg takes today, so it stays a hang detector rather
 * than a throughput ceiling even if the fan-out is reduced to 1, while leaving
 * the 20-minute job cap several minutes of room to publish the dump.
 */
export const GO_TEST_TIMEOUT_MINUTES = 12;

/** The env pair spec.ShardFromEnv reads. Contract with test/spec/shard.go. */
const SPEC_SHARD_INDEX_ENV = 'SPEC_SHARD_INDEX';
const SPEC_SHARD_COUNT_ENV = 'SPEC_SHARD_COUNT';

/** Heads this lane knows how to run. */
export const HEADS = Object.keys(FANOUT);

/**
 * The commands one head's leg runs, fan-out expanded.
 *
 * A fan-out of 1 declares NO shard env at all rather than `1/1`: half a
 * declared partition is an error on the Go side, and a whole-corpus leg should
 * reach spec.WalkShard through the same unset-means-everything path a
 * hand-typed `go test` does.
 *
 * `-p 1` runs the leg's two packages one after the other inside the leg instead
 * of concurrently, so N legs mean exactly N test binaries. Without it `go test`
 * would run both packages of every leg at once and the real concurrency would
 * be 2N — double what the fan-out count says, and past the core count.
 */
export function legCommands({ ql, tags, go = 'go' }) {
  const fanOut = FANOUT[ql];
  const argv = [
    go,
    'test',
    '-tags',
    tags,
    '-count=1',
    '-p',
    '1',
    `-timeout=${GO_TEST_TIMEOUT_MINUTES}m`,
    `./test/spec/${ql}/...`,
    `./internal/${ql}/...`,
  ];
  if (fanOut <= 1) return [{ name: ql, argv, env: {} }];
  return Array.from({ length: fanOut }, (_, i) => ({
    name: `${ql} ${i + 1}/${fanOut}`,
    argv,
    env: {
      [SPEC_SHARD_INDEX_ENV]: String(i + 1),
      [SPEC_SHARD_COUNT_ENV]: String(fanOut),
    },
  }));
}

/**
 * The build-only pass that runs BEFORE a fan-out, never as part of it.
 *
 * `-run=^$` matches no test, so this compiles and links both of the head's test
 * binaries and executes nothing. Without it every leg of a fan-out starts by
 * compiling the SAME two binaries at the same time: Go's build cache dedupes
 * the stored output, not the concurrent work, so N legs on a cold runner cache
 * do the whole ~75s compile N times over the same cores before any fixture
 * runs. Paying it once, serially, hands every leg a warm cache.
 *
 * A fan-out of 1 needs none of this — its single leg IS the one compile.
 */
export function warmupCommand({ ql, tags, go = 'go' }) {
  if (FANOUT[ql] <= 1) return null;
  return {
    name: `${ql} build`,
    argv: [go, 'test', '-tags', tags, '-count=1', '-p', '1', '-run=^$', `./test/spec/${ql}/...`, `./internal/${ql}/...`],
    env: {},
  };
}

async function main() {
  const ql = process.env.QL ?? '';
  const tags = process.env.TAGS ?? '';
  if (!HEADS.includes(ql)) {
    error(`QL must be one of ${HEADS.join(', ')} (got ${JSON.stringify(ql)})`);
    process.exit(1);
  }
  if (tags === '') {
    error('TAGS is required — the leg would compile without the chdb driver and walk nothing');
    process.exit(1);
  }

  const go = process.env.GO ?? 'go';
  const legs = legCommands({ ql, tags, go });
  notice(`roundtrip ${ql}: ${legs.length} leg(s), ${GO_TEST_TIMEOUT_MINUTES}m per-process timeout`);

  const started = Date.now();

  const warmup = warmupCommand({ ql, tags, go });
  if (warmup) {
    const built = await runLegBuffered(warmup);
    if (built.code !== 0) {
      group(`${warmup.name} (exit ${built.code})`, () => log(built.out.trimEnd()));
      error(`roundtrip ${ql}: the leg binaries did not build`);
      process.exit(1);
    }
  }

  const results = await Promise.all(legs.map(runLegBuffered));
  const elapsedSeconds = Math.round((Date.now() - started) / 1000);

  for (const r of results) {
    group(`${r.leg.name} (exit ${r.code})`, () => log(r.out.trimEnd()));
  }

  const failed = results.filter((r) => r.code !== 0);
  if (failed.length > 0) {
    error(`roundtrip ${ql}: ${failed.length} of ${results.length} leg(s) failed after ${elapsedSeconds}s`);
    process.exit(1);
  }
  notice(`roundtrip ${ql}: ${results.length} leg(s) passed in ${elapsedSeconds}s`);
}

// Import-safe: the tests import legCommands without running a leg.
if (process.argv[1] && process.argv[1].endsWith('chdb-roundtrip.mjs')) {
  await main();
}
