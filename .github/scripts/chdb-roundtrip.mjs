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
// A SECOND partition (cerberus#2737): internal/<ql>'s own hand-written
// *_test.go suite. SPEC_SHARD_INDEX/COUNT above only ever partitioned the
// TXTAR-fixture-driven walks (TestRoundTripChDB / TestLower) — every OTHER
// top-level test in either package ran in FULL, redundantly, on every leg.
// That suite's own growth (internal/promql crossed ~2000 new chDB-backed
// test lines during the v1.19.0 cycle's composability campaign) blew a real
// release-gate leg's timeout even after accounting for the fixture corpus
// being properly split — one leg's SHARE of the corpus had finished in
// 205s while the SAME leg's copy of the unpartitioned suite alone measured
// past 23 real minutes locally. [listTestNames] enumerates every top-level
// test both packages declare (authoritative — `go test -list`, not a
// source-file regex scan that could miss one), [runFilterFor] partitions
// everything except the SELF_SHARDING_TESTS entries (which own their OWN
// corpus split and must run on every leg) by measured weight — the committed
// WEIGHTS_LEDGER, heaviest test first onto the lightest leg — and
// [legCommands] wires the result in as an extra `-run` flag. See
// chdb-roundtrip.test.mjs's own completeness tests for the safety net a
// `-run`-regex partition needs: a leg that silently narrows to nothing
// reports green over an empty walk, the exact failure class every other
// shard in this file already guards against.
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
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
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
 * this fan-out did not shard at the time (only `spec.ShardFromEnv`-aware
 * `TestLower` and `test/spec/promql`'s `TestRoundTripChDB` did; every other
 * top-level test in the package ran in full on every leg) — grew past ~2000 new lines of
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
export const FANOUT = {
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

/** The repository root, which WEIGHTS_LEDGER paths are relative to. */
export const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

/** The env pair spec.ShardFromEnv reads. Contract with test/spec/shard.go. */
export const SPEC_SHARD_INDEX_ENV = 'SPEC_SHARD_INDEX';
export const SPEC_SHARD_COUNT_ENV = 'SPEC_SHARD_COUNT';

/** Heads this lane knows how to run. */
export const HEADS = Object.keys(FANOUT);

/**
 * The two package directories one head's leg runs, repo-root-relative: the
 * TXTAR walk under test/spec/<ql> and the head's own suite under
 * internal/<ql>. Each is one test binary under its own `-timeout`.
 */
export function rosterPackages(ql) {
  return [`test/spec/${ql}`, `internal/${ql}`];
}

/**
 * The per-test weight ledger of each fanned-out head, repo-root-relative.
 * Generated by `just update-chdb-roundtrip-weights`
 * (.github/scripts/chdb-roundtrip-weights.mjs), never hand-edited:
 *
 *   legs           the global leg count the ledger was measured at;
 *   selfShardLegs  {packageDir: {TestName: [seconds on leg 1..legs]}} — each
 *                  SELF_SHARDING_TESTS entry timed under every
 *                  SPEC_SHARD_INDEX/COUNT pair, since each splits its own
 *                  cases unevenly across legs;
 *   tests          {packageDir: {TestName: seconds}} — every other top-level
 *                  test of [rosterPackages], timed whole.
 */
export const WEIGHTS_LEDGER = {
  promql: 'test/chdb-roundtrip/promql-weights-baseline.json',
};

/**
 * Top-level test names, keyed by ql, that already own their OWN partition by
 * reading the SPEC_SHARD_INDEX/COUNT pair: test/spec/promql's
 * TestRoundTripChDB and internal/promql's TestLower walk their slice of the
 * fixture corpus (spec.ShardFromEnv), and the heaviest internal/promql tests
 * create their slice of their own case tables (spec.NewSubtestShard), so
 * their cost divides by the leg count instead of landing whole on one leg
 * (cerberus#4001). They must be included in EVERY leg's -run filter below
 * rather than partitioned like the rest of the suite.
 * test/regression/self_sharding_tests_test.go pins this list to exactly the
 * tests in either package that call spec.ShardFromEnv or
 * spec.NewSubtestShard: a self-sharding test missing from it would run its
 * slice on one leg only and drop the rest of its cases. Partitioning one of these would either drop its own
 * SPEC_SHARD_INDEX/COUNT-driven walk out of the legs that don't draw it —
 * walking none of ITS corpus on those legs, the exact "leg reports green
 * over nothing" failure class spec.WalkShard's own fatal check exists to
 * catch — or, on the legs that DO draw it, leave its corpus split doing
 * exactly what it already does; there is no scenario where partitioning
 * helps and one where it silently breaks, so these are simply exempted.
 */
export const SELF_SHARDING_TESTS = {
  promql: [
    'TestLower',
    'TestRoundTripChDB',
    'TestSumAvgNestedMixedGroupParity_ChDB',
    'TestMixedOrNestedSubquerySelectParity_ChDB',
    'TestCountValuesNestedMixedSerialization_ChDB',
    'TestNestedMixedConsumersAnswerLikeTheirRoots_ChDB',
    'TestHistogramRateStorageEdgeParity_ChDB',
  ],
};

/**
 * The slowest runner speed the promql legs are sized for, as a multiple of a
 * normal runner's: probe-promql measured its `internal/promql` partitions at
 * 1.9x their usual time on a slow runner (cerberus#3998).
 */
export const SLOW_RUNNER_FACTOR = 1.9;

/**
 * The share of GO_TEST_TIMEOUT_MINUTES a leg's slowest test binary may use on
 * that slow runner — the bound the chDB-lanes epic (cerberus#3997) holds every
 * lane to, leaving the rest for runner variance and suite growth.
 */
export const TIMEOUT_HEADROOM_SHARE = 0.75;

/**
 * Seconds a typical CI runner takes per ledger second. The ledger is measured
 * on whatever host runs `just update-chdb-roundtrip-weights`, not on a CI
 * runner, so its seconds are that host's. This is the ratio of the
 * `internal/promql` binary's total time over all promql legs of a CI run to
 * the total [predictLegSeconds] gives for the partition that run used: 1.016
 * for the committed ledger, over chdb.yml run 43423154952. Individual runners
 * in that run ranged from 0.70x to 1.28x the ledger host; SLOW_RUNNER_FACTOR
 * covers the runners slower than typical. It is a hand-set constant: neither
 * `just update-chdb-roundtrip-weights` nor chdb-roundtrip-weights.mjs computes
 * it, because the CI run it is measured on can only use the ledger after the
 * ledger is committed. After a regeneration on a different host, re-derive it
 * the same way from a chdb.yml run that partitioned with the new ledger.
 */
export const CI_RUNNER_SLOWDOWN = 1.02;

/**
 * The most ledger seconds one leg's test binary may be predicted to take: a
 * runner SLOW_RUNNER_FACTOR times slower than a typical one, itself
 * CI_RUNNER_SLOWDOWN times slower than the ledger host, still finishes it
 * inside TIMEOUT_HEADROOM_SHARE of its `-timeout`.
 */
export const LEG_BINARY_BUDGET_SECONDS =
  (GO_TEST_TIMEOUT_MINUTES * 60 * TIMEOUT_HEADROOM_SHARE) / (SLOW_RUNNER_FACTOR * CI_RUNNER_SLOWDOWN);

/** The package of every ledger test: TestName -> {pkg, seconds}. */
function ledgerTests(ledger) {
  const tests = new Map();
  for (const [pkg, byName] of Object.entries(ledger.tests)) {
    for (const [name, seconds] of Object.entries(byName)) tests.set(name, { pkg, seconds });
  }
  return tests;
}

/**
 * Each package binary's starting load on each of `count` legs: the measured
 * per-leg seconds of its self-sharding tests. A ledger measured at another
 * leg count carries no per-leg split for `count`, so each test's total is
 * spread evenly instead.
 */
export function selfShardLoads(ledger, count) {
  const loads = {};
  for (const pkg of Object.keys(ledger.tests)) loads[pkg] = Array(count).fill(0);
  for (const [pkg, byName] of Object.entries(ledger.selfShardLegs)) {
    loads[pkg] ??= Array(count).fill(0);
    for (const perLeg of Object.values(byName)) {
      const total = perLeg.reduce((a, b) => a + b, 0);
      for (let leg = 0; leg < count; leg++) loads[pkg][leg] += ledger.legs === count ? perLeg[leg] : total / count;
    }
  }
  return loads;
}

/** The 1-based leg FNV-1a places `name` on: a stable spread for names the ledger cannot weigh. */
function hashLeg(name, count) {
  let h = 0x811c9dc5;
  for (let i = 0; i < name.length; i++) {
    h ^= name.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return ((h >>> 0) % count) + 1;
}

/**
 * Assigns every name to one of `count` legs (1-based).
 *
 * A name the ledger weighs above zero is placed heaviest first, each on the
 * leg where its own package's binary is lightest so far — longest-processing-
 * time-first, starting every leg from its measured self-sharding load
 * ([selfShardLoads]). That lets the partition give a leg whose self-sharding
 * slices run long fewer of the other tests.
 *
 * A name the ledger does not list (a test added since it was measured), or
 * weighs at zero, has no weight to balance by, so it is placed by a hash of
 * its name instead: those spread evenly across legs rather than piling onto
 * whichever leg is lightest.
 *
 * Deterministic for a given name set and ledger: ties in weight are broken by
 * name, ties in load by the lower leg index. `names` must exclude every
 * SELF_SHARDING_TESTS entry (see [runFilterFor]).
 */
export function assignLegs(names, count, ledger) {
  const tests = ledgerTests(ledger);
  const loads = selfShardLoads(ledger, count);
  const legOf = new Map();
  const weighed = [];
  for (const name of new Set(names)) {
    const entry = tests.get(name);
    if (entry && entry.seconds > 0) weighed.push([name, entry]);
    else legOf.set(name, hashLeg(name, count));
  }
  weighed.sort(([a, ea], [b, eb]) => eb.seconds - ea.seconds || (a < b ? -1 : a > b ? 1 : 0));
  for (const [name, { pkg, seconds }] of weighed) {
    const load = loads[pkg];
    let lightest = 0;
    for (let leg = 1; leg < count; leg++) if (load[leg] < load[lightest]) lightest = leg;
    load[lightest] += seconds;
    legOf.set(name, lightest + 1);
  }
  return legOf;
}

/**
 * The slice of `names` global leg `index` of `count` owns under [assignLegs].
 * Every leg computes the same assignment from the same `go test -list`
 * enumeration and the same committed ledger, so the slices are disjoint and
 * cover `names`.
 */
export function partitionTestNames(names, index, count, ledger) {
  if (count <= 1) return names;
  const legOf = assignLegs(names, count, ledger);
  return names.filter((n) => legOf.get(n) === index);
}

/**
 * The predicted ledger seconds of every package binary on every leg for an
 * assignment `legOf` (TestName -> 1-based leg) over `count` legs:
 * {packageDir: [seconds on leg 1..count]}. The same model [assignLegs]
 * balances: each leg's measured self-sharding load plus the ledger weight of
 * every test placed on it. A name the ledger does not weigh adds nothing.
 */
export function predictLegSeconds(legOf, count, ledger) {
  const tests = ledgerTests(ledger);
  const loads = selfShardLoads(ledger, count);
  for (const [name, leg] of legOf) {
    const entry = tests.get(name);
    if (entry) loads[entry.pkg][leg - 1] += entry.seconds;
  }
  return loads;
}

/**
 * How `names` (one `go test -list` enumeration, self-sharding tests excluded)
 * and the ledger disagree: tests the ledger does not weigh yet, and ledger
 * entries for tests that no longer exist. Neither breaks the partition — an
 * unweighed test is placed by hash and a stale entry is never selected — but
 * both mean the ledger is due for `just update-chdb-roundtrip-weights`.
 */
export function ledgerDrift(names, ledger) {
  const live = new Set(names);
  const weighed = ledgerTests(ledger);
  return {
    unmeasured: [...live].filter((n) => !weighed.has(n)).sort(),
    stale: [...weighed.keys()].filter((n) => !live.has(n)).sort(),
  };
}

/**
 * The global promql leg count chdb.yml runs: the `roundtrip-promql-shard`
 * job's `shard:` matrix list length times FANOUT.promql.
 */
export function promqlLegCount(workflow) {
  const start = workflow.indexOf('\n  roundtrip-promql-shard:');
  if (start < 0) throw new Error('chdb.yml: no roundtrip-promql-shard job');
  const rest = workflow.slice(start + 1);
  const next = rest.slice(1).search(/\n {2}[A-Za-z0-9_-]+:\s*\n/);
  const job = next < 0 ? rest : rest.slice(0, next + 1);
  const m = /^\s*shard:\s*\[([0-9,\s]*)\]\s*$/m.exec(job);
  if (!m) throw new Error('chdb.yml: roundtrip-promql-shard declares no `shard: [...]` matrix list');
  return m[1].split(',').filter((x) => x.trim() !== '').length * FANOUT.promql;
}

/**
 * The `-run` regex selecting global leg `index` of `count`'s share of
 * `names` — every SELF_SHARDING_TESTS entry for `ql` unconditionally
 * (present in every leg — see that const's own doc), plus this leg's
 * weight-balanced slice of everything else ([partitionTestNames]). Names are valid Go identifiers
 * (`[A-Za-z0-9_]+`, enforced by [listTestNames]'s own filter), so no
 * alternation member needs escaping.
 *
 * Anchored on both ends (`^...$`) so a name that is a PREFIX of another
 * (`TestFoo` vs `TestFooBar`) never cross-matches — `go test -run`'s own
 * semantics do this per `/`-separated path segment already for subtests,
 * but two DISTINCT top-level test names sharing a prefix are not subtests
 * of each other and must not share a match.
 */
export function runFilterFor(names, ql, index, count, ledger) {
  const always = SELF_SHARDING_TESTS[ql] ?? [];
  const alwaysSet = new Set(always);
  const partitioned = partitionTestNames(
    names.filter((n) => !alwaysSet.has(n)),
    index,
    count,
    ledger,
  );
  const selected = [...always, ...partitioned];
  return `^(${selected.join('|')})$`;
}

/**
 * Enumerates every top-level Test function `go test` would run across BOTH
 * of a head's packages (`./test/spec/<ql>/...` and `./internal/<ql>/...`) —
 * the exact pair [legCommands] runs together in one invocation, so the same
 * `-run` regex has to cover names from either package. `go test -list`
 * rather than a source-file regex scan: it is the authoritative enumeration
 * (respects build tags, handles every legal function-signature spelling)
 * where a hand-rolled scan of *_test.go files would silently miss a test
 * behind an unusual signature or a tag this invocation's own TAGS does not
 * select — exactly the "leg silently walks less than it should" failure
 * class this whole partition exists to avoid, not reintroduce.
 *
 * Reuses [warmupCommand]'s already-warm build cache (this always runs AFTER
 * it — see main()), so listing costs a link, not a compile.
 *
 * Throws on a non-zero exit rather than returning a partial list: a failed
 * enumeration silently producing an empty or truncated corpus is the same
 * "reports green over less than it should" shape as everything else this
 * file's sharding logic guards against.
 */
export function listTestNames({ ql, tags, go = 'go' }) {
  const res = spawnSync(go, ['test', '-tags', tags, '-list', '.*', ...rosterPackages(ql).map((dir) => `./${dir}/...`)], {
    encoding: 'utf8',
  });
  if (res.status !== 0) {
    throw new Error(`listing ${ql} test names failed (exit ${res.status}): ${res.stderr || res.stdout}`);
  }
  const names = new Set();
  for (const line of res.stdout.split('\n')) {
    const t = line.trim();
    if (/^Test[A-Za-z0-9_]*$/.test(t)) names.add(t);
  }
  return [...names];
}

/** A fanned-out head's committed weight ledger. */
export function readLedger(ql) {
  const ledgerPath = WEIGHTS_LEDGER[ql];
  if (!ledgerPath) throw new Error(`${ql} fans out but WEIGHTS_LEDGER names no ledger for it`);
  return JSON.parse(readFileSync(path.join(repoRoot, ledgerPath), 'utf8'));
}

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
export function legCommands({ ql, tags, go = 'go', outerIndex = 1, outerCount = 1, testNames = [], ledger = null }) {
  const fanOut = FANOUT[ql];
  const totalLegs = fanOut * outerCount;
  const baseArgv = [go, 'test', '-tags', tags, '-count=1', '-p', '1', `-timeout=${GO_TEST_TIMEOUT_MINUTES}m`];
  const packages = rosterPackages(ql).map((dir) => `./${dir}/...`);
  if (totalLegs <= 1) return [{ name: ql, argv: [...baseArgv, ...packages], env: {} }];
  return Array.from({ length: fanOut }, (_, i) => {
    const globalIndex = (outerIndex - 1) * fanOut + i + 1;
    const name =
      outerCount > 1 ? `${ql} shard ${outerIndex}/${outerCount} leg ${i + 1}/${fanOut}` : `${ql} ${i + 1}/${fanOut}`;
    // testNames partitions internal/<ql>'s OWN hand-written test suite
    // across legs too (cerberus#2737), balanced by the weight ledger
    // (cerberus#4001) — SPEC_SHARD_INDEX/COUNT below only
    // ever partitioned the TXTAR-fixture-driven walks (TestRoundTripChDB /
    // TestLower), so every OTHER top-level test ran in full, redundantly,
    // on every leg. Empty testNames (the two logql/traceql heads, whose
    // FANOUT is 1 and never reach this branch, plus any caller that hasn't
    // enumerated yet) means no -run filter at all — the pre-#2737 behaviour.
    const argv =
      testNames.length > 0
        ? [...baseArgv, `-run=${runFilterFor(testNames, ql, globalIndex, totalLegs, ledger)}`, ...packages]
        : [...baseArgv, ...packages];
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

  // Enumerating AFTER the warmup build reuses its warm cache (a link, not a
  // compile) and only when FANOUT[ql] > 1 — the sole reason to partition at
  // all. cerberus#2737.
  let testNames = [];
  let ledger = null;
  if (FANOUT[ql] > 1) {
    try {
      testNames = listTestNames({ ql, tags, go });
      ledger = readLedger(ql);
    } catch (e) {
      error(`roundtrip ${ql}: ${e.message}`);
      process.exit(1);
    }
  }

  if (ledger) {
    const always = new Set(SELF_SHARDING_TESTS[ql] ?? []);
    const { unmeasured, stale } = ledgerDrift(
      testNames.filter((n) => !always.has(n)),
      ledger,
    );
    const ledgerNote = `regenerate ${WEIGHTS_LEDGER[ql]} with \`just update-chdb-roundtrip-weights\``;
    if (unmeasured.length > 0) {
      notice(`roundtrip ${ql}: ${unmeasured.length} test(s) not in the weight ledger, placed by name hash (${ledgerNote}): ${unmeasured.join(', ')}`);
    }
    if (stale.length > 0) {
      notice(`roundtrip ${ql}: ${stale.length} weight ledger entr(ies) name no live test and are ignored (${ledgerNote}): ${stale.join(', ')}`);
    }
    if (ledger.legs !== outerCount * FANOUT[ql]) {
      notice(`roundtrip ${ql}: the weight ledger was measured at ${ledger.legs} legs, not ${outerCount * FANOUT[ql]}; self-sharding loads are spread evenly (${ledgerNote})`);
    }
  }

  const legs = legCommands({ ql, tags, go, outerIndex, outerCount, testNames, ledger });
  const outerNotice = outerCount > 1 ? `, outer shard ${outerIndex}/${outerCount}` : '';
  notice(`roundtrip ${ql}: ${legs.length} leg(s)${outerNotice}, ${GO_TEST_TIMEOUT_MINUTES}m per-process timeout`);

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
