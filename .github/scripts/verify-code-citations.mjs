// verify-code-citations.mjs — every source citation in a Go comment or string
// must name a construct that still exists, not a line number that has drifted.
//
// WHY THIS EXISTS (#2953). Mutation-adjudication notes cite the mutant they are
// about: `NOT KILLABLE` footers, `// TestX kills foo.go:…` headers, the
// `CONDITIONALS_BOUNDARY at bar.go:…` strings inside `t.Fatalf`. Those citations
// are the only way a reviewer, or a later agent, can re-check an equivalence
// claim. A bare `file.go:613` is UNVERIFIABLE by machine — nothing about an
// integer can be checked against intent — and it is invalidated by any edit that
// inserts a line above it. Measured over the 200 first-parent commits before
// this gate landed, a line-number citation would have been invalidated 575
// times, and in 573 of those the construct it named was never touched: the
// citation rotted purely because unrelated lines moved. 33% of the citations on
// `main` had already rotted onto a comment or a blank line.
//
// THE FIX is to make the citation name the thing rather than the address:
//
//     range_window.go:`numAnchors-1`                  construct, unique in file
//     range_window.go:emitRangeWindow:`numAnchors-1`  construct, unique in func
//
// which a gate CAN check — resolve the file, search its code lines, require
// exactly one match. Naming the construct removes the drift class rather than
// detecting it: inserting a line above the construct changes nothing, and the
// only edit that invalidates the citation is one that changes the construct
// itself — exactly when a human must re-read the note.
//
// FAIL CLOSED. Anything that looks like a citation and is not a well-formed one
// is an error, never a skip:
//   - `foo.go:613`, `foo.go:613:22`, `foo.go:108-109` — line-number citations.
//   - a construct whose file does not resolve, including an upstream path such
//     as `promql/engine.go:2114`. An upstream line number is unverifiable from
//     this repository, so the convention forbids the form outright: name the
//     upstream construct in prose instead.
//   - a construct that matches no code line, or more than one.
//   - an unterminated construct, or an empty one.
// The scope is a git pathspec set — every Go file `git ls-files` reports as
// tracked or as untracked-but-not-ignored — rather than a hand-maintained list
// of files, so it cannot silently shrink the way an enumerated set does.
//
// SEARCH SPACE. Only CODE lines of the cited file are searched — blank lines and
// whole-line `//` comments are excluded. That is what makes the pre-existing
// provable-drift class (a citation resolving to a comment or a blank line)
// unrepresentable rather than merely detected, and it lets a construct be quoted
// in its own doc comment without colliding with itself.
//
// THREE PLACES A NOTE NAMES CODE. The backticked construct above is one of
// them, and the two others rot the same way while the resolver never looks at
// them. This gate checks all three rather than growing a sibling per shape.
//
// 2. A LINE ADDRESS WRITTEN AS PROSE (#2964). The `file.go:613` spelling is
//    banned, so the same address survives as `line 613`, `line 92:20` or
//    `lines 273-276` — carrying no filename at all in the worst case, which is
//    strictly less verifiable than the form already rejected. Tree-wide at the
//    commit this landed, 72 of these existed and every one was a genuine source
//    address; the remedy is the one the convention already prescribes, name the
//    construct, and for the many that merely decorate a resolving citation
//    ("col 27 flips `call.Func != nil`") it is to drop the number, because the
//    prose already names the token. Two spellings are deliberately NOT modelled,
//    on measurement rather than taste: `column N` spelled out, which was a
//    ClickHouse result column or a text column in 10 of 10 occurrences and never
//    a source address (`col N` was a source address in 11 of 11), and a line
//    address inside a tab-indented block, which is quoted material rather than
//    the note's own prose and belongs to shape 3.
//
// 3. A CODE BLOCK QUOTED UNDER A CITATION (#2969). The prevailing note shape is
//    a citation followed by a tab-indented block quoting the construct in situ.
//    The backticked span resolves, so the gate passes, while the block below it
//    can show code that is not in the cited file at all.
//
//    Only the PROVABLE half of that is gateable, and the split is the whole
//    design. "The block does not resolve in the cited file" cannot be checked:
//    a block that resolves nowhere is indistinguishable from the mutant's
//    rewritten form, which is the entire point of an adjudication note, and from
//    an elision or a caret-pointer rule. Measured against the tree that carried
//    the three real instances, that rule flagged 10 and 3 were genuine — 30%,
//    and 0% on the tree after they were fixed. It is refused, and the refusal is
//    recorded in `docs/test-strategy.md` beside #2957's and #2966's.
//
//    What IS provable is MIS-ATTRIBUTION: the block quotes a line that exists in
//    this repository, in a file OTHER than the one the citation names. A reader
//    checking the block against the cited file will not find it, and no mutant
//    form, elision or diagram can be mistaken for it — those resolve nowhere, so
//    they are never asked about. That rule flags 3 of 3 on the pre-fix tree and
//    0 on the tree today: a ratchet with no backlog, which is what it is for.
//
//    Two shapes are excluded because they name nothing to attribute: a block
//    line that is ITSELF a citation (an indented citation LIST, which the naive
//    reading gets wrong 23 times out of 23), and a line carrying no identifier
//    character at all — a bare `...`, a lone brace.
//
// ENV CONTRACT
//   REPO_ROOT   — repository root. Default `process.cwd()`.
//   PATHSPECS   — whitespace-separated git pathspecs to scan.
//                 Default `*.go` (every tracked Go file).
//
// Exit: 0 when every citation resolves, 1 otherwise.

import { readFileSync, existsSync, statSync } from 'node:fs';
import { isAbsolute, join, dirname, resolve, relative } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { error, log, notice, lsFiles } from './lib/gh.mjs';

// The construct delimiter. Backticks already mark code spans in Go comments and
// are legal inside the double-quoted strings `t.Fatalf` messages use, so the
// citation reads the same in both places it appears.
const DELIM = '`';

// A citation opens at `.go:`. The grammar models exactly two shapes and ignores
// every other follower:
//
//   `.go:<digit>`                       the banned line-number form
//   `.go:DELIM…DELIM`                   a construct citation
//   `.go:<ident>:DELIM…DELIM`           a construct citation scoped to a func
//
// Everything else is left alone because it carries no address and therefore
// cannot suffer the drift this gate closes: ordinary prose ("see
// range_window.go: the anchor grid…"), a bare function reference
// (`lower.go:wrapRangeLatestPerSeries`), and the rejection-parity catalogue's
// site identifiers (`internal/promql/lower.go:lowerHistogramNativeRoot#53e0f9b3`),
// which are a DATA FORMAT rather than a citation — catalogue_test.go builds
// deliberately fabricated ones (`internal/promql/a.go:fnA#0000aaaa`) that no
// resolver should ever be asked to resolve.
const OPENER = /\.go:/g;

// PROSE_ADDRESS — shape 2: an address carrying an explicit ADDRESS MARKER, in
// the two spellings the tree uses — the words `line`/`col`, and an `@` that is
// not preceded by a digit. The trailing character class swallows the whole
// address rather than its first number, so `line 92:20` and `lines 273-276`
// are reported as written.
//
// The marker is what makes the address identifiable, and its absence is why an
// UNMARKED pair (`223:11`, `at 76:23`) is not modelled here. That is measured,
// not overlooked. The 32 that existed were repointed by hand (#2981); the 383
// bare pairs left in note scope are all data — `1:1` correspondences, clock
// times in chDB fixtures (`23:59:30`), slice bounds (`src[0:0]`) and a
// `host:9000`. Nothing lexical separates `76:23` the address from `23:59` the
// timestamp. The closest rule that works — the pair, scoped to a unit also
// carrying mutation vocabulary — flagged 36 pre-cleanup (30 real, 6 prose)
// while MISSING both `// --- file.go … ---` section headers, and on the tree
// today it flags those 6 and nothing else: the false-positive machine #2966
// refused. `docs/test-strategy.md` records the measurement, and it is not an
// escape hatch: the unmarked pair is the SAME unverifiable address, left to
// the reviewer rather than blessed.
//
// The HYPHENATED attributive form (`the line-116 condition`, `both line-277
// mutants`) is out for the same reason and measured the same way: of the four
// in note scope, three are `line-3`/`line-4`/`line-5` — the NAMES of log lines
// in an internal/api/loki fixture, which no rewrite improves. Requiring a
// space after the keyword costs the one real instance, which this change
// repoints by hand, and buys immunity to the three.
//
// `column`/`columns` spelled out is likewise absent by measurement — see the
// header — as is a digit-prefixed `@`, which is a chDB fixture's `500@00:12`
// value-at-timestamp and never an address.
const PROSE_ADDRESS = /\b(?:lines?|cols?)\s+[0-9]+(?:[:/,-][0-9]+)*|(?<![0-9])@[0-9]+:[0-9]+/gi;

// FAILURE_CALL — the testing report calls whose message text is the second
// place this repo writes an adjudication note, alongside the Go comment. A
// prose address is checked THERE and not in every string literal, because a
// string that is not a note carries no address that can rot: the parse-error
// text `test/e2e/migration/lib` and `internal/logql/lsyntax` assert on says
// "line 5, col 0" about the USER'S input, and no rewrite of it is available or
// wanted.
const FAILURE_CALL = /\bt\.(?:Fatal|Fatalf|Error|Errorf|Log|Logf|Skip|Skipf)\s*\(/;

// QUOTED_BLOCK_INDENT — what marks a quoted code block inside a Go comment.
// gofmt normalises a doc-comment code block to a leading tab, and the repo's
// notes follow it; a `  - ` bullet and its space-aligned continuations are
// prose, not a block, which is why the marker is the tab and not indentation
// in general.
const QUOTED_BLOCK_INDENT = '\t';

// commentBody — the text after `//` on a whole-line comment, or null. The
// leading whitespace is KEPT: a tab there is what separates a quoted code block
// from the note's own prose, and both shapes above turn on that distinction.
export function commentBody(line) {
  const trimmed = line.trim();
  if (!trimmed.startsWith('//')) return null;
  return trimmed.slice(2);
}

// isQuotedBlock — whether a comment body is a quoted code block rather than
// prose.
export function isQuotedBlock(body) {
  return body.startsWith(QUOTED_BLOCK_INDENT);
}

// proseAddresses — every prose line/column address in a piece of text.
export function proseAddresses(text) {
  return [...text.matchAll(PROSE_ADDRESS)].map((m) => m[0]);
}

// splitCode — one source line separated into the code OUTSIDE string literals
// and the contents of the literals themselves. Both halves are needed and
// neither substitutes for the other: the call span below counts parentheses,
// which must not see a `(` inside a message, and the address scan reads message
// text, which must not see the code around it. `state.inRaw` carries a raw
// string across the line break it is allowed to span.
// Runs are sliced out whole rather than accumulated character by character:
// the scan walks every line of every tracked Go file, and per-character string
// building made it the slowest step in the gate.
export function splitCode(line, state) {
  const strings = [];
  let code = '';
  let inString = state.inRaw;
  let mark = 0; // where the current run — code, or string content — began
  let i = 0;
  while (i < line.length) {
    const ch = line[i];
    if (inString) {
      if (state.inRaw) {
        if (ch === '`') {
          strings.push(line.slice(mark, i));
          inString = false;
          state.inRaw = false;
          mark = i + 1;
        }
        i += 1;
        continue;
      }
      if (ch === '\\') {
        i += 2;
        continue;
      }
      if (ch === '"') {
        strings.push(line.slice(mark, i));
        inString = false;
        mark = i + 1;
      }
      i += 1;
      continue;
    }
    // A rune literal is skipped whole. `if ch == '"' {` would otherwise open a
    // string on the quote inside it and swallow the rest of the file.
    if (ch === "'") {
      code += line.slice(mark, i);
      i += line[i + 1] === '\\' ? 4 : 3;
      mark = i;
      continue;
    }
    if (ch === '"' || ch === '`') {
      code += line.slice(mark, i);
      inString = true;
      state.inRaw = ch === '`';
      mark = i + 1;
      i += 1;
      continue;
    }
    if (ch === '/' && line[i + 1] === '/') {
      code += line.slice(mark, i);
      return { code, strings }; // the rest of the line is a trailing comment
    }
    i += 1;
  }
  // A raw string may carry on past the line break, so its fragment is taken
  // here rather than waiting for a close that belongs to a later line. An
  // interpreted string cannot: an unterminated one is a compile error, and
  // closing it here keeps the state from leaking onward.
  if (inString) strings.push(line.slice(mark));
  else code += line.slice(mark);
  return { code, strings };
}

// failureMessages — 1-based line -> the string literals on that line, for every
// line inside a testing report call. The span runs from the call's opening
// parenthesis to the one that balances it, counted on the code half only, so a
// `+`-concatenated message spanning five lines is one span and a parenthesis
// inside the message text cannot open or close it.
export function failureMessages(lines) {
  const out = new Map();
  const state = { inRaw: false };
  let depth = 0;
  lines.forEach((line, i) => {
    // Outside a span, a line can only matter if it names a report call, and it
    // can only change the raw-string state if it carries a backtick. Testing
    // both on the raw line first is a superset of the real answer — a match
    // inside a comment or a string just falls through to the full split, which
    // decides correctly — and it skips that split for most of the corpus.
    if (depth === 0 && !state.inRaw && !line.includes('`') && !FAILURE_CALL.test(line)) return;
    const { code, strings } = splitCode(line, state);
    let from = 0;
    if (depth === 0) {
      const opener = FAILURE_CALL.exec(code);
      if (opener === null) return;
      from = opener.index;
    }
    out.set(i + 1, strings);
    for (const ch of code.slice(from)) {
      if (ch === '(') depth += 1;
      else if (ch === ')') depth -= 1;
    }
    if (depth < 0) depth = 0;
  });
  return out;
}

// normalise — collapse every whitespace run to one space so a citation may be
// written with the spacing that reads best, independent of the source's
// indentation and alignment.
export function normalise(text) {
  return text.replace(/\s+/g, ' ').trim();
}

// unescape — undo the escapes a citation can acquire purely by being written
// inside a Go string. A `t.Fatalf` message citing a construct that contains a
// double quote has to spell it `\\"` for the compiler, and one citing a construct
// that contains `%` — a modulo, as in `ns%stepNS != 0` — has to spell it `%%` or
// `go vet` rejects the format string. The gate reads the file as text and would
// otherwise compare those escapes against a source line that never had them.
//
// Both are safe to undo unconditionally rather than only inside a string: `%%`
// is not valid Go anywhere a construct could legitimately contain it, so a
// comment-borne citation cannot mean a literal `%%`. Nothing else is unescaped —
// this undoes the literal's encoding, it does not interpret the construct.
export function unescape(text) {
  return text.replace(/\\(["\\])/g, '$1').replace(/%%/g, '%');
}

// isCodeLine — a line that a mutation operator could apply to. Blank lines and
// whole-line comments are not; a line with a trailing comment is.
export function isCodeLine(line) {
  const t = line.trim();
  return t !== '' && !t.startsWith('//');
}

// funcRanges — the top-level function scopes of a Go file, as
// name -> [{ start, end }] 1-based inclusive line bounds. A top-level `func`
// declaration starts at column 0 and its body closes at the first following
// line that is exactly `}` at column 0; gofumpt is enforced repo-wide, so both
// anchors are reliable. Ending at the closing brace rather than at the next
// declaration matters: otherwise a package-level `var` block sitting between
// two funcs would be searchable under the name of the func above it.
export function funcRanges(lines) {
  const out = new Map();
  lines.forEach((line, i) => {
    const m = /^func\s+(?:\([^)]*\)\s*)?([A-Za-z0-9_]+)/.exec(line);
    if (!m) return;
    let end = lines.length;
    for (let k = i; k < lines.length; k += 1) {
      if (lines[k] === '}') {
        end = k + 1;
        break;
      }
    }
    // A name may be declared more than once across receivers; keep every range
    // so a citation naming it searches all of them and still demands one hit.
    if (!out.has(m[1])) out.set(m[1], []);
    out.get(m[1]).push({ start: i + 1, end });
  });
  return out;
}

// parseCitations — every citation opener in a unit, with what follows it
// classified. Each result carries the source line the citation itself sits on.
export function parseCitations(unit) {
  const { text } = unit;
  const found = [];
  for (const m of text.matchAll(OPENER)) {
    const at = m.index;
    const line = lineAt(unit, at);
    // Walk back over the path: the run of path characters before `.go`.
    const before = text.slice(0, at);
    const pathMatch = /([A-Za-z0-9_./-]+)$/.exec(before);
    const path = `${pathMatch ? pathMatch[1] : ''}.go`;
    const rest = text.slice(at + m[0].length);
    const head = rest[0];

    if (head === undefined) continue; // `.go:` at end of unit — prose.
    if (/[0-9]/.test(head)) {
      found.push({ line, path, kind: 'line-number', detail: rest.slice(0, 24) });
      continue;
    }
    if (head === DELIM) {
      found.push({ line, path, kind: 'construct', scope: null, ...readConstruct(rest.slice(1)) });
      continue;
    }
    const scoped = /^([A-Za-z0-9_]+):/.exec(rest);
    if (scoped && rest[scoped[0].length] === DELIM) {
      found.push({
        line,
        path,
        kind: 'construct',
        scope: scoped[1],
        ...readConstruct(rest.slice(scoped[0].length + 1)),
      });
    }
    // Every other follower carries no address — see the OPENER note above.
  }
  return found;
}

// readConstruct — the delimited construct text starting just past the opening
// delimiter. Returns `{ construct }` or `{ unterminated: true }`.
function readConstruct(after) {
  const close = after.indexOf(DELIM);
  if (close === -1) return { unterminated: true };
  return { construct: after.slice(0, close) };
}

// units — the logical units of a Go file. A run of consecutive whole-line `//`
// comments is one unit, so a citation may wrap across the comment's line breaks;
// every other line is its own unit. Each unit carries a `lines` array parallel
// to `text`, giving the 1-based SOURCE line every character came from — a
// citation inside a twenty-line note has to be reported at its own line, not at
// the line the note happens to start on.
export function units(lines) {
  const out = [];
  let block = null;
  const flush = () => {
    if (block) out.push(block);
    block = null;
  };
  lines.forEach((line, i) => {
    const n = i + 1;
    const raw = commentBody(line);
    if (raw !== null) {
      const body = raw.replace(/^\s?/, '');
      if (!block) block = { text: '', lines: [], comment: true };
      else {
        block.text += ' ';
        block.lines.push(n);
      }
      block.text += body;
      for (let k = 0; k < body.length; k += 1) block.lines.push(n);
      return;
    }
    flush();
    out.push({ text: line, lines: new Array(line.length).fill(n) });
  });
  flush();
  return out;
}

// lineAt — the source line a character index inside a unit came from.
function lineAt(unit, index) {
  return unit.lines[Math.min(index, unit.lines.length - 1)];
}

// resolveCited — the repo-relative path a citation names, or null. Tried
// relative to the citing file's own directory first (the common intra-package
// case), then relative to the repository root (the cross-package case).
export function resolveCited({ root, from, path }) {
  const candidates = [join(dirname(from), path), path];
  for (const c of candidates) {
    const abs = resolve(root, c);
    // Keep the resolution inside the repository: a `../..` path that escapes it
    // is not a citation this gate can vouch for.
    const rel = relative(root, abs);
    if (rel.startsWith('..') || isAbsolute(rel)) continue;
    if (existsSync(abs) && statSync(abs).isFile()) return rel;
  }
  return null;
}

// matchLines — the 1-based code lines of `lines` whose normalised text contains
// the normalised construct, restricted to `ranges` when given.
export function matchLines({ lines, construct, ranges }) {
  const needle = normalise(unescape(construct));
  const hits = [];
  lines.forEach((line, i) => {
    const n = i + 1;
    if (ranges && !ranges.some((r) => n >= r.start && n <= r.end)) return;
    if (!isCodeLine(line)) return;
    if (normalise(line).includes(needle)) hits.push(n);
  });
  return hits;
}

// blockCandidates — the quoted block lines of one comment run that do not
// appear in the file the nearest citation above them names. They are only
// CANDIDATES: whether each is a mis-attribution or an unresolvable form the
// gate deliberately permits (a mutant, an elision) is settled in `scan`'s
// second pass, which is the only place that knows the rest of the repository.
export function blockCandidates({ file, unit, lines, anchors, load }) {
  if (anchors.length === 0) return [];
  const out = [];
  const last = unit.lines[unit.lines.length - 1];
  for (let n = unit.lines[0]; n <= last; n += 1) {
    const body = commentBody(lines[n - 1]);
    if (body === null || !isQuotedBlock(body)) continue;
    const quoted = body.replace(/^\t+/, '');
    // A block line that is itself a citation is an entry in an indented
    // citation LIST, not quoted code. Reading those as code is the obvious
    // naive implementation and it was wrong 23 times out of 23 (#2969).
    if (quoted.includes('.go:')) continue;
    // A line with no identifier names nothing that could be attributed: a bare
    // `...` elision, a lone brace, a `^^^` pointer rule. Without this, `...`
    // matches the variadic parameter of an unrelated function and reports a
    // mis-attribution that is an artefact of substring search.
    if (!/[A-Za-z0-9_]/.test(quoted)) continue;
    const anchor = anchors.filter((a) => a.line < n).sort((a, b) => b.line - a.line)[0];
    if (anchor === undefined) continue;
    const needle = normalise(unescape(quoted));
    if (load(anchor.rel).some((l) => isCodeLine(l) && normalise(l).includes(needle))) continue;
    out.push({ file, line: n, quoted: quoted.trim(), needle, cited: anchor.rel, construct: anchor.construct });
  }
  return out;
}

// checkFile — every violation in one tracked file, plus the quoted-block
// candidates whose verdict needs the whole repository.
export function checkFile({ root, file }) {
  const text = readFileSync(resolve(root, file), 'utf8');
  const lines = text.split('\n');
  const violations = [];
  const candidates = [];
  const cache = new Map();
  const load = (rel) => {
    if (!cache.has(rel)) cache.set(rel, readFileSync(resolve(root, rel), 'utf8').split('\n'));
    return cache.get(rel);
  };

  // Shape 2: a line address written as prose. The two places a note is written
  // are a comment's own prose and a testing report message; a tab-indented
  // comment line is quoted material, governed by shape 3 instead, which is what
  // keeps a verbatim transcript of a tool's own "failed on line 1306" out of
  // this scan.
  const failure = failureMessages(lines);
  lines.forEach((line, i) => {
    const n = i + 1;
    const body = commentBody(line);
    let texts;
    if (body === null) texts = failure.get(n) || [];
    else texts = isQuotedBlock(body) ? [] : [body];
    for (const t of texts) {
      for (const addr of proseAddresses(t)) {
        violations.push(
          `${file}:${n}: prose line address \`${addr}\` — a number addresses code no more verifiably written out than written \`file.go:${addr.replace(/\D+/, '')}\`, and it drifts on every insertion above it. Name the construct (\`file.go:${DELIM}<construct>${DELIM}\`), or drop the number where the prose already names the token.`,
        );
      }
    }
  });

  for (const unit of units(lines)) {
    const anchors = [];
    for (const c of parseCitations(unit)) {
      const where = `${file}:${c.line}`;
      if (c.kind === 'line-number') {
        violations.push(
          `${where}: line-number citation \`${c.path}:${c.detail.trim()}\` — a line number cannot be verified and drifts on every insertion above it. Name the construct: \`${c.path}:${DELIM}<construct>${DELIM}\`.`,
        );
        continue;
      }
      if (c.unterminated) {
        violations.push(`${where}: unterminated construct in \`${c.path}\` citation — no closing ${DELIM}.`);
        continue;
      }
      if (normalise(unescape(c.construct)) === '') {
        violations.push(`${where}: empty construct in \`${c.path}\` citation.`);
        continue;
      }
      const rel = resolveCited({ root, from: file, path: c.path });
      if (rel === null) {
        violations.push(
          `${where}: citation names \`${c.path}\`, which does not resolve to a file in this repository. An out-of-repo line or construct cannot be verified here — name it in prose instead.`,
        );
        continue;
      }
      // The citation's FILE resolved, so it can anchor a quoted block below it
      // even when the construct itself turns out not to match — the block is
      // then checked against the file the note actually means.
      anchors.push({ line: c.line, rel, construct: c.construct });
      const citedLines = load(rel);
      let ranges = null;
      if (c.scope !== null) {
        const found = funcRanges(citedLines).get(c.scope);
        if (!found) {
          violations.push(`${where}: citation scopes to \`${c.scope}\`, which is not a top-level func in ${rel}.`);
          continue;
        }
        ranges = found;
      }
      const hits = matchLines({ lines: citedLines, construct: c.construct, ranges });
      const scopeNote = c.scope === null ? rel : `${rel} func ${c.scope}`;
      if (hits.length === 0) {
        violations.push(
          `${where}: construct \`${c.construct}\` matches no code line in ${scopeNote}. The construct moved or changed — re-read the note before repointing it.`,
        );
        continue;
      }
      if (hits.length > 1) {
        violations.push(
          `${where}: construct \`${c.construct}\` matches ${hits.length} code lines in ${scopeNote} (${hits.join(', ')}). Widen it, or scope it with \`${c.path}:<func>:${DELIM}…${DELIM}\`.`,
        );
      }
    }
    // Shape 3, first half: which quoted block lines are not in the file the
    // citation above them names.
    if (unit.comment) candidates.push(...blockCandidates({ file, unit, lines, anchors, load }));
  }
  return { violations, candidates };
}

// attribute — shape 3, second half. A quoted block line missing from the cited
// file is a violation only when the SAME line lives elsewhere in this
// repository, which is what makes it a provable mis-attribution rather than a
// form the gate has no business adjudicating. A mutant's rewritten form, an
// elided call and a pointer rule all resolve nowhere, so this never asks about
// them — that separation is why the check is shippable at all.
export function attribute({ root, files, candidates }) {
  const homes = candidates.map(() => new Set());
  for (const f of files) {
    for (const line of readFileSync(resolve(root, f), 'utf8').split('\n')) {
      if (!isCodeLine(line)) continue;
      const norm = normalise(line);
      candidates.forEach((c, i) => {
        if (f !== c.cited && norm.includes(c.needle)) homes[i].add(f);
      });
    }
  }
  const violations = [];
  candidates.forEach((c, i) => {
    if (homes[i].size === 0) return;
    violations.push(
      `${c.file}:${c.line}: the block quotes \`${c.quoted}\`, which is not in ${c.cited} — the file the citation \`${c.construct}\` above it names. It lives in ${[...homes[i]].sort().join(', ')}. A reader checking the block against the cited file will not find it: quote the cited construct, or cite the file the block is really from.`,
    );
  });
  return violations;
}

export function scan({ root = process.cwd(), pathspecs = ['*.go'] } = {}) {
  const files = lsFiles(pathspecs, { cwd: root });
  const violations = [];
  const candidates = [];
  for (const f of files) {
    const found = checkFile({ root, file: f });
    violations.push(...found.violations);
    candidates.push(...found.candidates);
  }
  // The repo-wide sweep is skipped entirely when nothing is pending, which is
  // the green path — it costs a second pass over the corpus only when a block
  // has already failed to resolve where it claims to be.
  if (candidates.length > 0) violations.push(...attribute({ root, files, candidates }));
  return { files: files.length, violations };
}

function main() {
  const root = process.env.REPO_ROOT || process.cwd();
  const pathspecs = (process.env.PATHSPECS || '*.go').split(/\s+/).filter(Boolean);
  const { files, violations } = scan({ root, pathspecs });

  if (files === 0) {
    // A gate that passes because it found nothing to check reports the same
    // green as a satisfied one.
    error(`verify-code-citations: pathspec ${JSON.stringify(pathspecs)} matched no Go file — the scan is broken, not the tree`);
    return 1;
  }
  if (violations.length > 0) {
    for (const v of violations) log(v);
    error(
      `${violations.length} place(s) where a note names code that cannot be verified. A note names a construct, not a line: ` +
        `\`file.go:${DELIM}construct${DELIM}\`, or \`file.go:func:${DELIM}construct${DELIM}\` when the construct repeats — ` +
        `in the backticked citation (#2953), in the prose around it (#2964), and in the block quoted under it (#2969).`,
    );
    return 1;
  }
  log(`verify-code-citations: ${files} Go file(s) scanned, every construct citation, prose address and quoted block resolves.`);
  notice(`verify-code-citations: every construct citation, prose address and quoted block across ${files} Go file(s) resolves`);
  return 0;
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exit(main());
}
