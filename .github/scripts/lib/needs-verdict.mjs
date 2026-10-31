// needs-verdict.mjs — the verdict of an aggregating job that rolls a sharded
// lane's producer jobs up into one status context.
//
// GitHub exposes each `needs` job's result to its dependents as one string:
// `success` only when the job (for a matrix, every leg) succeeded, and
// otherwise `failure`, `cancelled` or `skipped`. A strict `!== 'success'`
// comparison therefore catches all three not-a-pass outcomes; a test for
// `failure` alone would let a cancelled or skipped shard through green.
//
// The one deliberate skip is a docs-only change: the `changes` job succeeded
// and reported `docs_only == 'true'`, so the producers skipped by design and
// the context reports green without work. A skip without that verdict from a
// `changes` job that succeeded is a lane that never decided, not a lane with
// nothing to do, so it fails.
//
// Callers: ch-server-safety-shards.mjs `aggregate`, strict-scan-aggregate.mjs.
//
// node: builtins only.

// needsProblems — the problems in a set of `needs` results.
//   changes    needs.changes.result
//   docsOnly   needs.changes.outputs.docs_only
//   producers  [[job, result], ...] for every job the context aggregates
// Returns one message per job that did not succeed; empty means pass.
export function needsProblems({ changes, docsOnly, producers }) {
  const results = [['changes', changes]];
  if (changes === 'success' && docsOnly === 'true') return [];
  results.push(...producers);
  return results
    .filter(([, result]) => result !== 'success')
    .map(([job, result]) => `${job} did not succeed (${result || 'no result'})`);
}
