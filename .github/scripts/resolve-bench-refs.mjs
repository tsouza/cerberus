// resolve-bench-refs.mjs — pick the benchmark baseline + ref SHAs,
// extracted from the "resolve baseline + ref SHAs" step in
// .github/workflows/perf-benchmark.yml.
//
// Reproduces the original bash exactly:
//   git fetch --no-tags origin main
//   ref_sha   = git rev-parse HEAD
//   main_sha  = git rev-parse origin/main
//   if INPUT_BASELINE_REF set      -> baseline_ref = it
//   elif ref_sha == main_sha       -> baseline_ref = ref_sha + "^"   (parent)
//   else                           -> baseline_ref = main_sha
//   baseline_sha = git rev-parse baseline_ref
//   if baseline_sha == ref_sha     -> ::error:: + exit 1 (same-vs-same noise)
//   emit ref_sha / baseline_sha / baseline_ref to $GITHUB_OUTPUT
//
// Env contract:
//   INPUT_BASELINE_REF   optional explicit baseline ref; blank -> auto-pick
//
// Exit codes: 0 = SHAs resolved + written, 1 = baseline == ref / git error.

import process from 'node:process';
import { git, error, setOutput, log } from './lib/gh.mjs';

function revParse(ref) {
  const res = git(['rev-parse', ref]);
  if (res.status !== 0) {
    error(`git rev-parse ${ref} failed: ${res.stderr.trim()}`);
    process.exit(res.status || 1);
  }
  return res.stdout.trim();
}

// git fetch --no-tags origin main
const fetch = git(['fetch', '--no-tags', 'origin', 'main']);
if (fetch.status !== 0) {
  error(`git fetch origin main failed: ${fetch.stderr.trim()}`);
  process.exit(fetch.status || 1);
}

const refSha = revParse('HEAD');
const mainSha = revParse('origin/main');

const input = process.env.INPUT_BASELINE_REF || '';

let baselineRef;
if (input !== '') {
  baselineRef = input;
} else if (refSha === mainSha) {
  // Trigger ref already is main: compare against the parent commit so the
  // diff isn't a trivial same-vs-same.
  baselineRef = `${refSha}^`;
} else {
  baselineRef = mainSha;
}

const baselineSha = revParse(baselineRef);

if (baselineSha === refSha) {
  error(
    `baseline (${baselineRef} = ${baselineSha}) is the same as ref (${refSha}); benchstat would produce same-vs-same noise.`,
  );
  process.exit(1);
}

log(`ref_sha=${refSha}`);
log(`baseline_sha=${baselineSha}`);
log(`baseline_ref=${baselineRef}`);

setOutput('ref_sha', refSha);
setOutput('baseline_sha', baselineSha);
setOutput('baseline_ref', baselineRef);

process.exit(0);
