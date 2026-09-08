// forbid-verbatim-concat.mjs — gate that flags verbatim() calls built via
// string concatenation, the shape-building misuse invariant 10 forbids.
//
// verbatim(sql string) Frag exists for emitter-chosen SYNTHETIC TOKENS — a
// bare alias name, a pre-quoted literal, pre-rendered subquery SQL — never
// for constructing a whole expression shape (a window function, a
// comparison, an ORDER BY list) by hand. A single synthetic token never
// needs Go's `+` string-concatenation operator; any verbatim(...) call
// whose argument concatenates strings is therefore almost certainly
// building a shape, not naming a token. forbid-sql-raw.mjs's regex already
// catches the raw-write PRIMITIVES (strings.Builder, writeSQL, sb.Write);
// its own doc explicitly says it is blind to verbatim() misuse, and that
// gap is exactly what let the internal/chsql/nested_set_annotate.go /
// structural_join.go call sites (#2297, #2301) go undetected until a full
// manual audit found them.
//
// What is scanned: internal/chsql/**/*.go (non-test, non-builder.go — the
// same pathspec forbid-sql-raw.mjs uses, since verbatim() itself is
// defined, not called, in builder.go).
//
// What is flagged: any verbatim(...) call whose argument contains a
// top-level `+` operator (Go string concatenation) outside a string
// literal.
//
// Exit codes: 0 = clean, 1 = violation found.
//
// This is a text-level heuristic, not a full Go AST parse — consistent
// with forbid-sql-raw.mjs's own approach, and cheap enough to run on every
// PR. A future call site that legitimately needs `+` inside a verbatim()
// argument for some reason this scan hasn't anticipated is not silently
// exempted: add its "file:line" to KNOWN_GOOD below with a rationale
// comment and reviewer sign-off, mirroring forbid-sql-raw.mjs's own
// escape hatch. This is an inventory, not a bypass.
//
// See CLAUDE.md § "No raw SQL strings — typed chsql API only" (invariant
// 10) and #2297.

import { lsFiles, error, log } from './lib/gh.mjs';
import { readFileSync } from 'node:fs';
import process from 'node:process';

// KNOWN_GOOD — argument texts pre-approved to concatenate inside a verbatim()
// call, keyed by (file, argument text) rather than by line number.
//
// Each entry is a synthetic-token concatenation: gluing an emitter-chosen,
// never-user-controlled alias (a bare single letter like `L`/`R`) to FIXED
// punctuation to form one simple qualified-identifier-like token (`L.*`, `L.`,
// ` AS L`). That is "a synthetic token" per invariant 10's own definition, not
// a whole expression SHAPE: no operators, no function calls, no
// ORDER BY/PARTITION BY/frame clauses — the `+` stitches two inert strings.
//
// WHY THIS IS KEYED ON CONTENT (#3187). It used to be keyed on "file:line",
// which is brittle in both directions: any edit ABOVE an entry either breaks
// the gate spuriously (the real violation moves off its key and the build goes
// red with a misleading message) or, worse, silently exempts whatever unrelated
// text drifts ONTO that line. The second half was not hypothetical — two
// further entries pointed at nested_set_annotate.go:404 and :414 as tracked
// debt from #2297, that debt was paid by PR #2319, and the entries were left
// behind pointing at `As(` and `From(events.Frag())`. They exempted nothing
// they described and stood ready to exempt anything that landed there next.
// A key made of the text being exempted cannot drift away from it.
//
// There is deliberately no "tracked debt" category any more. A pre-existing
// shape-building violation is a bug to fix, not an inventory line: the two
// entries that category ever held outlived their fix, which is the argument
// against having it.
const KNOWN_GOOD = [
  { file: 'internal/chsql/structural_join.go', arg: 'side+".*"' }, //      starExceptKeys
  { file: 'internal/chsql/vector_join.go', arg: 'side + "."' }, //         qualColFrag
  { file: 'internal/chsql/vector_join.go', arg: '" AS " + bareAlias' }, // aliasedFrag
];

const KNOWN_GOOD_KEYS = new Set(KNOWN_GOOD.map((e) => knownGoodKey(e.file, e.arg)));

// knownGoodKey — the (file, normalized argument text) identity of one exempt
// site. Whitespace is collapsed so a gofmt reflow of the argument does not
// break the key, but nothing else is normalized: a change to which identifiers
// or which literal are concatenated is a DIFFERENT expression and must face
// the gate again rather than inherit an exemption.
function knownGoodKey(file, argText) {
  return `${file}::${argText.replace(/\s+/g, ' ').trim()}`;
}

// findVerbatimCalls() walks content once, locating each `verbatim(` call
// and extracting its full argument text (balanced across parens and,
// where the call wraps multiple lines, across newlines too) plus the
// 1-based line the call starts on.
function findVerbatimCalls(content) {
  const calls = [];
  const callPattern = /\bverbatim\(/g;
  let match;
  while ((match = callPattern.exec(content)) !== null) {
    const argStart = match.index + match[0].length;
    const { argText, end } = readBalancedArgs(content, argStart);
    if (end === -1) continue; // unterminated — malformed source, not this gate's job
    const line = content.slice(0, match.index).split('\n').length;
    calls.push({ line, argText });
  }
  return calls;
}

// readBalancedArgs() scans forward from `start` (just past the opening
// paren already consumed by the caller) tracking paren depth and Go
// string-literal state, returning the argument text up to the matching
// close paren. String-literal-aware so a `)` or `+` inside a quoted
// string never mistakenly ends the scan or counts as concatenation.
function readBalancedArgs(content, start) {
  let depth = 1; // the opening paren the caller already matched
  let i = start;
  let inString = null; // '"', '`', or null
  while (i < content.length && depth > 0) {
    const c = content[i];
    if (inString === '"') {
      if (c === '\\') { i += 2; continue; }
      if (c === '"') inString = null;
    } else if (inString === '`') {
      if (c === '`') inString = null;
    } else {
      if (c === '"' || c === '`') inString = c;
      else if (c === '(') depth++;
      else if (c === ')') depth--;
    }
    i++;
  }
  if (depth !== 0) return { argText: '', end: -1 };
  return { argText: content.slice(start, i - 1), end: i };
}

// hasTopLevelConcat() reports whether argText contains a Go `+` operator
// outside any string literal — the only way a single-string-parameter
// call like verbatim(sql string) legitimately contains a bare `+`.
function hasTopLevelConcat(argText) {
  let inString = null;
  for (let i = 0; i < argText.length; i++) {
    const c = argText[i];
    if (inString === '"') {
      if (c === '\\') { i++; continue; }
      if (c === '"') inString = null;
      continue;
    }
    if (inString === '`') {
      if (c === '`') inString = null;
      continue;
    }
    if (c === '"' || c === '`') { inString = c; continue; }
    if (c === '+') return true;
  }
  return false;
}

// A bare `**` pathspec needs git's `glob` pathspec magic to mean "any
// number of directories, including zero" — without it (no `:(glob)`
// prefix, no `core.globPathspecs=true`), `**` is matched as plain fnmatch
// wildcards, which still requires a literal `/` to appear right where the
// pattern has one. That silently matches zero files for every path that
// has no subdirectory below the prefix (exactly `internal/chsql/*.go`'s
// shape, since the package has no subpackages) while still "succeeding"
// with an empty result — a scan that reports "clean" without ever having
// looked at anything. `:(glob)` makes the intended "any depth" meaning
// explicit and unambiguous regardless of ambient git config.
const files = lsFiles([
  ':(glob)internal/chsql/**/*.go',
  ':!:internal/chsql/builder.go', // defines verbatim(), by design — not a caller
  ':(exclude,glob)internal/chsql/**/*_test.go',
]);

log(`forbid-verbatim-concat: scanning ${files.length} file(s).`);
if (files.length === 0) {
  error('forbid-verbatim-concat: pathspec matched zero files — the scan pathspec is broken, not the tree.');
  process.exit(1);
}

let violations = 0;
const matchedExemptions = new Set();

for (const file of files) {
  const rel = file.replace(/\\/g, '/');

  let content;
  try {
    content = readFileSync(file, 'utf8');
  } catch (e) {
    error(`forbid-verbatim-concat: cannot read ${file}: ${e.message}`);
    violations++;
    continue;
  }

  for (const { line, argText } of findVerbatimCalls(content)) {
    const loc = `${rel}:${line}`;
    const key = knownGoodKey(rel, argText);
    if (KNOWN_GOOD_KEYS.has(key)) {
      matchedExemptions.add(key);
      continue;
    }
    if (hasTopLevelConcat(argText)) {
      error(
        `${loc}: verbatim(...) call built via string concatenation — this is shape-building, ` +
        'not a synthetic token, and violates invariant 10 (no raw SQL strings). Use typed Frags ' +
        '(Call / Window / WindowFrame / Eq / Lt / InlineLit / …) to express the shape instead. ' +
        'If this really is a legitimate synthetic token, add ' +
        `knownGoodKey(${JSON.stringify(rel)}, ${JSON.stringify(argText)}) to KNOWN_GOOD in ` +
        '.github/scripts/forbid-verbatim-concat.mjs with a rationale comment and reviewer ' +
        'sign-off (see CLAUDE.md § "No raw SQL strings" and #2297).',
      );
      violations++;
    }
  }
}

if (violations > 0) {
  log(`forbid-verbatim-concat: ${violations} violation(s) found.`);
  process.exit(1);
}

// Every KNOWN_GOOD entry whose FILE this scan actually read must have matched a
// real call. An entry that matches nothing is a stale exemption: it protects
// nothing it names, and stands ready to protect whatever text replaces it. That
// is precisely how two entries citing nested_set_annotate.go survived the fix
// that made them unnecessary (#2297 / PR #2319) and ended up pointing at `As(`
// and `From(events.Frag())`.
//
// Scoped to files the scan read, because this script is also run against
// synthetic single-file trees by its own unit tests; an entry for a file that
// is not in THIS tree is inapplicable, not stale.
const scanned = new Set(files.map((f) => f.replace(/\\/g, '/')));
const stale = KNOWN_GOOD.filter(
  (e) => scanned.has(e.file) && !matchedExemptions.has(knownGoodKey(e.file, e.arg)),
);
if (stale.length > 0) {
  for (const e of stale) {
    error(
      `forbid-verbatim-concat: stale KNOWN_GOOD entry — ${e.file} no longer contains a ` +
      `verbatim() call whose argument is \`${e.arg}\`. It exempts nothing it names and would ` +
      'exempt whatever replaces it; delete the entry.',
    );
  }
  process.exit(1);
}

log(
  `forbid-verbatim-concat: no verbatim() call builds a shape via concatenation ` +
  `(${matchedExemptions.size} synthetic-token exemption(s) matched).`,
);
process.exit(0);
