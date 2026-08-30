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
// Two levels of fan-out (tsouza/cerberus#2629)
// ---------------------------------------------------------------------------
// FANOUT is the IN-RUNNER level: N processes sharing one runner's cores. It
// has a hard ceiling — chDB threads WITHIN a single query, so a runner-level
// fan-out past ~3-4 processes on a 4-core `ubuntu-latest` box oversubscribes
// rather than spreads (measured on the update-golden fan-out: 8 legs on an
// 8-core box, 252s, came out WORSE than 4 legs at 231s). Once a head's corpus
// outgrows what that ceiling can clear inside the job timeout, the only lever
// left is more RUNNERS, not more processes on the same one.
//
// ROUNDTRIP_SHARD_INDEX / ROUNDTRIP_SHARD_COUNT (optional; the promql leg's
// own `roundtrip-promql-shard` matrix passes both via
// `strategy.job-total`) name the OUTER, cross-runner shard this process owns.
// The two levels compose into ONE partition of the same corpus: outer shard
// `o` of `outerCount`, leg `i` of `fanOut`, owns global slice
// `(o-1)*fanOut + i` out of `outerCount*fanOut` — so `spec.ShardFromEnv`'s
// SPEC_SHARD_INDEX/COUNT never has to know two levels exist at all, and a
// head with no outer shard (outerCount defaults to 1) reduces to exactly the
// single-level fan-out this script always had.
//
// Env:
//   QL                    head name — `promql` | `logql` | `traceql`.
//                         Required.
//   TAGS                  go build tags for the leg (`chdb`, or the
//                         AGPL-oracle triple for logql/traceql). Required.
//   ROUNDTRIP_SHARD_INDEX the 1-based outer (cross-runner) shard this process
//                         owns. Optional; must be set together with
//                         ROUNDTRIP_SHARD_COUNT or not at all.
//   ROUNDTRIP_SHARD_COUNT how many outer shards the corpus is split into.
//                         Optional, same pairing rule as the index.
//   GO                    go executable; default `go`. Test seam.
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
 * A GitHub-hosted `ubuntu-latest` runner is 2 vCPU (confirmed by reproducing
 * a `phase4-promql-h` gremlins leg's CI-only failure locally under
 * `GOMAXPROCS=2 taskset -c 0,1` during the v1.19.0 cycle's own release
 * ritual — the mismatch with the 4-core figure this comment used to state was
 * never re-measured), and each leg runs its two packages with `-p 1` (below),
 * so the fan-out count IS the number of test binaries competing for those
 * cores — no hidden second multiplier.
 *
 * promql is the only head that needs one. Its leg takes ~700s where logql's and
 * traceql's whole JOBS — checkout, toolchain, libchdb install and all — finish
 * in 173-181s, so those two have an order of magnitude of headroom and a
 * fan-out would buy them nothing but N-times-repeated compilation of the
 * non-corpus tests in the same packages.
 *
 * Two rather than three for promql (dropped 2026-08-30): three processes on a
 * confirmed 2-vCPU runner means every leg runs at roughly two thirds of a
 * core, and `./internal/promql/...`'s own hand-written test suite — which
 * this fan-out does NOT shard (only `spec.ShardFromEnv`-aware `TestLower` and
 * `test/spec/promql`'s `TestRoundTripChDB` do; every other top-level test in
 * the package runs in full on every leg) — grew past ~2000 new lines of
 * chDB-backed tests over the doubly/triply-nested subquery composability
 * campaign (EPIC #2617), until one leg's three-way-contended share of that
 * now-larger fixed cost missed the per-process timeout below on a real
 * release-gate run (`roundtrip-promql-shard (3)`, PR #2736). chDB also
 * threads WITHIN a single query, so even two processes on two cores already
 * lean toward oversubscription rather than headroom — matching the
 * diminishing-returns floor the update-golden fan-out measured (8 legs on an
 * 8-core box, 252s, came out WORSE than 4, 231s) — but two is the fan-out
 * this runner's actual core count can spread across at all; one would forgo
 * in-runner parallelism entirely. The unsharded internal/promql suite's own
 * growing absolute cost is a wider problem the fan-out count alone cannot
 * solve — see the timeout comment below for the immediate mitigation; a
 * durable fix wants a partition of internal/promql's own test suite —
 * cerberus#2737.
 */
const FANOUT = {
  promql: 2,
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
 * Bumped from 12 to 25 on 2026-08-25: the promql leg's fixture corpus grew
 * past 700 TXTAR files (the #2624 audit-fix batch alone added dozens of new
 * histogram_native / mixed-or fixtures), pushing real per-leg runtime past
 * the old 12-minute bound — observed as a genuine `test timed out after
 * 12m0s` panic on a real push-to-main run, not a wedged libchdb call (the
 * goroutine dump showed ordinary tests still progressing, not one frame
 * stuck). 25 restores real headroom over actual leg runtime while staying
 * well below chdb.yml's 35-minute job cap (see the assertion below), so it
 * stays a hang detector rather than a throughput ceiling.
 *
 * Bumped from 25 to 32 on 2026-08-30, alongside FANOUT.promql dropping from 3
 * to 2 above: `internal/promql`'s own unsharded test suite (see the FANOUT
 * comment) grew enough this cycle to blow the 25-minute bound on a real
 * release-gate run (`roundtrip-promql-shard (3)`, PR #2736) even with the
 * reduced fan-out's lighter contention. This is again a genuine-growth bump,
 * not a wedged-call one — the failing run's goroutine dump showed ordinary
 * tests still executing. See cerberus#2737 for the durable fix; this bump
 * plus the fan-out cut are the release-blocking mitigation.
 */
export const GO_TEST_TIMEOUT_MINUTES = 32;

/** The env pair spec.ShardFromEnv reads. Contract with test/spec/shard.go. */
const SPEC_SHARD_INDEX_ENV = 'SPEC_SHARD_INDEX';
const SPEC_SHARD_COUNT_ENV = 'SPEC_SHARD_COUNT';

/** Heads this lane knows how to run. */
export const HEADS = Object.keys(FANOUT);

/**
 * The commands one head's leg runs, fan-out expanded.
 *
 * `outerIndex`/`outerCount` name the cross-runner shard this process owns
 * (both default to the unsharded 1/1, i.e. "this is the only runner"). The two
 * levels compose into one partition: this process's in-runner leg `i` of
 * `fanOut` owns global slice `(outerIndex-1)*fanOut + i` out of
 * `outerCount*fanOut` — so a single-runner head (outerCount 1) reduces to
 * exactly the plain in-runner fan-out this always was.
 *
 * A total of 1 (no outer sharding AND no in-runner fan-out) declares NO shard
 * env at all rather than `1/1`: half a declared partition is an error on the
 * Go side, and a whole-corpus leg should reach spec.WalkShard through the same
 * unset-means-everything path a hand-typed `go test` does.
 *
 * `-p 1` runs the leg's two packages one after the other inside the leg instead
 * of concurrently, so N legs mean exactly N test binaries. Without it `go test`
 * would run both packages of every leg at once and the real concurrency would
 * be 2N — double what the fan-out count says, and past the core count.
 */
export function legCommands({ ql, tags, go = 'go', outerIndex = 1, outerCount = 1 }) {
  const fanOut = FANOUT[ql];
  const totalLegs = fanOut * outerCount;
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
  if (totalLegs <= 1) return [{ name: ql, argv, env: {} }];
  return Array.from({ length: fanOut }, (_, i) => {
    const globalIndex = (outerIndex - 1) * fanOut + i + 1;
    const name =
      outerCount > 1 ? `${ql} shard ${outerIndex}/${outerCount} leg ${i + 1}/${fanOut}` : `${ql} ${i + 1}/${fanOut}`;
    return {
      name,
      argv,
      env: {
        [SPEC_SHARD_INDEX_ENV]: String(globalIndex),
        [SPEC_SHARD_COUNT_ENV]: String(totalLegs),
      },
    };
  });
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

/** The env pair naming the OUTER (cross-runner) shard this process owns. */
const ROUNDTRIP_SHARD_INDEX_ENV = 'ROUNDTRIP_SHARD_INDEX';
const ROUNDTRIP_SHARD_COUNT_ENV = 'ROUNDTRIP_SHARD_COUNT';

/**
 * Reads the outer shard pair, defaulting to the unsharded 1/1. Both-or-neither
 * and strictly validated, for the same reason spec.ShardFromEnvNames is: a
 * half-declared partition either walks the whole corpus on every runner or
 * walks none of it, and both report green.
 */
export function outerShardFromEnv(env) {
  const rawIndex = env[ROUNDTRIP_SHARD_INDEX_ENV];
  const rawCount = env[ROUNDTRIP_SHARD_COUNT_ENV];
  const hasIndex = rawIndex !== undefined;
  const hasCount = rawCount !== undefined;
  if (!hasIndex && !hasCount) return { outerIndex: 1, outerCount: 1 };
  if (hasIndex !== hasCount) {
    throw new Error(
      `${ROUNDTRIP_SHARD_INDEX_ENV} and ${ROUNDTRIP_SHARD_COUNT_ENV} must be set together ` +
        `(got ${ROUNDTRIP_SHARD_INDEX_ENV}=${JSON.stringify(rawIndex)}, ` +
        `${ROUNDTRIP_SHARD_COUNT_ENV}=${JSON.stringify(rawCount)})`,
    );
  }
  const outerCount = Number(rawCount);
  const outerIndex = Number(rawIndex);
  if (!Number.isInteger(outerCount) || outerCount < 1) {
    throw new Error(`${ROUNDTRIP_SHARD_COUNT_ENV}=${rawCount} must be a positive integer`);
  }
  if (!Number.isInteger(outerIndex) || outerIndex < 1 || outerIndex > outerCount) {
    throw new Error(
      `${ROUNDTRIP_SHARD_INDEX_ENV}=${rawIndex} is outside [1, ${ROUNDTRIP_SHARD_COUNT_ENV}=${outerCount}] — ` +
        'the corpus slice it names does not exist',
    );
  }
  return { outerIndex, outerCount };
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

  let outerIndex;
  let outerCount;
  try {
    ({ outerIndex, outerCount } = outerShardFromEnv(process.env));
  } catch (e) {
    error(`roundtrip ${ql}: ${e.message}`);
    process.exit(1);
  }

  const go = process.env.GO ?? 'go';
  const legs = legCommands({ ql, tags, go, outerIndex, outerCount });
  const outerNotice = outerCount > 1 ? `, outer shard ${outerIndex}/${outerCount}` : '';
  notice(`roundtrip ${ql}: ${legs.length} leg(s)${outerNotice}, ${GO_TEST_TIMEOUT_MINUTES}m per-process timeout`);

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
