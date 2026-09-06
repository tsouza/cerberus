// run-tempo-compatibility.mjs — Tempo / TraceQL compatibility harness entry
// point. Ported from the former compatibility/tempo/scripts/
// run-tempo-compatibility.sh (issue #3090) onto this tree's `.mjs`
// convention (CLAUDE.md invariant 15); the Docker Compose lifecycle pieces
// genuinely shared with the prometheus/loki harnesses live in
// .github/scripts/lib/compat-compose-lifecycle.mjs.
//
// The driver runs in two phases sequentially:
//
//   1. seed — push a deterministic OTLP batch to Tempo's :4317 AND insert
//             the same fixture into ClickHouse so cerberus reads it via
//             /api/traces. In-process smoke confirms both backends resolve
//             the first trace ID with non-zero spans.
//   2. diff — read the TXTAR corpus, run every TraceQL query through both
//             backends over HTTP, write a markdown diff report to
//             $REPORT_DIR/diff.md AND a shields.io endpoint-badge score
//             JSON to $REPORT_DIR/compat-score.json. Report-only: the
//             differ exits 0 even when parity diffs are present; only
//             driver-wide hard errors (compose up failure, seed failure,
//             corpus load failure) escalate to a non-zero rc.
//   3. diff-grpc — same corpus + comparator as step 2, run over cerberus's
//             and reference Tempo's tempopb.StreamingQuerier gRPC/h2c
//             service instead of HTTP (#1453). Writes
//             $REPORT_DIR/diff-grpc.md + compat-score-grpc.json +
//             compat-cases-grpc.json. Same report-only contract; two corpus
//             endpoint kinds (traces / traces_v2) have no gRPC RPC and are
//             listed in the report as skipped rather than run.
//
// The seeder and differ run as Go binaries on the CI runner (host),
// connecting to Docker-published ports on localhost. This avoids Docker DNS
// resolution failures ("lookup tempo on 127.0.0.11:53: server misbehaving")
// that occur when the driver runs inside Docker on some CI runner
// configurations. Matches the pattern used by the sibling Loki harness.
//
// Usage (run from the repository root):
//   node .github/scripts/run-tempo-compatibility.mjs   full lifecycle (seed -> diff)
//   COMPOSE_KEEP=1 node .github/scripts/run-tempo-compatibility.mjs   leave stack up after run
//
// Env:
//   REPORT_DIR     where the driver writes diff.md, compat-score.json and
//                  compat-cases.json — the per-case (identity, agreed)
//                  roster the parity ratchet gates on
//                  (default: compatibility/tempo/reports/).
//   COMPOSE_KEEP   non-empty: leave the compose stack running after the
//                  differ completes (useful for poking at /api/traces and
//                  the otel_traces table manually).
//
// Exit codes:
//   0         seed + diff completed; parity drift is captured in
//             compat-score.json, not in the exit code.
//   non-zero  stack failed to come up OR seeder failed OR differ hit a hard
//             error (corpus load, report write, etc.). Parity drift never
//             reaches this branch.

import { spawnSync } from 'node:child_process';
import { chmodSync, mkdirSync, mkdtempSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { log } from './lib/gh.mjs';
import { deriveComposeProjectSuffix, exitOnSpawnFailure, registerComposeTeardown, runRejectionParityDriver } from './lib/compat-compose-lifecycle.mjs';

const ROOT_DIR = 'compatibility/tempo';
// Tempo v3's /api/search endpoint only queries completed blocks, not the
// live store. Even with complete_block_timeout: 5s, the block builder needs
// time to flush, complete, and index each block before /api/search returns
// results. On CI runners this can take 10-30s after the seeder finishes; 30s
// is a conservative upper bound.
const BLOCK_FLUSH_WAIT_SECONDS = 30;
const WAIT_TIMEOUT_SECONDS = 300;

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function main() {
  const reportDir = process.env.REPORT_DIR || `${ROOT_DIR}/reports`;

  deriveComposeProjectSuffix(process.cwd());

  const scratchDir = mkdtempSync(join(tmpdir(), 'cerberus-tempo-compat-'));
  registerComposeTeardown({ composeCwd: ROOT_DIR, scratchDir });

  mkdirSync(reportDir, { recursive: true });
  // Mirrors the replaced bash's `chmod a+w`: add write for user/group/other
  // without clobbering the rest of the mode bits.
  chmodSync(reportDir, (statSync(reportDir).mode | 0o222) & 0o7777);

  // The images compose FETCHES (ClickHouse, reference Tempo) are acquired
  // here rather than by either `up` below — compose's own pull path does
  // not carry the credentials `docker login` wrote (see
  // .github/scripts/compose-pull-images.mjs). This spawnSync call, and the
  // two `up` ones below, stay literal per the module header in
  // lib/compat-compose-lifecycle.mjs.
  log('==> pre-pulling compose images (retry, over the authenticated pull path)');
  const prePull = spawnSync('node', ['.github/scripts/compose-pull-images.mjs', 'compatibility/tempo/docker-compose.yml'], { stdio: 'inherit' });
  exitOnSpawnFailure(prePull, 'compose-pull-images.mjs');

  log('==> bringing up tempo-compatibility stack');
  // Step 1: start tempo (no compose-level healthcheck — distroless image,
  // see compose.yml). The driver polls /ready before pushing.
  const bringUpTempo = spawnSync('node', [join(process.cwd(), '.github/scripts/build-with-registry-retry.mjs'), 'docker', 'compose', 'up', '-d', '--build', 'tempo'], {
    cwd: ROOT_DIR,
    stdio: 'inherit',
  });
  exitOnSpawnFailure(bringUpTempo, 'build-with-registry-retry.mjs docker compose up (tempo)');

  // Step 2: `--wait` block on the healthchecked services. 5min compose-level
  // timeout is generous: CH boot can take 30-60s on a cold runner, cerberus
  // is <2s. A timeout here is an infra-layer issue, not a harness bug.
  const bringUpRest = spawnSync(
    'node',
    [
      join(process.cwd(), '.github/scripts/build-with-registry-retry.mjs'),
      'docker',
      'compose',
      'up',
      '-d',
      '--build',
      '--wait',
      '--wait-timeout',
      String(WAIT_TIMEOUT_SECONDS),
      'clickhouse',
      'cerberus-tempo',
    ],
    { cwd: ROOT_DIR, stdio: 'inherit' },
  );
  exitOnSpawnFailure(bringUpRest, 'build-with-registry-retry.mjs docker compose up (clickhouse, cerberus-tempo)');

  // The seeder and differ run as Go binaries on the host, connecting to
  // Docker-published ports on localhost. Port mapping from
  // docker-compose.yml:
  //   Tempo HTTP:     23200:3200   -> localhost:23200
  //   Tempo OTLP:     24317:4317   -> localhost:24317
  //   Tempo gRPC:     23095:9095   -> localhost:23095  (StreamingQuerier, #1453)
  //   cerberus:       29092:29092  -> localhost:29092  (HTTP + h2c gRPC, same port)
  //   ClickHouse:     29100:9000   -> localhost:29100

  log('==> running seeder (go run ./compatibility/tempo/driver/ seed)');
  // Note: NO ignored exit code here. The driver's exit code is meaningful —
  // masking it would let regressions land green (the same trap that bit the
  // PromQL harness pre-#298). The cleanup hook still tears down the stack on
  // a non-zero exit.
  const seedRes = spawnSync('go', ['run', './compatibility/tempo/driver/', 'seed'], { stdio: 'inherit' });
  const seedRc = seedRes.status ?? 1;
  log(`==> seeder exited with rc=${seedRc}`);
  if (seedRc !== 0) {
    log('==> seeder failed — skipping diff');
    process.exit(seedRc);
  }

  log(`==> waiting ${BLOCK_FLUSH_WAIT_SECONDS}s for Tempo to flush and index blocks...`);
  await sleep(BLOCK_FLUSH_WAIT_SECONDS * 1000);

  // Build the diff driver. The driver imports internal/schema/ddl and OTLP
  // gRPC client types via cerberus's go.mod, so it compiles from the repo
  // root like the cerberus binary.
  log('==> building diff driver');
  const driverBin = join(scratchDir, 'tempo-compat-driver');
  exitOnSpawnFailure(spawnSync('go', ['build', '-o', driverBin, './compatibility/tempo/driver/'], { stdio: 'inherit' }), 'tempo diff driver build');

  // After seed, run the differ against the same backends. Report-only:
  // parity drift is captured in compat-score.json + the markdown diff
  // report; the driver returns 0 even when cases diverge. Only driver-wide
  // hard errors (corpus load failure, report write failure) escalate to a
  // non-zero rc.
  log(`==> running diff driver (writing report to ${reportDir}/diff.md, score to ${reportDir}/compat-score.json)`);
  log('    --tempo-http=http://localhost:23200  (reference Tempo)');
  log('    --cerberus=http://localhost:29092  (cerberus)');
  const diffRes = spawnSync(
    driverBin,
    [
      'diff',
      '--tempo-http=http://localhost:23200',
      '--cerberus=http://localhost:29092',
      `--corpus=${ROOT_DIR}/driver/corpus/smoke.txtar`,
      `--report=${reportDir}/diff.md`,
      `--score=${reportDir}/compat-score.json`,
      `--cases=${reportDir}/compat-cases.json`,
    ],
    { stdio: 'inherit' },
  );
  const diffRc = diffRes.status ?? 1;

  // gRPC/h2c StreamingQuerier diff (#1453) — same corpus + comparator as the
  // HTTP diff above, driven over cerberus's and reference Tempo's
  // tempopb.StreamingQuerier gRPC service instead of HTTP. Runs against the
  // SAME seeded data (no re-seed) while the stack is still up. Report-only,
  // same exit-code contract as `diff`.
  log(`==> running diff-grpc driver (writing report to ${reportDir}/diff-grpc.md, score to ${reportDir}/compat-score-grpc.json)`);
  log('    --tempo-grpc=localhost:23095  (reference Tempo query-frontend StreamingQuerier)');
  log('    --cerberus-grpc=localhost:29092  (cerberus h2c StreamingQuerier, same port as HTTP)');
  const diffGrpcRes = spawnSync(
    driverBin,
    [
      'diff-grpc',
      '--tempo-grpc=localhost:23095',
      '--cerberus-grpc=localhost:29092',
      `--corpus=${ROOT_DIR}/driver/corpus/smoke.txtar`,
      `--report=${reportDir}/diff-grpc.md`,
      `--score=${reportDir}/compat-score-grpc.json`,
      `--cases=${reportDir}/compat-cases-grpc.json`,
    ],
    { stdio: 'inherit' },
  );
  const diffGrpcRc = diffGrpcRes.status ?? 1;

  // Rejection-parity pass: every deliberate 422 in internal/traceql must
  // also be rejected by reference Tempo.
  runRejectionParityDriver({
    repoRoot: process.cwd(),
    head: 'traceql',
    ref: 'http://localhost:23200',
    cerberusUrl: 'http://localhost:29092',
    reportPath: `${reportDir}/rejection-parity.json`,
  });

  log(`==> HTTP differ exited with rc=${diffRc}`);
  log(`==> report at ${reportDir}/diff.md`);
  log(`==> score at ${reportDir}/compat-score.json`);
  log(`==> per-case roster at ${reportDir}/compat-cases.json`);
  log(`==> gRPC differ exited with rc=${diffGrpcRc}`);
  log(`==> report at ${reportDir}/diff-grpc.md`);
  log(`==> score at ${reportDir}/compat-score-grpc.json`);
  log(`==> per-case roster at ${reportDir}/compat-cases-grpc.json`);

  // Non-zero if EITHER transport's driver hit a hard error. Per-case parity
  // drift never reaches either RC (both drivers are report-only); this is
  // purely the "did the harness itself run to completion" signal.
  process.exit(diffRc !== 0 ? diffRc : diffGrpcRc);
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
