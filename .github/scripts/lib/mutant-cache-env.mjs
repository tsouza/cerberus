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
//   - DIFF_REF: the leg's merge base, which moves with every push.
//   - MUTANT_MEMORY_HOLD, MUTANT_MEMORY_LEDGER: read by the memory guard that
//     wraps each test binary, never by a test; the hold follows the leg's
//     measured budget and the ledger is a per-run path. The guard's ceiling,
//     MUTANT_MEMORY_MAX, stays in the key.
export const MUTANT_CACHE_ENV_IGNORE = Object.freeze([
  'GITHUB_*',
  'RUNNER_*',
  'ACTIONS_*',
  'INVOCATION_ID',
  'JOURNAL_STREAM',
  'DIFF_REF',
  'MUTANT_MEMORY_HOLD',
  'MUTANT_MEMORY_LEDGER',
]);
