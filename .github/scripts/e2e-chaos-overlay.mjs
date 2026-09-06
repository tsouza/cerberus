// e2e-chaos-overlay.mjs — apply the chaos lane's resilience-knob env
// overlay to the running cerberus Deployment, extracted from
// `just e2e-chaos-overlay`'s inline bash (cerberus issue #3096, epic #3091
// phase 5).
//
// Reads test/e2e/chaos/manifests/chaos-overlay.env (one KEY=VALUE per
// non-blank, non-comment line — see that file's own header for what each
// knob does and why), patches the running Deployment's pod env via
// `kubectl set env` (one rollout), then waits for the rollout so every
// cerberus pod carries the overlay before fault injection starts.
// Idempotent — re-applying the same env values is a no-op rollout.
//
// Env contract:
//   NAMESPACE          k8s namespace                   (default cerberus)
//   DEPLOYMENT         Deployment to patch              (default cerberus)
//   OVERLAY_ENV_FILE   the KEY=VALUE overlay file       (default test/e2e/chaos/manifests/chaos-overlay.env)
//   ROLLOUT_TIMEOUT_SECONDS  rollout-status budget       (default 120)
//
// Exit: 0 once the overlay is applied and the rollout completes; 1 on any
// kubectl failure or an unreadable overlay file.

import { readFileSync } from 'node:fs';
import process from 'node:process';

import { error, exec, log, notice } from './lib/gh.mjs';

const NAMESPACE = process.env.NAMESPACE || 'cerberus';
const DEPLOYMENT = process.env.DEPLOYMENT || 'cerberus';
const OVERLAY_ENV_FILE = process.env.OVERLAY_ENV_FILE || 'test/e2e/chaos/manifests/chaos-overlay.env';
const ROLLOUT_TIMEOUT_SECONDS = Number(process.env.ROLLOUT_TIMEOUT_SECONDS || '120');

// parseEnvFile — every non-blank, non-`#`-comment line of `text`, split on
// whitespace exactly the way the extracted bash's `case '' | \#*` skip +
// unquoted `$env_args` word-splitting did (each real line is one
// `KEY=VALUE` token; splitting on whitespace is a no-op for those but keeps
// the transform byte-identical to the shell it replaces for any future
// line that carries more than one token). Pure so the overlay's parsing is
// unit-testable without a cluster.
export function parseEnvFile(text) {
  const args = [];
  for (const rawLine of text.split('\n')) {
    const line = rawLine.trim();
    if (line === '' || line.startsWith('#')) continue;
    args.push(...line.split(/\s+/));
  }
  return args;
}

function main() {
  let text;
  try {
    text = readFileSync(OVERLAY_ENV_FILE, 'utf8');
  } catch (err) {
    error(`could not read overlay env file ${OVERLAY_ENV_FILE}: ${err.message}`);
    process.exit(1);
  }
  const envArgs = parseEnvFile(text);
  if (envArgs.length === 0) {
    error(`${OVERLAY_ENV_FILE} carries no KEY=VALUE lines — refusing a no-op \`kubectl set env\` with no args`);
    process.exit(1);
  }

  log(`==> applying chaos overlay (resilience knobs) to deploy/${DEPLOYMENT}`);
  log(`    kubectl set env deploy/${DEPLOYMENT} ${envArgs.join(' ')}`);
  exec('kubectl', ['-n', NAMESPACE, 'set', 'env', `deployment/${DEPLOYMENT}`, ...envArgs]);

  log('==> waiting for the overlay rollout');
  exec('kubectl', ['-n', NAMESPACE, 'rollout', 'status', `deployment/${DEPLOYMENT}`, `--timeout=${ROLLOUT_TIMEOUT_SECONDS}s`]);

  notice(`e2e-chaos-overlay: ${envArgs.length} env var(s) applied to deploy/${DEPLOYMENT}, rollout complete`);
}

function isMain() {
  const invoked = process.argv[1] || '';
  return invoked.endsWith('e2e-chaos-overlay.mjs');
}

if (isMain()) {
  main();
  process.exit(0);
}
