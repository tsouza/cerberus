// forbid-sql-raw.mjs — gate that enforces the no-raw-SQL rule in
// internal/chsql/.
//
// Cerberus's SQL emitter is built from typed Frags (Call, Eq, And, …).
// The raw token-writing primitives (strings.Builder writes, writeSQL) are
// confined to the Frag-constructor layer in builder.go and two known-good
// locations in the emitter itself. Any NEW raw-write site outside those
// locations is a violation of the typed-API discipline that CI cannot
// review but CAN detect mechanically.
//
// SCOPE — a chsql-scoped SUBSET of CLAUDE.md invariant 10. The invariant
// forbids writing SQL tokens through a strings.Builder, writeSQL(...),
// fmt.Sprintf or `+`-concatenation EVERYWHERE outside builder.go's Frag
// primitives; this gate mechanically enforces the strings.Builder /
// writeSQL / sb.Write* half of that, and only under internal/chsql/**.
// The pathspec is deliberately NOT wider: every other package formats
// non-SQL strings (log lines, error messages, HTTP bodies) with the same
// primitives, so a tree-wide scan of these tokens would flag nothing but
// legitimate code, and widening it is a separate design decision, not a
// tweak to this file. What the gate does NOT catch, by design, and what
// reviewer discipline covers instead:
//   - format-string SQL construction (fmt.Sprintf of a SQL fragment) —
//     indistinguishable mechanically from formatting a non-SQL string;
//   - verbatim() misuse building a whole expression shape — see
//     forbid-verbatim-concat.mjs for the one mechanically detectable
//     sub-case (string concatenation inside a verbatim() argument);
//   - the semantic question of whether a typed Frag could replace a write.
//
// What is scanned:
//   internal/chsql/**/*.go  (non-test, non-builder.go)
//
// What is flagged (RAW_WRITE_PATTERN, per line):
//   - strings.Builder type references or variable declarations
//   - sb.Write* / b.sb.Write* method calls (raw token writes)
//   - writeSQL( calls (the Builder's unexported raw-write method)
//
// Known-good files (KNOWN_GOOD — pre-approved raw-write sites, the
// implementations):
//   internal/chsql/builder.go   — the Frag-primitive constructors; every
//                                 raw-write primitive in the project lives
//                                 here BY DESIGN. Excluded by the pathspec
//                                 itself rather than listed.
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
// The pure halves (RAW_WRITE_PATTERN, KNOWN_GOOD, scanFiles) are exported
// and pinned by forbid-sql-raw.test.mjs; the scan itself runs only when
// this file is invoked as the program.
//
// See CLAUDE.md § "No raw SQL strings — typed chsql API only" and #1441.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { lsFiles, error, log } from './lib/gh.mjs';

// KNOWN_GOOD — files that are allowed to contain raw-write patterns.
// builder.go is excluded from the scan pathspec itself (see below) because it
// is the implementation layer, not the consumer layer. The two emit files are
// allowed because they implement the output buffer and the one approved
// raw-splice (subqueryFrag). Adding a file here requires a rationale comment
// and reviewer sign-off — this is not an escape hatch, it is an inventory.
export const KNOWN_GOOD = new Set([
  'internal/chsql/emit_node.go',
  'internal/chsql/emit.go',
]);

// RAW_WRITE_PATTERN — the raw-write primitives, matched per line: a
// strings.Builder reference, the unexported writeSQL method (as a method
// call or a bare call), and a direct sb.Write* call. Deliberately NOT
// fmt.Sprintf — see the header's SCOPE section for why a formatting call
// cannot be told apart from non-SQL formatting mechanically.
export const RAW_WRITE_PATTERN = /strings\.Builder|\.writeSQL\s*\(|\.sb\.Write|\bwriteSQL\s*\(/;

// SCAN_PATHSPEC — all non-test, non-builder chsql Go files.
//
// `:(glob)` is required, not decorative: without it (or a global
// core.globPathspecs=true), a bare `**` is matched as a plain fnmatch
// wildcard rather than "any number of directories including zero", which
// needs a literal `/` right where the pattern has one — so it silently
// matches ZERO files for any path with no subdirectory below the prefix.
// internal/chsql has no subpackages, so this scan was passing vacuously
// (0 files scanned, 0 violations found, exit 0) until this fix. See
// forbid-verbatim-concat.mjs's identical comment and #2321 for the
// discovery, and the zero-file guard in main() for how this class of bug is
// now prevented from recurring silently.
export const SCAN_PATHSPEC = [
  ':(glob)internal/chsql/**/*.go',
  ':!:internal/chsql/builder.go',            // allowed by design — Frag constructors
  ':(exclude,glob)internal/chsql/**/*_test.go', // test files are out of scope
];

// scanFiles — every `{ file, line }` at which a file outside KNOWN_GOOD
// matches RAW_WRITE_PATTERN. `read(file)` is the file reader (injected so the
// test can drive it from fixtures); an unreadable file is itself reported as
// a violation at line 0 rather than skipped, so a scan that cannot see a
// file never passes on its account.
export function scanFiles(files, read = (f) => readFileSync(f, 'utf8')) {
  const violations = [];
  for (const file of files) {
    // Repo-relative path for set lookup.
    const rel = file.replace(/\\/g, '/');
    if (KNOWN_GOOD.has(rel)) continue;

    let content;
    try {
      content = read(file);
    } catch (e) {
      violations.push({ file: rel, line: 0, reason: `cannot read: ${e.message}` });
      continue;
    }

    const lines = content.split('\n');
    for (let i = 0; i < lines.length; i++) {
      if (RAW_WRITE_PATTERN.test(lines[i])) {
        violations.push({ file: rel, line: i + 1, reason: 'raw SQL token write' });
      }
    }
  }
  return violations;
}

function main() {
  const files = lsFiles(SCAN_PATHSPEC);

  if (files.length === 0) {
    error('forbid-sql-raw: pathspec matched zero files — the scan pathspec is broken, not the tree.');
    process.exit(1);
  }
  log(`forbid-sql-raw: scanning ${files.length} file(s).`);

  const violations = scanFiles(files);
  for (const v of violations) {
    const loc = `${v.file}:${v.line}`;
    error(
      `${loc}: ${v.reason} outside the approved Frag-constructor layer. ` +
      `Use typed Frags (Call / binOp / Cast / Paren / InlineLit / …) instead. ` +
      `If this site is a legitimate new addition to the emitter layer, add it to ` +
      `KNOWN_GOOD in .github/scripts/forbid-sql-raw.mjs with a rationale comment ` +
      `(see CLAUDE.md § "No raw SQL strings" and #1441).`,
    );
  }

  if (violations.length > 0) {
    log(`forbid-sql-raw: ${violations.length} raw-write violation(s) found.`);
    process.exit(1);
  }

  log('forbid-sql-raw: no raw SQL token writes outside the approved layer.');
  process.exit(0);
}

// The scan runs only when this file is the program. forbid-sql-raw.test.mjs
// imports the pure halves above and would otherwise trip the zero-file guard
// (and `git ls-files`) at import time.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
