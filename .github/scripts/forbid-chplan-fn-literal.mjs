// forbid-chplan-fn-literal.mjs — keep chplan's function vocabulary sealed.
//
// Scans every tracked or untracked Go source file and rejects direct string
// conversions such as chplan.Fn("arrayMap") or Fn(`arrayMap`). Call sites must
// use a declared Fn* constant so spelling changes and emitter completeness are
// checked centrally. Dynamic conversions used by the resolution-table source
// scanner are allowed because the argument is not a literal.
//
// The resolution boundary itself is excluded: its negative test deliberately
// constructs an undeclared symbol to prove emission fails closed.
//
// Exit codes: 0 = clean; 1 = at least one raw literal construction.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { error, log, lsFiles } from './lib/gh.mjs';

const RESOLUTION_BOUNDARY = /^internal\/chsql\/fnresolution(?:_completeness)?(?:_test)?\.go$/;
const RAW_FN_LITERAL = /\b(?:chplan\.)?Fn\s*\(\s*(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|`[^`]*`)/g;

let violations = 0;
for (const file of lsFiles(['*.go', ':!:vendor/**', ':!:.claude/**'])) {
  const rel = file.replace(/\\/g, '/');
  if (RESOLUTION_BOUNDARY.test(rel)) continue;

  let source;
  try {
    source = readFileSync(file, 'utf8');
  } catch (err) {
    // `git ls-files --cached --others` still reports a tracked path removed in
    // the working tree until the deletion is staged. It has no source left to
    // inspect; the renamed/untracked replacement is returned separately.
    if (err.code === 'ENOENT') continue;
    error(`forbid-chplan-fn-literal: cannot read ${rel}: ${err.message}`);
    violations++;
    continue;
  }

  for (const match of source.matchAll(RAW_FN_LITERAL)) {
    const line = source.slice(0, match.index).split('\n').length;
    error(
      `${rel}:${line}: raw chplan.Fn literal bypasses the sealed vocabulary. ` +
      `Declare and use a named Fn* constant in internal/chplan/fn.go, then add ` +
      `its ClickHouse resolution in internal/chsql/fnresolution.go.`,
    );
    violations++;
  }
}

if (violations > 0) {
  log(`forbid-chplan-fn-literal: ${violations} violation(s) found.`);
  process.exit(1);
}

log('forbid-chplan-fn-literal: no raw Fn literal constructions.');
