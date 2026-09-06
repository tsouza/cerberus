// verify-just-invocations.mjs — the CI-safety gate for the Justfile split
// (issue #3093, epic #3091).
//
// WHAT THIS REPLACES: a plain `just --show <recipe>` existence check only
// proves the recipe NAME still resolves. It is blind to an ARITY change —
// a recipe like `_pull-retry +IMAGES` losing its variadic parameter, or a
// recipe gaining a mandatory one — because `--show` prints the recipe
// unconditionally, without binding any of the arguments a real call site
// would pass. `_pull-retry` alone has 31 internal `@just` call sites inside
// the Justfile plus one direct external call from a workflow `run:` step
// (`.github/workflows/e2e.yml`), so an arity regression there would report
// green under `--show` and fail at CI time regardless.
//
// WHAT THIS DOES: scans every `.github/workflows/*.yml` file, extracts ONLY
// the literal shell text of each step's `run:` value (never a YAML comment,
// a step `name:`, or any other YAML content), finds every `just <recipe>
// [args...]` invocation inside that text, and validates each with
// `just --dry-run <exact invocation>` — which binds the recipe's parameters
// exactly as a real invocation would (catching both a missing recipe and an
// arity mismatch) without ever executing the recipe body, so the scan has
// no side effects.
//
// WHY NOT A NAIVE WHOLE-FILE GREP: verified live against this repo before
// this script existed — grepping the raw YAML text for `just <word>` finds
// 25 matches that are plain English inside YAML comments ("not just
// string-asserted", "just lands after the spec's own timeout", …) or prose
// inside an echoed error message inside a REAL run: step (e.g. `run: |` …
// `echo "... run 'just gen-opt-docs' and commit the result"`). Both classes
// are excluded here: the first because only `run:` step VALUES are ever
// scanned, never a YAML comment; the second because every quoted shell
// string and every `${{ … }}` GitHub Actions expression inside a `run:`
// value is neutralised to a single opaque token before the `just` scan runs
// — a quoted argument to a REAL invocation (`just _pull-retry "$CH_IMAGE"`)
// still counts as exactly one positional argument, but the word `just`
// spelled out INSIDE a quoted echo message no longer appears as a
// standalone word for the scan to find.
//
// This is meant to be the acceptance gate for every later phase of the
// Justfile-modularization epic, not a one-off: it is invoked the same way
// (`node .github/scripts/verify-just-invocations.mjs`) regardless of which
// `just/*.just` file a recipe physically lives in, because `import` merges
// every file into one flat recipe namespace `just --dry-run` resolves
// against directly.
//
// Env contract:
//   REPO_ROOT  optional; repo root to scan and to run `just` from
//              (default: process.cwd(), i.e. a runner's checkout root or a
//              contributor's own working tree).
//   JUST_BIN   optional; the `just` binary to invoke (default: "just").
//
// Exit codes: 0 = every extracted invocation dry-runs clean, 1 = at least
// one invocation is broken (missing recipe or arity mismatch), each named
// with its originating file:line and the exact command text extracted.

import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import process from 'node:process';
import { capture, error, notice } from './lib/gh.mjs';

const WORKFLOWS_DIR = '.github/workflows';

// listWorkflowFiles — every `*.yml` / `*.yaml` directly under
// .github/workflows/, sorted for deterministic output. Not recursive: this
// repo has no nested workflow directories, and GitHub Actions itself never
// reads one.
export function listWorkflowFiles(repoRoot) {
  const dir = join(repoRoot, WORKFLOWS_DIR);
  return readdirSync(dir)
    .filter((f) => f.endsWith('.yml') || f.endsWith('.yaml'))
    .sort()
    .map((f) => join(dir, f));
}

// extractRunBlocks — pull every `run:` step's literal shell text out of a
// workflow YAML source, as { text, startLine } records. Handles both the
// single-line form (`run: just foo`) and the block-scalar form (`run: |` /
// `run: >`, with an optional chomp indicator), using indentation to find
// the block's extent exactly the way YAML itself does — no YAML parser
// dependency (invariant 15: node: builtins only).
//
// Deliberately ignorant of everything else in the file: a `run:` that is
// itself the value of some OTHER key (impossible in a well-formed GitHub
// Actions workflow — `run:` is always a step key) is not a case actionlint
// would ever let merge, so this scan does not need to defend against it.
export function extractRunBlocks(source) {
  const lines = source.split('\n');
  const blocks = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const m = /^(\s*)run:[ \t]?(.*)$/.exec(line);
    if (!m) {
      i++;
      continue;
    }
    const indent = m[1].length;
    const rest = m[2].replace(/\r$/, '');
    const blockScalar = /^([|>][+-]?)[ \t]*(#.*)?$/.exec(rest.trim());
    if (blockScalar) {
      // Block scalar: gather every following line indented deeper than the
      // `run:` key itself, up to (not including) the first line that is
      // blank-or-not but indented at or shallower — the end of this step's
      // value, per YAML's own block-scalar indentation rule.
      const startLine = i + 2; // 1-indexed line number of the block's first content line
      i++;
      const contentLines = [];
      let contentIndent = null;
      while (i < lines.length) {
        const l = lines[i].replace(/\r$/, '');
        if (l.trim() === '') {
          contentLines.push('');
          i++;
          continue;
        }
        const curIndent = /^[ \t]*/.exec(l)[0].length;
        if (curIndent <= indent) break;
        if (contentIndent === null) contentIndent = curIndent;
        contentLines.push(l.slice(Math.min(curIndent, contentIndent)));
        i++;
      }
      blocks.push({ text: contentLines.join('\n'), startLine });
      continue;
    }
    if (rest.trim() !== '') {
      // Single-line form. Strip a whole-value YAML double-quote wrapper —
      // `run: "just foo"` — the only quoting style this repo's workflows
      // use for a single-line run:, if ever; bash-level quoting inside the
      // command text is handled later by neutralizeShellText().
      let text = rest.trim();
      if (text.length >= 2 && text.startsWith('"') && text.endsWith('"')) {
        text = text.slice(1, -1);
      }
      blocks.push({ text, startLine: i + 1 });
    }
    i++;
  }
  return blocks;
}

// neutralizeShellText — collapse every GitHub Actions `${{ … }}` expression
// and every quoted shell string to a same-length run of a filler word
// character, character-for-character, so:
//   (a) the RESULT string has the exact same length (and the exact same
//       newline positions) as the input, keeping offset-based line lookups
//       exact even across a replaced span;
//   (b) a quoted argument to a real `just` call (`"$CH_IMAGE"`,
//       `${{ matrix.scenario }}`) still counts as exactly one shell word,
//       preserving arity;
//   (c) the literal substring "just" spelled out INSIDE a quoted echo
//       message, or inside a `${{ }}` expression, can never be mistaken for
//       an invocation, because it no longer spells "just" anywhere in the
//       neutralized text.
// A bash comment (an unquoted `#` preceded by start-of-line or whitespace)
// is then blanked to end-of-line the same way, so a `just` mentioned only in
// an inline shell comment is likewise never treated as an invocation.
export function neutralizeShellText(text) {
  const out = text.split('');
  const blank = (start, end, ch) => {
    for (let k = start; k < end; k++) if (out[k] !== '\n') out[k] = ch;
  };
  for (const m of text.matchAll(/\$\{\{[\s\S]*?\}\}/g)) blank(m.index, m.index + m[0].length, 'G');
  for (const m of text.matchAll(/"(?:[^"\\]|\\.)*"/g)) blank(m.index, m.index + m[0].length, 'Q');
  for (const m of text.matchAll(/'[^']*'/g)) blank(m.index, m.index + m[0].length, 'Q');
  // Comment stripping runs on the quote/expression-neutralized text: a `#`
  // that was inside a quoted string has already become a 'Q' above, so any
  // literal `#` still present here is a real, unquoted shell comment.
  const neutralized = out.join('');
  const lines = neutralized.split('\n');
  const stripped = lines.map((l) => l.replace(/(^|\s)#.*$/, '$1'));
  return stripped.join('\n');
}

// findInvocations — every `just <recipe> [args...]` command-position
// invocation in one `run:` block's neutralized text, as
// { recipe, args, offset } records. "Command position" means the `just`
// token is preceded (ignoring whitespace) by start-of-text or one of
// `& | ; \n ( {` — never by an arbitrary other word — so `just` used as a
// plain English word inside an already-neutralized quoted string can still
// never surface here even if neutralization somehow missed it, and a
// command like `time just foo` (where `just` is the SECOND word of its own
// pipeline segment, not the first token overall) still counts, since the
// character immediately before it is a space preceded by "time" — wait,
// no: "time just foo" — `just` is preceded by a space, and the character
// before THAT space is "e" (not an operator) — so this intentionally does
// NOT flag `time just foo` as an invocation. No call site in this repo
// wraps `just` in another command, so this stays a real constraint rather
// than dead code.
export function findInvocations(neutralizedText) {
  const invocations = [];
  const re = /\bjust\b/g;
  let m;
  while ((m = re.exec(neutralizedText)) !== null) {
    const start = m.index;
    let p = start - 1;
    // Only horizontal whitespace is skipped here — a newline is ITSELF a
    // valid statement boundary in shell, exactly like `;`, so it must stop
    // the backward scan rather than being skipped over in search of one.
    while (p >= 0 && (neutralizedText[p] === ' ' || neutralizedText[p] === '\t')) p--;
    const precedingOk = p < 0 || '&|;({\n'.includes(neutralizedText[p]);
    if (!precedingOk) continue;
    // Find the end of this shell segment: the next unquoted operator or
    // redirection, or end of text. Quotes/expressions are already
    // neutralized, so a plain character scan is safe here.
    let end = neutralizedText.length;
    const terms = /&&|\|\||[;|<>\n]/g;
    terms.lastIndex = re.lastIndex;
    const tm = terms.exec(neutralizedText);
    if (tm) end = tm.index;
    const argsText = neutralizedText.slice(re.lastIndex, end).trim();
    const tokens = argsText.length > 0 ? argsText.split(/\s+/) : [];
    if (tokens.length === 0) continue; // bare `just` (no recipe) — not a recipe invocation
    const [recipe, ...args] = tokens;
    if (recipe.startsWith('-')) continue; // a flag to just itself (`just --list`), not a recipe call
    invocations.push({ recipe, args, offset: start });
  }
  return invocations;
}

// lineForOffset — 1-indexed line number of a character offset within
// `text`, given the 1-indexed line number `startLine` of text's own first
// line. Offsets and newline positions in the neutralized text are identical
// to the original block text (neutralizeShellText never inserts or removes
// characters), so this is exact, not approximate.
export function lineForOffset(text, offset, startLine) {
  let line = startLine;
  for (let k = 0; k < offset && k < text.length; k++) {
    if (text[k] === '\n') line++;
  }
  return line;
}

// collectInvocations — every `just` invocation found across every `run:`
// step of one workflow file, as { recipe, args, file, line } records.
export function collectInvocations(file, source) {
  const found = [];
  for (const block of extractRunBlocks(source)) {
    const neutralized = neutralizeShellText(block.text);
    for (const inv of findInvocations(neutralized)) {
      found.push({
        recipe: inv.recipe,
        args: inv.args,
        file,
        line: lineForOffset(block.text, inv.offset, block.startLine),
      });
    }
  }
  return found;
}

// dryRun — `just --dry-run <recipe> [args...]` from `cwd`. Never executes a
// recipe body (that is the entire point of `--dry-run`): it only resolves
// the recipe name and binds its parameters, so this call has no side
// effects regardless of what the recipe itself would otherwise do.
export function dryRun(justBin, cwd, recipe, args) {
  return capture(justBin, ['--dry-run', recipe, ...args], { cwd });
}

function isMain() {
  const invoked = process.argv[1] || '';
  return invoked.endsWith('verify-just-invocations.mjs');
}

if (isMain()) {
  const repoRoot = process.env.REPO_ROOT || process.cwd();
  const justBin = process.env.JUST_BIN || 'just';

  const allInvocations = [];
  for (const file of listWorkflowFiles(repoRoot)) {
    const source = readFileSync(file, 'utf8');
    const relFile = file.startsWith(repoRoot) ? file.slice(repoRoot.length + 1) : file;
    allInvocations.push(...collectInvocations(relFile, source));
  }

  // Dedupe the actual `just --dry-run` calls by recipe+args — the same
  // invocation commonly recurs across a workflow's job matrix — but keep
  // every originating occurrence so a failure names each file:line it came
  // from, not just the first one found.
  const byKey = new Map();
  for (const inv of allInvocations) {
    const key = `${inv.recipe} ${inv.args.join(' ')}`;
    if (!byKey.has(key)) byKey.set(key, { recipe: inv.recipe, args: inv.args, occurrences: [] });
    byKey.get(key).occurrences.push(`${inv.file}:${inv.line}`);
  }

  const failures = [];
  for (const entry of byKey.values()) {
    const res = dryRun(justBin, repoRoot, entry.recipe, entry.args);
    if (res.status !== 0) {
      failures.push({ ...entry, stderr: res.stderr.trim() || res.stdout.trim() });
    }
  }

  if (failures.length > 0) {
    for (const f of failures) {
      const invocation = ['just', f.recipe, ...f.args].join(' ');
      error(`broken invocation \`${invocation}\` (${f.occurrences.join(', ')}): ${f.stderr}`, {
        title: 'verify-just-invocations',
      });
    }
    error(`verify-just-invocations: ${failures.length} broken invocation(s) out of ${byKey.size} distinct call(s)`);
    process.exit(1);
  }

  notice(
    `verify-just-invocations: ${byKey.size} distinct invocation(s) (${allInvocations.length} call site(s) across ${
      listWorkflowFiles(repoRoot).length
    } workflow file(s)) all dry-run clean`,
  );
  process.exit(0);
}
