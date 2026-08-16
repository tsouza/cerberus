// mutation-run.mjs — run one mutation phase with safe changed-line fallback.
//
// Required env: SCOPE, REPORT, MUTANT_TIMEOUT_MAX.
// Optional env: WORKERS, EXCLUDE_FILES, DIFF_REF.
//
// A non-empty DIFF_REF first invokes gremlins' native merge-base line filter.
// If that report contains zero executable mutants, the script deletes it and
// reruns the phase without --diff. This makes comment-only or otherwise
// mutation-free edits conservative full checks instead of hollow greens.

import { spawnSync } from 'node:child_process';
import { existsSync, readFileSync, unlinkSync } from 'node:fs';
import process from 'node:process';

import { error, notice } from './lib/gh.mjs';

function required(name) {
  const value = String(process.env[name] ?? '').trim();
  if (value === '') throw new Error(`${name} is required`);
  return value;
}

function reportTotal(path) {
  let document;
  try {
    document = JSON.parse(readFileSync(path, 'utf8'));
  } catch (cause) {
    throw new Error(`cannot read gremlins report ${path}: ${cause.message}`);
  }
  if (!Number.isSafeInteger(document.mutants_total) || document.mutants_total < 0) {
    throw new Error(`gremlins report ${path} has invalid mutants_total`);
  }
  return document.mutants_total;
}

function runGremlins({ report, diffRef = '' }) {
  const args = [
    'unleash',
    '--output',
    report,
    '--on-shutdown-status=not-run',
    '--timeout-max',
    required('MUTANT_TIMEOUT_MAX'),
  ];
  const workers = String(process.env.WORKERS ?? '').trim();
  if (workers !== '' && workers !== '0') {
    if (!/^[1-9][0-9]*$/.test(workers)) throw new Error(`WORKERS is invalid: ${workers}`);
    args.push('--workers', workers);
  }
  const exclude = String(process.env.EXCLUDE_FILES ?? '').trim();
  if (exclude !== '') args.push('--exclude-files', exclude);
  if (diffRef !== '') args.push('--diff', diffRef);
  args.push(required('SCOPE'));

  const result = spawnSync('gremlins', args, { stdio: 'inherit' });
  if (result.error) throw new Error(`cannot execute gremlins: ${result.error.message}`);
  if (result.status !== 0) throw new Error(`gremlins exited ${result.status}`);
  if (!existsSync(report)) throw new Error(`gremlins produced no report at ${report}`);
}

function main() {
  const report = required('REPORT');
  const diffRef = String(process.env.DIFF_REF ?? '').trim();
  if (diffRef !== '' && !/^[0-9a-f]{40,64}$/i.test(diffRef)) {
    throw new Error(`DIFF_REF is invalid: ${diffRef}`);
  }
  if (existsSync(report)) throw new Error(`refusing stale gremlins report ${report}`);

  runGremlins({ report, diffRef });
  if (diffRef !== '' && reportTotal(report) === 0) {
    notice('changed-line mutation found zero executable mutants; rerunning the full phase');
    unlinkSync(report);
    runGremlins({ report });
  }
  const total = reportTotal(report);
  if (total <= 0) throw new Error('mutation phase executed zero mutants after full fallback');
  notice(`mutation phase executed ${total} mutant(s)${diffRef === '' ? ' in full' : ''}`);
}

try {
  main();
} catch (cause) {
  error(cause instanceof Error ? cause.message : String(cause));
  process.exit(1);
}
