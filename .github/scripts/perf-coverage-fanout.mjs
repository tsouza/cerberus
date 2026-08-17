// perf-coverage-fanout.mjs — runs the EXTRA shards of `just coverage`'s
// chDB-tagged TestCardinalityRatchet corpus walk as concurrent go test
// PROCESSES, so the lane's main `./...` sweep (still one ordinary,
// Justfile-visible `go test` invocation — see that recipe's own comment)
// only has to carry 1/RATCHET_FANOUT of the corpus itself instead of all of
// it.
//
// Why this is a script rather than more `go test` lines in the Justfile
// ---------------------------------------------------------------------------
// `just coverage`'s chdb-tagged lane used to be one `go test -tags chdb,... \
// -coverpkg=<every package> ./...` call, timed out at 40 minutes. That one
// process compiles test/perf into a single binary — Go always does this for
// one package — so TestCardinalityRatchet (a serial, non-t.Parallel() test
// that profiles the WHOLE TXTAR corpus through chDB, one fixture at a time in
// one process; see test/perf/cardinality_ratchet_test.go) shares its budget
// with every OTHER top-level test in the package. Several of those (the
// TestBaselineShards* roundtrip suite, all t.Parallel()) sat blocked in
// testing.(*T).Parallel() for the entire 40 minutes, never reaching their own
// bodies, because a parallel test only resumes once every non-parallel
// sibling in the SAME binary has finished — exactly the failure shape
// property-fanout.mjs (`.github/scripts/property-fanout.mjs`) fixed for
// test/property's analogous timeout. Four consecutive `coverage` runs on
// `main` panicked with `test timed out after 40m0s running: TestCardinalityRatchet`.
//
// TestCardinalityRatchet's own runtime is a straight line in corpus size
// (test/spec/**'s TXTAR count), and the corpus grew substantially while this
// lane's 40-minute ceiling stayed fixed — mirroring exactly what chdb.yml's
// `perf-guards-shard` job comment already documents about the SAME test:
// "the lane went from the ~8 minutes its recipe comment recorded to 867s,
// then past 1020s ... in a single day" (#2002). `perf-guards-shard` answered
// that by sharding the corpus across 8 CI matrix legs (test/perf/profile/
// shard.go's PERF_SHARD_INDEX / PERF_SHARD_COUNT), each a separate runner.
// This lane cannot spin up separate CI jobs — `just coverage` is one job —
// so it shards across concurrent OS PROCESSES on the SAME runner instead,
// the same technique property-fanout.mjs already established for the
// identical problem shape in test/property.
//
// The Justfile's main `./...` sweep is left completely intact — same flags,
// same package list, no `-skip` — deliberately: `-skip` on that sweep would
// make test/regression/tagged_test_enrollment_test.go's static evidence
// scanner discard the WHOLE invocation as unable to "certify tagged test
// execution" (parseTaggedGoTestArgs rejects any go test command carrying
// `-skip` outright), silently losing enrollment evidence for every OTHER
// chdb-tagged package the sweep also happens to be the sole CI evidence for.
// Instead, the main sweep is handed PERF_SHARD_INDEX=1 / PERF_SHARD_COUNT
// (below) so its own TestCardinalityRatchet run — still selected, still
// asserting, still real evidence — only walks 1/RATCHET_FANOUT of the
// corpus. This script runs the REMAINING RATCHET_FANOUT-1 shards
// concurrently, AFTER the main sweep finishes (so the two phases never
// compete for the runner's cores at once): TestCardinalityRatchet already
// has independent enrollment evidence via `perf-chdb`/`perf-guards-shard` in
// chdb.yml, so this script's own invocations do not need to be
// Justfile-visible.
//
// t.Parallel() inside one process is not a fix here either, for the same
// reason property-fanout.mjs's header documents: chdb-go v1.12.0's
// chdb/session.go keeps a single package-level `globalSession` shared by
// every sql.Open("chdb", "") call in a process, so racing ratchet shards
// inside one binary would silently alias the same chDB session. Separate
// PROCESSES each get their own address space and their own globalSession,
// which is what makes running the extra shards concurrently with EACH OTHER
// safe.
//
// Env:
//   TAGS       go build tags for every leg (chdb,agpl_oracle,chdb_agpl_oracle
//              in `just coverage`). Required.
//   COVERPKG   the -coverpkg package list (comma-separated) — the SAME value
//              the main sweep uses, so every leg's profile instruments
//              exactly the packages a single unsharded run would have.
//              Required.
//   GO         go executable; default `go`. Test seam.
//
// Exit: 0 when every extra shard passes; 1 on a failed shard or missing env.
//
// node: builtins only — no npm deps, no setup-node needed.

import process from 'node:process';
import { error, notice, log, group } from './lib/gh.mjs';
import { runLegBuffered } from './lib/spawn-tagged.mjs';

/** The corpus-wide ratchet this lane fans out. */
export const RATCHET_TEST = 'TestCardinalityRatchet';
export const RATCHET_RUN_PATTERN = `^${RATCHET_TEST}$`;

/**
 * How many shards the corpus walk is split into. Shard 1 is carried by the
 * Justfile's main `./...` sweep (see its own comment); this script runs
 * shards 2..RATCHET_FANOUT.
 *
 * A GitHub-hosted `ubuntu-latest` runner has 4 cores, and chDB threads WITHIN
 * a single query too (the same effect chdb-roundtrip.mjs's FANOUT and
 * property-fanout.mjs's HISTOGRAM_FANOUT both name), so process-count
 * fan-out oversubscribes before it reaches the core count. This script's
 * legs run only AFTER the main sweep has already finished (they do not
 * compete with it for cores), so RATCHET_FANOUT - 1 = 2 concurrent
 * processes here, matching the same conservative 3-way split
 * chdb-roundtrip.mjs's promql FANOUT and property-fanout.mjs's
 * HISTOGRAM_FANOUT already use for this runner shape.
 */
export const RATCHET_FANOUT = 3;

/**
 * Per-leg `go test -timeout`, in minutes.
 *
 * Covers one shard (1/RATCHET_FANOUT of the corpus). Measured locally
 * (nothing else chdb-tagged running concurrently, per the perf-chdb
 * recipe's own caveat about untrustworthy contended timings): shard 1/3
 * of the corpus took 979s (~16.3 minutes). 30 minutes leaves ~1.8x margin
 * over that measurement for a busier or smaller-core CI runner — chdb.yml's
 * own perf-guards-shard job independently confirms an 8-way shard of the
 * SAME test completes in low single-digit minutes of actual test time on
 * real CI hardware, so a 3-way shard landing well inside 30 minutes is the
 * conservative case, not the risky one. It stays under coverage.yml's job
 * `timeout-minutes: 60` with the same 3-minute abort-reporting margin
 * test/regression/go_test_timeout_budget_test.go holds every OTHER `just
 * coverage` invocation to, so Go's own alarm — which dumps every
 * goroutine's stack — always fires before the runner takes the container
 * away. perf-coverage-fanout.test.mjs asserts that ordering against
 * coverage.yml.
 */
export const RATCHET_TIMEOUT_MINUTES = 30;

/** The env pair test/perf/profile/shard.go's ShardFromEnv reads. */
const PERF_SHARD_INDEX_ENV = 'PERF_SHARD_INDEX';
const PERF_SHARD_COUNT_ENV = 'PERF_SHARD_COUNT';

/**
 * The leg commands: shards 2..RATCHET_FANOUT (shard 1 is the Justfile's
 * main sweep, not this script's concern).
 */
export function legCommands({ tags, coverpkg, go = 'go' }) {
  const common = ['-tags', tags, '-count=1', '-coverpkg', coverpkg];

  return Array.from({ length: RATCHET_FANOUT - 1 }, (_, i) => {
    const shardIndex = i + 2; // shards 2..RATCHET_FANOUT; shard 1 is the main sweep.
    return {
      name: `cardinality-ratchet ${shardIndex}/${RATCHET_FANOUT}`,
      argv: [
        go,
        'test',
        ...common,
        `-timeout=${RATCHET_TIMEOUT_MINUTES}m`,
        '-coverprofile',
        `cover-chdb-ratchet-${shardIndex}.out`,
        '-run',
        RATCHET_RUN_PATTERN,
        './test/perf/...',
      ],
      env: {
        [PERF_SHARD_INDEX_ENV]: String(shardIndex),
        [PERF_SHARD_COUNT_ENV]: String(RATCHET_FANOUT),
      },
      profile: `cover-chdb-ratchet-${shardIndex}.out`,
    };
  });
}

async function main() {
  const tags = process.env.TAGS ?? '';
  if (tags === '') {
    error('TAGS is required — the leg would compile without the chdb driver and run nothing');
    process.exit(1);
  }

  const coverpkg = process.env.COVERPKG ?? '';
  if (coverpkg === '') {
    error('COVERPKG is required — the leg would produce an uninstrumented profile');
    process.exit(1);
  }

  const go = process.env.GO ?? 'go';
  const legs = legCommands({ tags, coverpkg, go });
  notice(
    `perf-coverage: ${legs.length} extra ${RATCHET_TEST} shard(s) (2..${RATCHET_FANOUT} of ${RATCHET_FANOUT}), ` +
      'shard 1 already carried by the main sweep',
  );

  const started = Date.now();
  const results = await Promise.all(legs.map(runLegBuffered));
  const elapsedSeconds = Math.round((Date.now() - started) / 1000);

  for (const r of results) {
    group(`${r.leg.name} (exit ${r.code})`, () => log(r.out.trimEnd()));
  }

  const failed = results.filter((r) => r.code !== 0);
  if (failed.length > 0) {
    error(`perf-coverage: ${failed.length} of ${results.length} shard(s) failed after ${elapsedSeconds}s`);
    process.exit(1);
  }

  notice(`perf-coverage: ${results.length} extra shard(s) passed in ${elapsedSeconds}s`);
}

// Import-safe: the tests import legCommands without running a leg.
if (process.argv[1] && process.argv[1].endsWith('perf-coverage-fanout.mjs')) {
  await main();
}
