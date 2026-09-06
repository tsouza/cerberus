// run-loki-compatibility.mjs — LogQL compatibility harness entry point.
// Ported from the former compatibility/loki/scripts/run-loki-compatibility.sh
// (issue #3090) onto this tree's `.mjs` convention (CLAUDE.md invariant 15);
// the Docker Compose lifecycle pieces genuinely shared with the prometheus/
// tempo harnesses live in .github/scripts/lib/compat-compose-lifecycle.mjs.
//
// Lifecycle (mirrors run-prometheus-compatibility.mjs):
//
//   1. `docker compose up --wait` brings reference Loki + cerberus + CH up.
//   2. The Go seeder pushes a deterministic fixture to both targets and
//      asserts /labels is non-empty (the original PR 1 smoke).
//   3. Builds the cerberus-owned diff driver (cmd/loki-compliance-tester);
//      the binary imports the vendored upstream/loki-bench/ corpus loader,
//      so a `-mod=mod` build is required.
//   4. The driver runs against -addr-1 (reference Loki :23100) and
//      -addr-2 (cerberus :29092); it emits a structured JSON report
//      matching the prometheus-compliance harness's shape into
//      reports/diff.json.
//   5. The compose stack is torn down on every exit path (success, driver
//      failure, an uncaught exception, an explicit process.exit()) via
//      lib/compat-compose-lifecycle.mjs's registerComposeTeardown().
//
// The driver is the cerberus-owned `cmd/loki-compliance-tester`, whose JSON
// report shape matches the Prom harness.
//
// Exit semantics (post task #68 / "compat is informational"):
//
//   - 0  -> smoke + driver completed. Parity drift (diffs, unexpected
//           failures) is captured in the JSON report + the shields.io
//           endpoint-badge compat-score.json, NOT in the exit code.
//   - 1+ -> harness itself failed (compose up, seed, build, docker missing,
//           file write, …) OR the driver hit a hard error before it could
//           write the report. Inspect script output; the report file may be
//           empty or partial.
//
// Usage (run from the repository root):
//   node .github/scripts/run-loki-compatibility.mjs   full lifecycle
//   COMPOSE_KEEP=1 node .github/scripts/run-loki-compatibility.mjs   leave stack up after run
//   DRIVER_SKIP=1 node .github/scripts/run-loki-compatibility.mjs    run smoke only (skip diff)
//
// Env:
//   COMPOSE_KEEP        non-empty: leave the compose stack running after the
//                       run completes (useful for poking at
//                       /loki/api/v1/* and ClickHouse manually).
//   DRIVER_SKIP         non-empty: skip the diff driver entirely. Useful
//                       when the seeder is the bisect target.
//   DRIVER_REPORT       report file path (default: reports/diff.json). The
//                       driver emits a JSON envelope matching the Prom
//                       harness's report shape; existing tooling can
//                       consume both via the same schema.
//   DRIVER_SCORE        compat-score.json file path (default:
//                       reports/compat-score.json). shields.io endpoint-
//                       badge contract; see compatibility/internal/score.
//   DRIVER_CASES        compat-cases.json file path (default:
//                       reports/compat-cases.json). The per-case (identity,
//                       agreed) roster the parity ratchet gates on; see
//                       compatibility/internal/score.
//   LIVE_PATTERNS_METADATA
//                       Seeder/tester handshake for the now-anchored
//                       /patterns fixture (default:
//                       reports/live-patterns-metadata.json).
//   DRIVER_TIMEOUT      Per-request HTTP timeout (default: 30s).
//   DRIVER_TOLERANCE    -tolerance flag (default: 1e-5; matches upstream).
//   DRIVER_RANGE_TYPE   -range-type flag (default: range; 'instant' also valid).
//   DRIVER_PARALLELISM  -parallelism flag (default: 8).
//   DRIVER_SKIP_BASELINE
//                       Path to the upstream `skip: true` baseline file
//                       (default: compatibility/loki/upstream-skip-baseline.txt).
//                       When set, the driver asserts the upstream YAML's
//                       skipped set matches this file; drift is a hard
//                       error (sanity rail per task #269).

import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { error, log } from './lib/gh.mjs';
import { deriveComposeProjectSuffix, exitOnSpawnFailure, registerComposeTeardown, runRejectionParityDriver } from './lib/compat-compose-lifecycle.mjs';

const ROOT_DIR = 'compatibility/loki';

const REPORT = process.env.DRIVER_REPORT || `${ROOT_DIR}/reports/diff.json`;
const SCORE = process.env.DRIVER_SCORE || `${ROOT_DIR}/reports/compat-score.json`;
const CASES = process.env.DRIVER_CASES || `${ROOT_DIR}/reports/compat-cases.json`;
const LIVE_PATTERNS_METADATA = process.env.LIVE_PATTERNS_METADATA || `${ROOT_DIR}/reports/live-patterns-metadata.json`;
const TIMEOUT = process.env.DRIVER_TIMEOUT || '30s';
const TOLERANCE = process.env.DRIVER_TOLERANCE || '1e-5';
const RANGE_TYPE = process.env.DRIVER_RANGE_TYPE || 'range';
const PARALLELISM = process.env.DRIVER_PARALLELISM || '8';
const SKIP_BASELINE = process.env.DRIVER_SKIP_BASELINE || `${ROOT_DIR}/upstream-skip-baseline.txt`;

function summarise(report) {
  let passed = 0;
  let diffs = 0;
  let unexpectedFailures = 0;
  let unsupported = 0;
  for (const result of report.results ?? []) {
    const diff = result?.diff ?? '';
    const unexpected = result?.unexpectedFailure ?? '';
    const unexpectedSuccess = result?.unexpectedSuccess ?? false;
    if (diff !== '') diffs++;
    if (unexpected !== '') unexpectedFailures++;
    if (result?.unsupported === true) unsupported++;
    if (diff === '' && unexpected === '' && unexpectedSuccess === false) passed++;
  }
  return { total: report.totalResults, passed, diffs, unexpected_failures: unexpectedFailures, unsupported };
}

function main() {
  deriveComposeProjectSuffix(process.cwd());

  const scratchDir = mkdtempSync(join(tmpdir(), 'cerberus-loki-compat-'));
  registerComposeTeardown({ composeCwd: ROOT_DIR, scratchDir });

  mkdirSync(dirname(REPORT), { recursive: true });

  // The images compose FETCHES (ClickHouse, reference Loki) are acquired
  // here rather than by `up` — compose's own pull path does not carry the
  // credentials `docker login` wrote (see
  // .github/scripts/compose-pull-images.mjs). This spawnSync call, and the
  // `up` one below, stay literal per the module header in
  // lib/compat-compose-lifecycle.mjs.
  log('==> pre-pulling compose images (retry, over the authenticated pull path)');
  const prePull = spawnSync('node', ['.github/scripts/compose-pull-images.mjs', 'compatibility/loki/docker-compose.yml'], { stdio: 'inherit' });
  exitOnSpawnFailure(prePull, 'compose-pull-images.mjs');

  log('==> bringing up loki-compatibility stack (compose up --wait)');
  const bringUp = spawnSync(
    'node',
    [join(process.cwd(), '.github/scripts/build-with-registry-retry.mjs'), 'docker', 'compose', 'up', '-d', '--build', '--wait', 'clickhouse', 'loki', 'cerberus'],
    { cwd: ROOT_DIR, stdio: 'inherit' },
  );
  exitOnSpawnFailure(bringUp, 'build-with-registry-retry.mjs docker compose up');

  log('==> running seeder (go run ./cmd/seed)');
  exitOnSpawnFailure(
    spawnSync('go', ['run', './compatibility/loki/cmd/seed/', `-live-patterns-metadata=${LIVE_PATTERNS_METADATA}`], { stdio: 'inherit' }),
    'loki seeder',
  );

  if (process.env.DRIVER_SKIP) {
    log('==> DRIVER_SKIP set — finishing after smoke');
    process.exit(0);
  }

  // Build the cerberus-owned diff driver. The driver imports the vendored
  // bench package for corpus loading + cerberus's existing Loki / yaml deps
  // for the HTTP + decode path. The root go.mod marks `ignore
  // ./compatibility/loki/upstream`, which keeps `go build ./...` from
  // walking the bench tree as a build target; importing the package by path
  // is still permitted because every transitive dep is already a direct
  // entry in go.mod.
  log('==> building diff driver (cmd/loki-compliance-tester)');
  const driverBin = join(scratchDir, 'loki-compliance-tester');
  exitOnSpawnFailure(
    spawnSync('go', ['build', '-o', driverBin, './compatibility/loki/cmd/loki-compliance-tester/'], { stdio: 'inherit' }),
    'loki-compliance-tester build',
  );

  log(`==> running diff driver (writing report to ${REPORT})`);
  log('    -addr-1=http://localhost:23100  (reference Loki)');
  log('    -addr-2=http://localhost:29092  (cerberus)');
  log(`    -tolerance=${TOLERANCE} -range-type=${RANGE_TYPE} -timeout=${TIMEOUT} -parallelism=${PARALLELISM}`);

  const driverRes = spawnSync(
    driverBin,
    [
      '-addr-1=http://localhost:23100',
      '-addr-2=http://localhost:29092',
      `-corpus=${ROOT_DIR}/upstream/loki-bench/queries`,
      `-cerberus-queries=${ROOT_DIR}/cerberus-queries`,
      `-metadata-dir=${ROOT_DIR}`,
      `-live-patterns-metadata=${LIVE_PATTERNS_METADATA}`,
      `-skip-baseline=${SKIP_BASELINE}`,
      `-report=${REPORT}`,
      `-score=${SCORE}`,
      `-cases=${CASES}`,
      `-tolerance=${TOLERANCE}`,
      `-range-type=${RANGE_TYPE}`,
      `-parallelism=${PARALLELISM}`,
      `-timeout=${TIMEOUT}`,
    ],
    { stdio: 'inherit' },
  );
  if (driverRes.error) {
    error(`loki-compliance-tester failed to start: ${driverRes.error.message}`);
    process.exit(1);
  }
  const driverRc = driverRes.status ?? 1;

  // Rejection-parity pass: every deliberate 422 in internal/logql must also
  // be rejected by reference Loki.
  runRejectionParityDriver({
    repoRoot: process.cwd(),
    head: 'logql',
    ref: 'http://localhost:23100',
    cerberusUrl: 'http://localhost:29092',
    reportPath: `${ROOT_DIR}/reports/rejection-parity.json`,
  });

  log(`==> report written to ${REPORT}`);
  log(`==> score written to ${SCORE}`);
  log(`==> per-case roster written to ${CASES}`);
  log('==> summary:');
  try {
    const report = JSON.parse(readFileSync(REPORT, 'utf8'));
    log(JSON.stringify(summarise(report)));
  } catch (e) {
    log(`    (failed to parse ${REPORT} for summary: ${e.message})`);
  }

  process.exit(driverRc);
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
