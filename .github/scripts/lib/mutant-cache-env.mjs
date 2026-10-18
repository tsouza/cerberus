// mutant-cache-env.mjs — the environment variables the per-mutant verdict
// cache leaves out of its key (`gremlins unleash --cache-env-ignore`).
//
// gremlins keys every mutant verdict on the whole environment its test
// binaries inherit, because a package initialiser or a TestMain can read a
// variable before the test log that records a test's own reads has started.
// Each name here changes from one run of the same leg to the next, so keying
// it would make every lookup miss, and leaving it out is sound only while no
// package initialiser or TestMain under internal/ reads it. A test's own reads
// of these variables are still recorded in the entry.
// mutation-run.test.mjs fails if any Go source under internal/ names one.
//
//   - GITHUB_*, RUNNER_*, ACTIONS_*, INVOCATION_ID, JOURNAL_STREAM: the
//     Actions runner's per-run and per-step identifiers, tokens and paths.
//   - SYSTEMD_EXEC_PID: the pid systemd started the runner service with, set
//     on hosted runners and different on every job.
//   - DIFF_REF: the leg's merge base, which moves with every push.
//   - MUTANT_MEMORY_HOLD, MUTANT_MEMORY_LEDGER: read by the memory guard that
//     wraps each test binary, never by a test; the hold follows the leg's
//     measured budget and the ledger is a per-run path. The guard's ceiling,
//     MUTANT_MEMORY_MAX, stays in the key.

import { createHash } from 'node:crypto';

export const MUTANT_CACHE_ENV_IGNORE = Object.freeze([
  'GITHUB_*',
  'RUNNER_*',
  'ACTIONS_*',
  'INVOCATION_ID',
  'JOURNAL_STREAM',
  'SYSTEMD_EXEC_PID',
  'DIFF_REF',
  'MUTANT_MEMORY_HOLD',
  'MUTANT_MEMORY_LEDGER',
]);

// GREMLINS_UNKEYED_ENV are the variables gremlins itself leaves out of the key
// (the fork's internal/cache unkeyedEnv): the shell's bookkeeping, and the
// GOTMPDIR it sets for its own builds.
export const GREMLINS_UNKEYED_ENV = Object.freeze(['PWD', 'OLDPWD', 'SHLVL', '_', 'GOTMPDIR']);

// ENV_DIGEST_HEX_LENGTH is how much of a value's sha256 keyedEnvDigests shows:
// enough to tell two values apart in a log, never the value itself.
const ENV_DIGEST_HEX_LENGTH = 12;

export function envIgnored(name, ignore = MUTANT_CACHE_ENV_IGNORE) {
  return ignore.some((p) => (p.endsWith('*') ? name.startsWith(p.slice(0, -1)) : p === name));
}

// keyedEnvDigests lists, sorted by name, every variable of `env` that enters
// gremlins' per-mutant key, as `NAME <sha256 prefix of its value>`. Two legs
// whose lists differ cannot share a verdict; diffing their lists names the
// variable that keeps a lookup from hitting.
export function keyedEnvDigests(env, ignore = MUTANT_CACHE_ENV_IGNORE) {
  return Object.keys(env)
    .filter((name) => env[name] !== undefined && !GREMLINS_UNKEYED_ENV.includes(name) && !envIgnored(name, ignore))
    .sort()
    .map((name) => `${name} ${createHash('sha256').update(String(env[name])).digest('hex').slice(0, ENV_DIGEST_HEX_LENGTH)}`);
}
