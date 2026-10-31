// strict-scan-aggregate.mjs — the `strict-scan` job's verdict (strict-scan.yml,
// lane `chdb.strict-scan`): rolls the `strict-scan-shard` matrix up into the
// one `strict-scan` status context the `main` ruleset and release.yml's
// preflight require by name.
//
// Fails unless `changes` succeeded and, outside a docs-only change (whose
// shards skip by design), the shard matrix succeeded. A matrix result is
// `success` only when every shard succeeded, so a failed, cancelled (a
// `timeout-minutes` kill) or skipped shard fails the context. The rule is
// lib/needs-verdict.mjs's.
//
// Env:
//   CHANGES_RESULT  needs.changes.result
//   DOCS_ONLY       needs.changes.outputs.docs_only
//   SHARDS_RESULT   needs.strict-scan-shard.result
//
// Exit: 0 on success, 1 otherwise.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { error, log } from './lib/gh.mjs';
import { needsProblems } from './lib/needs-verdict.mjs';

export const SHARD_JOB = 'strict-scan-shard';

// aggregate — the problems in the shard matrix's result.
export function aggregate({ changes, docsOnly, shards }) {
  return needsProblems({ changes, docsOnly, producers: [[SHARD_JOB, shards]] });
}

function main() {
  const problems = aggregate({
    changes: process.env.CHANGES_RESULT,
    docsOnly: process.env.DOCS_ONLY,
    shards: process.env.SHARDS_RESULT,
  });
  for (const problem of problems) error(`strict-scan: ${problem}`);
  if (problems.length > 0) return 1;
  log('every strict-scan shard succeeded.');
  return 0;
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exit(main());
}
