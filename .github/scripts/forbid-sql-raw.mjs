// forbid-sql-raw.mjs — gate that enforces the no-raw-SQL rule in
// internal/chsql/.
//
// Cerberus's SQL emitter is built from typed Frags (Call, Eq, And, …).
// The raw token-writing primitives (strings.Builder writes, writeSQL,
// fmt.Sprintf of SQL) are confined to the Frag-constructor layer in
// builder.go and two known-good locations in the emitter itself. Any NEW
// raw-write site outside those locations is a violation of the typed-API
// discipline that CI cannot review but CAN detect mechanically.
//
// What is scanned:
//   internal/chsql/**/*.go  (non-test)
//
// What is flagged:
//   - strings.Builder type references or variable declarations
//   - sb.Write* / b.sb.Write* method calls (raw token writes)
//   - writeSQL( calls (the Builder's unexported raw-write method)
//   - fmt.Sprintf references (SQL string construction via formatting)
//
// Known-good files (pre-approved raw-write sites — the implementations):
//   internal/chsql/builder.go   — the Frag-primitive constructors; every
//                                 raw-write primitive in the project lives
//                                 here BY DESIGN.
//   internal/chsql/emit_node.go — three approved sites:
//     1. e.b = strings.Builder{} (emitter field reset in subqueryFrag)
//     2. b.sb.WriteString(sql)   (pre-rendered subquery splice)
//     3. var out strings.Builder (regex builder in regexQuoteMeta — not SQL)
//        plus e.b.WriteString / e.args splicing via emitSelect/splice
//   internal/chsql/emit.go      — b strings.Builder field declaration
//                                 (the emitter's output-buffer field).
//
// Exit codes: 0 = clean (no new raw-write sites), 1 = violation found.
//
// This script is intentionally conservative: it flags by FILE, not by
// line. A file outside the known-good list that contains any raw-write
// pattern is a hard failure regardless of context. The purpose is to
// prevent new emitter files from accidentally introducing raw writes;
// any legitimate new raw-write location must be added to KNOWN_GOOD here
// with a rationale comment, and that addition must be reviewed.
//
// See CLAUDE.md § "No raw SQL strings — typed chsql API only" and #1441.

import { lsFiles, error, log } from './lib/gh.mjs';
import process from 'node:process';

// KNOWN_GOOD — files that are allowed to contain raw-write patterns.
// builder.go is excluded from the scan pathspec itself (see below) because it
// is the implementation layer, not the consumer layer. The two emit files are
// allowed because they implement the output buffer and the one approved
// raw-splice (subqueryFrag). Adding a file here requires a rationale comment
// and reviewer sign-off — this is not an escape hatch, it is an inventory.
const KNOWN_GOOD = new Set([
  'internal/chsql/emit_node.go',
  'internal/chsql/emit.go',
]);

// Raw-write patterns to detect. The regex matches strings.Builder usage,
// the unexported writeSQL method, and direct sb.Write* calls.
const RAW_WRITE_PATTERN = /strings\.Builder|\.writeSQL\s*\(|\.sb\.Write|\bwriteSQL\s*\(/;

// Scan all non-test, non-builder chsql Go files.
const files = lsFiles([
  'internal/chsql/**/*.go',
  ':!:internal/chsql/builder.go',    // allowed by design — Frag constructors
  ':!:internal/chsql/**/*_test.go',  // test files are out of scope
]);

let violations = 0;

for (const file of files) {
  // Repo-relative path for set lookup.
  const rel = file.replace(/\\/g, '/');
  if (KNOWN_GOOD.has(rel)) continue;

  // Read the file and scan for raw-write patterns.
  const { readFileSync } = await import('node:fs');
  let content;
  try {
    content = readFileSync(file, 'utf8');
  } catch (e) {
    error(`forbid-sql-raw: cannot read ${file}: ${e.message}`);
    violations++;
    continue;
  }

  const lines = content.split('\n');
  for (let i = 0; i < lines.length; i++) {
    if (RAW_WRITE_PATTERN.test(lines[i])) {
      const loc = `${rel}:${i + 1}`;
      error(
        `${loc}: raw SQL token write outside the approved Frag-constructor layer. ` +
        `Use typed Frags (Call / binOp / Cast / Paren / InlineLit / …) instead. ` +
        `If this site is a legitimate new addition to the emitter layer, add it to ` +
        `KNOWN_GOOD in .github/scripts/forbid-sql-raw.mjs with a rationale comment ` +
        `(see CLAUDE.md § "No raw SQL strings" and #1441).`,
      );
      violations++;
    }
  }
}

if (violations > 0) {
  log(`forbid-sql-raw: ${violations} raw-write violation(s) found.`);
  process.exit(1);
}

log('forbid-sql-raw: no raw SQL token writes outside the approved layer.');
process.exit(0);
