// run-prometheus-compatibility.mjs — PromQL compatibility harness entry
// point. Ported from the former compatibility/prometheus/scripts/
// run-prometheus-compatibility.sh (issue #3090) onto this tree's `.mjs`
// convention (CLAUDE.md invariant 15); the Docker Compose lifecycle pieces
// genuinely shared with the loki/tempo harnesses live in
// .github/scripts/lib/compat-compose-lifecycle.mjs.
//
// Brings up the docker-compose stack (reference Prometheus + cerberus +
// ClickHouse + seeder), builds the upstream promql-compliance-tester
// binary, runs it pointed at the two endpoints, writes the JSON report
// to compatibility/prometheus/report.json, and emits a shields.io
// endpoint-badge compat-score JSON next to it.
//
// Per task #68 ("compat is informational" workstream), this harness is
// report-only BY DEFAULT: per-case parity diffs no longer fail the run.
// The upstream tester still exits non-zero when any case diff'd (it has
// no "report-only" flag), but the wrapping script ignores that exit
// as long as it can read + score the report. Only hard infrastructure
// failures (compose-up, build, missing report.json, scorer failure)
// escalate to a non-zero rc.
//
// FAIL_ON_DIFF=1 flips that: ANY per-case parity diff or unexpected
// failure in report.json becomes a hard non-zero exit. The
// compatibility/prometheus-forced-route CI job runs with
// CERBERUS_EVAL_ROUTE=sharded + FAIL_ON_DIFF=1 so the whole-corpus run
// is the PROOF that route B (the sharded-pushdown solver) is
// byte-identical to reference Prometheus — not just the 3 chDB-lane
// fixtures. A single diff under forced routing fails the gate.
//
// Tests are RUN_ONCE here. There is no expected-failures allow-list:
// every diff against reference Prometheus is a real bug to surface and
// fix at the source.
//
// Usage (run from the repository root, matching how `just` and CI invoke
// every script here):
//   node .github/scripts/run-prometheus-compatibility.mjs        full lifecycle
//   COMPOSE_KEEP=1 node .github/scripts/run-prometheus-compatibility.mjs   leave stack up after run
//
// Env:
//   TESTER_OUTPUT     report file path (default: compatibility/prometheus/report.json)
//   TESTER_SCORE      compat-score.json output path (default:
//                     compatibility/prometheus/compat-score.json).
//                     Shields.io endpoint-badge contract — see
//                     compatibility/internal/score for the schema.
//   TESTER_CASES      compat-cases.json output path (default:
//                     compatibility/prometheus/compat-cases.json). The
//                     per-case (identity, agreed) roster the parity
//                     ratchet gates on — see compatibility/internal/score.
//   TESTER_QUERIES    queries yaml override (default: the deterministic assembly
//                     of compatibility/prometheus/query-corpus, a curated copy
//                     of upstream/promql/promql-test-queries.yml with corpus-
//                     incompatible should_fail entries removed — see
//                     query-corpus/header.yml for the policy)
//   TESTER_END_TIME   compatibility end timestamp (default: 2026-05-11T01:00:00Z)
//   TESTER_RANGE      range in seconds (default: 3600 = 1h, matches seed window)
//   CH_IMAGE          docker-compose.yml's clickhouse service image tag
//                      (default: clickhouse/clickhouse-server:26.5). Set to
//                      clickhouse/clickhouse-server:24.8 to run this SAME
//                      corpus against cerberus's declared minCHBase floor
//                      (internal/preflight/preflight.go) — the
//                      compatibility/prometheus-floor CI job does exactly
//                      that. `docker compose config` resolves the
//                      interpolation before compose-pull-images.mjs reads
//                      the model, so the override reaches both the
//                      pre-pull and `up`.
//   TESTER_QUERY_PARALLELISM  passed through to the upstream tester's
//                      `-query-parallelism` flag. Default
//                      DEFAULT_TESTER_QUERY_PARALLELISM (2) — the ONE
//                      place the lanes' parallelism is decided; the
//                      workflow no longer restates it per job. The
//                      upstream tester's own default is 20, and every
//                      comparer-timeout measurement below was taken at 2:
//                      below 25.9 every ts_grid_* native-rate feature
//                      resolves OFF (#1500), so the floor lane's fallback
//                      SQL does real per-row rate/quantile/reset
//                      aggregation instead of ClickHouse's native windowed
//                      functions — genuinely slower, not incorrect — and
//                      parallelism is the lever for that queueing
//                      contention (tsouza/cerberus#2707).
//   FAIL_ON_DIFF      non-empty: ANY per-case diff or unexpected failure
//                      in report.json becomes a hard failure (the
//                      forced-route corpus-wide proof lane).
//   COMPOSE_KEEP      non-empty: leave the compose stack running after
//                      the run completes.
//
// Exit: 0 on a completed run (parity drift is captured in report.json, not
// in the exit code, unless FAIL_ON_DIFF is set); non-zero on an
// infrastructure failure (compose-up, build, missing/unparseable
// report.json, scorer failure) or, under FAIL_ON_DIFF, on any parity drift.

import { spawnSync } from 'node:child_process';
import { closeSync, fstatSync, mkdtempSync, openSync, readFileSync, readSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { error, log, notice } from './lib/gh.mjs';
import { deriveComposeProjectSuffix, exitOnSpawnFailure, registerComposeTeardown, runRejectionParityDriver } from './lib/compat-compose-lifecycle.mjs';

const ROOT_DIR = 'compatibility/prometheus';
const COMPARER_REL_PATH = `${ROOT_DIR}/upstream/promql/comparer/comparer.go`;
const TESTER_DIR = `${ROOT_DIR}/upstream/promql/cmd/promql-compliance-tester`;
const TESTER_BIN = `${TESTER_DIR}/promql-compliance-tester`;

const OUTPUT = process.env.TESTER_OUTPUT || `${ROOT_DIR}/report.json`;
// Score JSON lives next to report.json so today's workflow (which uploads the
// single report.json file as an artifact) can be widened in task #69 to
// include the score with a single path-glob change.
const SCORE = process.env.TESTER_SCORE || `${ROOT_DIR}/compat-score.json`;
const CASES = process.env.TESTER_CASES || `${ROOT_DIR}/compat-cases.json`;
let queries = process.env.TESTER_QUERIES || '';
const END_TIME = process.env.TESTER_END_TIME || '2026-05-11T01:00:00Z';
const RANGE = process.env.TESTER_RANGE || '3600';

// Upstream's per-comparison deadline this harness build-time patches past:
// see patchComparer() below for the full rationale (tsouza/cerberus#2707,
// and the floor-only widening from tsouza/cerberus#3556 / #3560).
const ORIGINAL_COMPARE_TIMEOUT = '10*time.Second';
// COMPARER_TIMEOUT_SECONDS is the deadline every lane on the 26.5 substrate
// gets: the per-comparison wall-clock bound is the compat lanes' only
// wall-clock bound, so it stays as tight as the measured floor-lane costs
// allow. FLOOR_COMPARER_TIMEOUT_SECONDS applies only when CH_IMAGE pins a
// server below the native-rate floor (NATIVE_RATE_FLOOR_MINOR), where the
// un-optimized fallback SQL is the thing being measured.
export const COMPARER_TIMEOUT_SECONDS = 45;
export const FLOOR_COMPARER_TIMEOUT_SECONDS = 90;
// NATIVE_RATE_FLOOR_MINOR mirrors versions.yaml's `min_native_rate` (25.9):
// the ts_grid_* family arrives there, and below it every rate/quantile/
// reset shape falls through to the per-row fallback (#1500).
export const NATIVE_RATE_FLOOR_MINOR = [25, 9];
export const DEFAULT_TESTER_QUERY_PARALLELISM = 2;
export function testerParallelismArgs(env = process.env) {
  return ['-query-parallelism', env.TESTER_QUERY_PARALLELISM || String(DEFAULT_TESTER_QUERY_PARALLELISM)];
}
export const DEFAULT_CH_IMAGE = 'clickhouse/clickhouse-server:26.5';

// chImageMinor — the [major, minor] a `clickhouse/clickhouse-server:<tag>`
// image pins, or null for a tag that carries no version (`latest`, a digest).
export function chImageMinor(image) {
  const m = /:(\d+)\.(\d+)(?:[.\-]|$)/.exec(image ?? '');
  return m ? [Number(m[1]), Number(m[2])] : null;
}

// comparerTimeoutSeconds — the per-comparison deadline for the server this
// run pins. A tag without a readable version gets the tight bound: a lane
// that cannot prove it runs the floor is not given the floor's headroom.
export function comparerTimeoutSeconds(image) {
  const v = chImageMinor(image);
  if (!v) return COMPARER_TIMEOUT_SECONDS;
  const [major, minor] = v;
  const [floorMajor, floorMinor] = NATIVE_RATE_FLOOR_MINOR;
  const belowFloor = major < floorMajor || (major === floorMajor && minor < floorMinor);
  return belowFloor ? FLOOR_COMPARER_TIMEOUT_SECONDS : COMPARER_TIMEOUT_SECONDS;
}

// COMPARE_TIMEOUT_LITERAL matches whatever `N*time.Second` the comparer
// currently carries — upstream's 10, or a value an earlier run of this
// harness already patched in (a checkout is reused between lanes locally,
// and `.gitmodules` marks the submodule `ignore = dirty`, so a previous
// patch is invisible to git). Matching by shape rather than by the current
// constant is what keeps the patch idempotent across constant changes.
const COMPARE_TIMEOUT_LITERAL = /(\d+)\*time\.Second/;

// patchComparerSource — the two build-time patches as a pure function of the
// source; see patchComparer() for the rationale. Throws when the upstream
// layout no longer carries the lines being patched.
export function patchComparerSource(src, seconds) {
  const symmetricSortMarker = 'sort.Sort(refResult.(model.Matrix))';
  if (!src.includes(symmetricSortMarker)) {
    const testSortLine = 'sort.Sort(testResult.(model.Matrix))\n';
    const patched = src.replace(testSortLine, `${testSortLine}\tsort.Sort(refResult.(model.Matrix))\n`);
    if (patched === src || !patched.includes(symmetricSortMarker)) {
      throw new Error('symmetric sort (upstream layout changed?)');
    }
    src = patched;
  }
  const widenedTimeout = `${seconds}*time.Second`;
  if (!src.includes(widenedTimeout)) {
    if (!COMPARE_TIMEOUT_LITERAL.test(src)) {
      throw new Error(`compare timeout: neither ${ORIGINAL_COMPARE_TIMEOUT} nor an earlier patched value found (upstream layout changed?)`);
    }
    src = src.replace(COMPARE_TIMEOUT_LITERAL, widenedTimeout);
  }
  return src;
}

// hardFailureExit — the exit code for a run whose report is unusable. The
// tester exits 0 when every query passed, so its rc alone can be 0 with an
// empty or truncated report behind it (a failed write, a corpus that
// assembled to nothing); an unusable report is a failure whatever the rc.
export function hardFailureExit(testerRc) {
  return testerRc || 1;
}

// reportIsUsable — a parsed report with a NON-EMPTY results array. Zero
// comparisons is not a pass: it is a run that measured nothing.
export function reportIsUsable(report) {
  return report !== null && typeof report === 'object' && Array.isArray(report.results) && report.results.length > 0;
}

// cerberus's own readiness poll: the compose `--wait` healthcheck only
// proves the distroless `cerberus --version` binary runs, not that it has
// resolved the ClickHouse native-protocol port — see patchComparer()'s
// sibling comment in the replaced bash for the failure this closes.
const READYZ_URL = 'http://localhost:29091/readyz';
const READYZ_TIMEOUT_SECONDS = 120;
const READYZ_POLL_INTERVAL_MS = 1000;

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// waitForCerberusReady — poll /readyz until it answers 2xx or the deadline
// passes. The host has curl in the bash original; here the runtime's own
// global `fetch` (Node 18+, already used throughout .github/scripts/) does
// the same job with no external dependency.
async function waitForCerberusReady() {
  log('==> waiting for cerberus /readyz');
  const deadline = Date.now() + READYZ_TIMEOUT_SECONDS * 1000;
  for (;;) {
    try {
      const res = await fetch(READYZ_URL);
      if (res.ok) {
        log('==> cerberus ready');
        return;
      }
    } catch {
      // Not listening yet; keep polling until the deadline.
    }
    if (Date.now() >= deadline) {
      error(`cerberus did not become ready within ${READYZ_TIMEOUT_SECONDS}s`);
      try {
        const res = await fetch(READYZ_URL);
        error(`last /readyz status: ${res.status}`);
      } catch (e) {
        error(`last /readyz request failed: ${e.message}`);
      }
      spawnSync('docker', ['compose', 'logs', '--tail', '50', 'cerberus'], { cwd: ROOT_DIR, stdio: 'inherit' });
      process.exit(1);
    }
    await sleep(READYZ_POLL_INTERVAL_MS);
  }
}

// patchComparer — two build-time patches to the vendored
// promql-compliance-tester comparer, applied idempotently (a re-run skips a
// patch already present).
//
// 1. Symmetric matrix sort: the comparer
//    (upstream/promql/comparer/comparer.go) sorts ONLY the test backend's
//    matrix — `sort.Sort(testResult.(model.Matrix))` — and then diffs it
//    against the reference matrix in the reference's NATIVE return order.
//    Those two orders are NOT the same: `model.Matrix.Less` (prometheus/
//    common) orders series by LABEL COUNT first, then lexicographically;
//    reference Prometheus returns series in pure lexicographic order. For a
//    selector whose matched series have DIFFERENT label counts the two
//    orders diverge and the order-sensitive `cmp.Diff` reports a spurious
//    mismatch even though both backends returned byte-identical series +
//    samples. Upstream bug; a PR to prometheus/compliance to sort both sides
//    would retire this patch.
// 2. Widen the per-comparison deadline: Compare() wraps the ref+test
//    QueryRange pair — run sequentially against the SAME ctx — in one
//    hardcoded `context.WithTimeout(..., 10*time.Second)`, with no flag to
//    raise it. Below ClickHouse 25.9 (#1500) every ts_grid_* native-rate
//    feature resolves OFF, so the floor lane deliberately measures
//    cerberus's un-optimized per-row fallback SQL — genuinely slower than
//    the native path every other lane exercises, not incorrect — and
//    resets()/changes() over an exponential histogram already costs 9-13s on
//    an otherwise idle host. A shared 10s budget was always going to flip on
//    nothing but runner-speed noise (tsouza/cerberus#2707): Compare() still
//    returns the moment both calls do, so a genuine semantic divergence is
//    caught exactly as fast as before — only the amount of time a
//    legitimately slow floor-lane answer is given before being mistaken for
//    one changes.
//
//    tsouza/cerberus#3556 widened this a second time, for the FLOOR lane
//    only, for the SAME class of problem on a different query shape:
//    `rate()`/`increase()`/`delta()` over a multi-name regex selector (e.g.
//    spanning several GAUGE metric families) reaches the identical
//    un-optimized fallback — below 25.9 the native ts_grid_range aggregate
//    is unavailable, and the improved argMin/argMax/sumIf fallback
//    (chopt.FeatureFixedAccumulatorExtrapolated, #2760) stays opt-in on
//    every server regardless of version (a measured memory-vs-wall-clock
//    trade #2760/#2894 already decided against auto-enabling — re-confirmed
//    at #3556's own 2,200-series multi-name-selector scale, so it is not a
//    lever here either), so this shape falls through to the heaviest
//    groupArray/arraySort/arrayPopBack/arrayPopFront array-fold plus a
//    windowed `sum(...) OVER (...)`. What was measured (#3556): one
//    isolated re-execution of a captured per-anchor batch took ~3.4s on a
//    2-vCPU-capped container matching the floor runner, and a live corpus
//    run at DEFAULT_TESTER_QUERY_PARALLELISM (2) measured 13.1s wall-clock
//    for the whole query_range request on that host — inside 45s, with a
//    ~3x margin that a loaded GH Actions runner does not reliably keep for
//    the floor's fallback SQL. The 26.5 lanes never take that path, so
//    they keep the 45s bound (COMPARER_TIMEOUT_SECONDS); only a run whose
//    CH_IMAGE is below the native-rate floor gets
//    FLOOR_COMPARER_TIMEOUT_SECONDS — see comparerTimeoutSeconds().
function patchComparer(seconds) {
  const src = readFileSync(COMPARER_REL_PATH, 'utf8');
  log('==> patching promql-compliance-tester comparer (symmetric matrix sort)');
  log(`==> patching promql-compliance-tester comparer (per-comparison deadline ${seconds}s)`);
  let patched;
  try {
    patched = patchComparerSource(src, seconds);
  } catch (e) {
    error(`failed to patch ${COMPARER_REL_PATH}: ${e.message}`);
    process.exit(2);
  }
  writeFileSync(COMPARER_REL_PATH, patched);
}

function summarise(report) {
  let total = 0;
  let passed = 0;
  let diffs = 0;
  let unexpectedFailures = 0;
  for (const result of report.results ?? []) {
    total++;
    const diff = result?.diff ?? '';
    const unexpected = result?.unexpectedFailure ?? '';
    if (diff !== '') diffs++;
    if (unexpected !== '') unexpectedFailures++;
    if (diff === '' && unexpected === '') passed++;
  }
  return { total, passed, diffs, unexpected_failures: unexpectedFailures };
}

async function main() {
  deriveComposeProjectSuffix();

  const scratchDir = mkdtempSync(join(tmpdir(), 'cerberus-prom-compat-'));
  registerComposeTeardown({ composeCwd: ROOT_DIR, scratchDir });

  if (queries === '') {
    log('==> assembling PromQL compatibility query fragments');
    const assembled = join(scratchDir, 'queries.yml');
    const res = spawnSync('go', ['run', './compatibility/prometheus/cmd/assemble-queries', '-source', `${ROOT_DIR}/query-corpus`, '-output', assembled], {
      stdio: 'inherit',
    });
    exitOnSpawnFailure(res, 'assemble-queries');
    queries = assembled;
  }

  // The images compose FETCHES (ClickHouse, reference Prometheus) are
  // acquired here rather than by `up` — compose's own pull path does not
  // carry the credentials `docker login` wrote (see
  // .github/scripts/compose-pull-images.mjs). This spawnSync call, and the
  // `up` one below, stay literal per the module header in
  // lib/compat-compose-lifecycle.mjs.
  log('==> pre-pulling compose images (retry, over the authenticated pull path)');
  const prePull = spawnSync('node', ['.github/scripts/compose-pull-images.mjs', 'compatibility/prometheus/docker-compose.yml'], { stdio: 'inherit' });
  exitOnSpawnFailure(prePull, 'compose-pull-images.mjs');

  log('==> bringing up compatibility stack');
  const bringUp = spawnSync(
    'node',
    [join(process.cwd(), '.github/scripts/build-with-registry-retry.mjs'), 'docker', 'compose', 'up', '-d', '--build', '--wait', 'clickhouse', 'prometheus', 'cerberus'],
    { cwd: ROOT_DIR, stdio: 'inherit' },
  );
  exitOnSpawnFailure(bringUp, 'build-with-registry-retry.mjs docker compose up');

  await waitForCerberusReady();

  log('==> running seeder (go run ./cmd/seed)');
  exitOnSpawnFailure(spawnSync('go', ['run', './compatibility/prometheus/cmd/seed/'], { stdio: 'inherit' }), 'prometheus seeder');

  patchComparer(comparerTimeoutSeconds(process.env.CH_IMAGE || DEFAULT_CH_IMAGE));

  log('==> building promql-compliance-tester');
  exitOnSpawnFailure(spawnSync('go', ['build', '-o', 'promql-compliance-tester', '.'], { cwd: TESTER_DIR, stdio: 'inherit' }), 'promql-compliance-tester build');

  // Materialise the time-window overlay. The upstream tester reads
  // `query_time_parameters.end_time` + `range_in_seconds` from one of its
  // `-config-file` inputs (it concatenates all of them before YAML
  // parsing). Keeping the overlay ephemeral keeps test-cerberus.yml clean —
  // that file declares stable wiring; the overlay carries CI-volatile
  // values (END_TIME / RANGE) expected to differ per invocation.
  const overlayPath = join(scratchDir, 'overlay.yml');
  writeFileSync(overlayPath, `query_time_parameters:\n  end_time: '${END_TIME}'\n  range_in_seconds: ${RANGE}\n`);

  log('==> running tester');
  // The upstream tester's exit code:
  //   - 0 -> every test passed (no diffs, no unexpected failures)
  //   - 1 -> at least one diff / unexpected failure, OR a corpus-vs-tester
  //          mismatch (a should_fail entry that no longer fails)
  //   - 2 -> flag parse / config load / build error
  //
  // Per task #68, RC=1 (parity drift) is no longer a harness failure: the
  // report.json captures the drift and the scorer below renders it into
  // compat-score.json. RC=2 is still a hard failure because there is no
  // usable report to score — distinguished below by whether report.json
  // parses as JSON with a non-null results array.
  const testerArgs = ['-config-file', `${ROOT_DIR}/test-cerberus.yml`, '-config-file', queries, '-config-file', overlayPath, '-output-format', 'json'];
  testerArgs.push(...testerParallelismArgs());

  // Read the report back through the SAME fd the tester wrote it through
  // (fstatSync + readSync at position 0) rather than reopening OUTPUT by
  // path — reopening by path is a TOCTOU (js/file-system-race): nothing
  // guarantees the path still refers to the same file bytes between the
  // write-side close and a path-based re-open. One open, one file
  // description, no race.
  const outFd = openSync(OUTPUT, 'w+'); // 'w+', not 'w' -- the read-back below needs the fd open for reading too
  let testerRc;
  let rawReport = '';
  try {
    const testerRes = spawnSync(TESTER_BIN, testerArgs, { stdio: ['ignore', outFd, 'inherit'] });
    if (testerRes.error) {
      error(`${TESTER_BIN} failed to start: ${testerRes.error.message}`);
      process.exit(1);
    }
    testerRc = testerRes.status ?? 1;
    const size = fstatSync(outFd).size;
    const buf = Buffer.alloc(size);
    readSync(outFd, buf, 0, size, 0); // explicit position 0 -- outFd's cursor is at EOF after the subprocess wrote through it
    rawReport = buf.toString('utf8');
  } finally {
    closeSync(outFd);
  }

  log(`==> report written to ${OUTPUT}`);
  log('==> summary:');
  let report;
  try {
    report = JSON.parse(rawReport);
  } catch (e) {
    error(`report not parseable as JSON: ${e.message}`);
    report = null;
  }

  // Hard-error gate: if the tester didn't even produce a parseable JSON
  // report with a NON-EMPTY `.results` array, the run is infrastructure-
  // broken — a failed write, or a corpus that assembled to nothing. The
  // tester exits 0 on "every query passed", which an empty report also
  // satisfies, so the exit code is `testerRc || 1`: never 0 for a run that
  // measured nothing.
  if (!reportIsUsable(report)) {
    log('==> report not parseable as JSON with a non-empty .results array; treating as hard failure');
    process.exit(hardFailureExit(testerRc));
  }
  log(JSON.stringify(summarise(report)));

  // Rejection-parity pass. -eval-time pins the driver onto the same instant
  // the compliance tester above uses (the fixture is seeded into a fixed
  // past hour).
  runRejectionParityDriver({
    repoRoot: process.cwd(),
    head: 'promql',
    ref: 'http://localhost:29090',
    cerberusUrl: 'http://localhost:29091',
    evalTime: END_TIME,
    reportPath: `${ROOT_DIR}/rejection-parity.json`,
  });

  // Metadata match[] parity pass: the differential sibling of
  // internal/api/prom/metadata_test.go's matchSelectorRejectedShapes table.
  // See compatibility/prometheus/cmd/metadata-parity/main.go's package doc.
  log('==> running metadata-parity driver');
  exitOnSpawnFailure(
    spawnSync(
      'go',
      ['run', './compatibility/prometheus/cmd/metadata-parity', '-ref', 'http://localhost:29090', '-cerberus', 'http://localhost:29091', '-report', `${ROOT_DIR}/metadata-parity.json`],
      { stdio: 'inherit' },
    ),
    'metadata-parity driver',
  );
  log(`==> metadata-parity report written to ${ROOT_DIR}/metadata-parity.json`);

  // Build + run the in-tree scorer. Reads report.json and writes the
  // shields.io endpoint-badge compat-score JSON to SCORE plus the per-case
  // parity roster to CASES. Its own exit is propagated.
  log('==> building prometheus-compat-scorer');
  const scorerBin = join(scratchDir, 'prometheus-compat-scorer');
  exitOnSpawnFailure(spawnSync('go', ['build', '-o', scorerBin, './compatibility/prometheus/cmd/scorer'], { stdio: 'inherit' }), 'prometheus-compat-scorer build');

  exitOnSpawnFailure(spawnSync(scorerBin, ['-report', OUTPUT, '-score', SCORE, '-cases', CASES], { stdio: 'inherit' }), 'prometheus-compat-scorer');

  log(`==> score written to ${SCORE}`);
  log(`==> per-case roster written to ${CASES}`);

  // FAIL_ON_DIFF gate (forced-route proof lane). When set, ANY per-case diff
  // or unexpected failure in report.json is a hard failure.
  if (process.env.FAIL_ON_DIFF) {
    const { diffs, unexpected_failures: unexpectedFailures } = summarise(report);
    if (diffs !== 0 || unexpectedFailures !== 0) {
      error(
        `FAIL_ON_DIFF set and report.json has ${diffs} diff(s) + ${unexpectedFailures} unexpected failure(s) — route B is NOT byte-identical to reference Prometheus over the corpus`,
        { title: 'forced-route parity drift' },
      );
      log('==> failing cases (query + diff/unexpectedFailure):');
      for (const result of report.results ?? []) {
        const diff = result?.diff ?? '';
        const unexpected = result?.unexpectedFailure ?? '';
        if (diff === '' && unexpected === '') continue;
        const query = result?.testCase?.query ?? '(unknown)';
        let line = `  - query: ${query}\n    diff: ${diff.replaceAll('\n', '\n      ')}`;
        if (unexpected !== '') line += `\n    unexpectedFailure: ${unexpected}`;
        log(line);
      }
      process.exit(1);
    }
    notice('FAIL_ON_DIFF: zero diffs over the full corpus under forced routing — route B == reference');
  }

  if (testerRc !== 0 && !process.env.FAIL_ON_DIFF) {
    log(`==> tester rc=${testerRc} was parity drift (report.json present); exiting 0`);
  }
  process.exit(0);
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
