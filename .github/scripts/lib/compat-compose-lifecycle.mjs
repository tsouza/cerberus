// compat-compose-lifecycle.mjs — the Docker Compose stack lifecycle pieces
// genuinely shared by the three differential-compatibility harnesses
// (run-prometheus-compatibility.mjs, run-loki-compatibility.mjs,
// run-tempo-compatibility.mjs), ported off the bash scripts of the same name
// (issue #3090). CLAUDE.md invariant 15: logic duplicated across two or more
// scripts moves here rather than being copy-pasted per harness.
//
// WHAT LIVES HERE, and why each piece is safe to centralise:
//   - deriveComposeProjectSuffix()   — exports COMPOSE_PROJECT_SUFFIX for
//     every subprocess this process spawns afterward (capture()/exec()
//     default to `env: process.env`, so setting it once here is enough).
//   - registerComposeTeardown()      — the `trap cleanup EXIT` replacement:
//     removes a scratch directory and runs `docker compose down -v` unless
//     COMPOSE_KEEP is set, on every process exit (including an uncaught
//     exception or an explicit process.exit()) via `process.on('exit', …)`,
//     which — unlike a try/finally — fires no matter which of the harness's
//     many early-return paths triggered the exit.
//   - exitOnSpawnFailure()           — the "did the child start and exit 0"
//     check every spawnSync call in the three harnesses repeats verbatim.
//   - runRejectionParityDriver()     — the `go run
//     ./compatibility/cmd/rejection-parity …` invocation, byte-identical
//     across all three harnesses bar four parameters (head / ref / cerberus
//     URL / eval-time). `go` is not a program
//     .github/scripts/assert-image-jobs-authenticate.mjs's R1-R6 resolver
//     dispatches on, so centralising this call carries no risk of hiding an
//     acquisition from that gate.
//
// WHAT DELIBERATELY DOES NOT LIVE HERE: the actual `docker compose up`
// bring-up and the `compose-pull-images.mjs` pre-pull. Both need to stay as
// literal `spawnSync('node', [...])` / `spawnSync('docker', [...])` calls
// INLINE in each harness file, even though the three calls are visually
// almost identical. assert-image-jobs-authenticate.mjs's R4 rule proves a
// job pre-pulls before it materialises a compose model by statically
// pattern-matching `capture|exec|spawnSync|spawn(` calls with a literal
// program name and a literal (or provably-resolvable) argv IN THE FILE the
// workflow's `run:` step names — it does not follow a script's own `import`s
// looking for spawn calls nested inside them (only for whether an imported
// module calls the SHARED MIRROR-FIRST POLICY, a narrower question). Moving
// the pre-pull or the `up` invocation into this module would make both
// invisible to that scanner: the job would silently stop being checked for
// the exact anonymous-Docker-Hub-pull regression the gate exists to catch
// (see that module's own header, R4). So the three-line spawnSync calls stay
// duplicated per harness — narrowly, and for a verified reason, not out of
// inattention.

import { spawnSync } from 'node:child_process';
import { rmSync } from 'node:fs';
import process from 'node:process';

import { error, log } from './gh.mjs';

// isComposeKeepSet — COMPOSE_KEEP honoured exactly like the bash scripts:
// any non-empty value leaves the stack running for post-mortem debugging.
export function isComposeKeepSet() {
  return (process.env.COMPOSE_KEEP ?? '').trim() !== '';
}

// deriveComposeProjectSuffix — run scripts/compose-project-suffix.sh and
// export its stdout as COMPOSE_PROJECT_SUFFIX for every later subprocess.
// `path` is a computed value on purpose: this call names no docker/node
// program and no image, so it carries nothing assert-image-jobs-authenticate.mjs
// tracks — see the module header for the rule that constrains what MUST stay
// literal, and why this one does not have to.
export function deriveComposeProjectSuffix(repoRoot) {
  const path = `${repoRoot}/scripts/compose-project-suffix.sh`;
  const res = spawnSync(path, [], { encoding: 'utf8' });
  if (res.status !== 0) {
    error(`${path} failed (status ${res.status}): ${res.stderr ?? ''}`);
    process.exit(1);
  }
  const suffix = (res.stdout ?? '').trim();
  process.env.COMPOSE_PROJECT_SUFFIX = suffix;
  return suffix;
}

// registerComposeTeardown — the trap-EXIT replacement. `composeCwd` is the
// harness's own compose directory (`docker compose down -v` there resolves
// the project via its `name:` interpolation, exactly like the bash `cd
// "$ROOT_DIR"` before every compose invocation); `scratchDir`, when given, is
// removed first (the mktemp-directory replacement for the harness's own
// throwaway binaries/overlays). `docker compose down` is not a verb
// assert-image-jobs-authenticate.mjs's R4 tracks (only up/pull/create/run/
// build materialise or acquire an image), so hiding it in this shared module
// carries none of the risk the header above documents for the pre-pull/up
// calls.
export function registerComposeTeardown({ composeCwd, scratchDir }) {
  process.on('exit', () => {
    if (scratchDir) {
      try {
        rmSync(scratchDir, { recursive: true, force: true });
      } catch {
        // Best-effort: a removal failure here must never mask the process's
        // real exit code.
      }
    }
    if (isComposeKeepSet()) {
      log('==> COMPOSE_KEEP set — leaving the compose stack running');
      return;
    }
    log('==> tearing down (set COMPOSE_KEEP=1 to leave running)');
    spawnSync('docker', ['compose', 'down', '-v'], { cwd: composeCwd, stdio: 'inherit' });
  });
}

// exitOnSpawnFailure — the "did the child even start, and did it exit 0"
// check every spawnSync call in the three harnesses repeats verbatim
// (compose-pull-images.mjs, the build-with-registry-retry.mjs-wrapped
// `docker compose up`, and each harness's own go build/go run steps that
// must hard-fail the harness on error, matching the replaced bash's `set
// -eu`). Takes an ALREADY-COMPLETED spawnSync result, never the argv — the
// argv has to stay inline at the call site (see the module header), but what
// happens to the result afterward is not something the R4 scanner reads.
export function exitOnSpawnFailure(result, label) {
  if (result.error) {
    error(`${label} failed to start: ${result.error.message}`);
    process.exit(1);
  }
  if (result.status !== 0) {
    error(`${label} exited ${result.status}`);
    process.exit(result.status ?? 1);
  }
}

// runRejectionParityDriver — every deliberate 4xx in the head's parser must
// also be rejected by the reference backend (status-class comparison only).
// See test/rejection-parity/ and docs/compatibility.md. The driver exits
// non-zero on a wrong_rejection / divergence_resolved / divergence_closed
// verdict, which this function propagates as a hard failure exactly like the
// replaced bash's `set -e` did — only stale_catalogue and hard_error stay
// non-fatal inside the driver itself.
//
// `evalTime`, when given, pins the driver onto the same instant the calling
// harness's own comparison tester uses (only the PromQL harness needs this:
// its fixture is seeded into a fixed past hour, so a driver left at
// wall-clock `now` reads empty selectors from both backends and no
// data-dependent reference guard ever fires).
export function runRejectionParityDriver({
  repoRoot,
  head,
  ref,
  cerberusUrl,
  reportPath,
  evalTime,
  catalogueDir = 'test/rejection-parity/catalogue',
}) {
  log(`==> running rejection-parity driver (${head})`);
  const args = ['run', './compatibility/cmd/rejection-parity', '-head', head, '-catalogue', catalogueDir, '-ref', ref, '-cerberus', cerberusUrl];
  if (evalTime) args.push('-eval-time', evalTime);
  args.push('-report', reportPath);

  const res = spawnSync('go', args, { cwd: repoRoot, stdio: 'inherit' });
  exitOnSpawnFailure(res, `rejection-parity driver (${head})`);
  log(`==> rejection-parity report written to ${reportPath}`);
}
