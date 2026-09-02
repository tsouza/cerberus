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

// normalise — collapse every whitespace run to one space so a citation may be
// written with the spacing that reads best, independent of the source's
// indentation and alignment.
export function normalise(text) {
  return text.replace(/\s+/g, ' ').trim();
}

// unescape — undo the two Go interpreted-string escapes a citation can acquire
// purely by being written inside one. A `t.Fatalf` message citing a construct
// that contains a double quote has to spell it `\\"` for the Go compiler; the
// gate reads the file as text and would otherwise compare the escape against a
// source line that never had one. Nothing else is unescaped: this undoes the
// literal's encoding, it does not interpret the construct.
export function unescape(text) {
  return text.replace(/\\(["\\])/g, '$1');
}

// isCodeLine — a line that a mutation operator could apply to. Blank lines and
// whole-line comments are not; a line with a trailing comment is.
export function isCodeLine(line) {
  const t = line.trim();
  return t !== '' && !t.startsWith('//');
}

// funcRanges — the top-level function scopes of a Go file, as
// name -> { start, end } 1-based inclusive line bounds. A top-level `func`
// declaration starts at column 0; its scope runs to the line before the next
// one. gofumpt is enforced repo-wide, so column-0 `func` is reliable.
export function funcRanges(lines) {
  const decls = [];
  lines.forEach((line, i) => {
    const m = /^func\s+(?:\([^)]*\)\s*)?([A-Za-z0-9_]+)/.exec(line);
    if (m) decls.push({ name: m[1], start: i + 1 });
  });
  const out = new Map();
  decls.forEach((d, i) => {
    const end = i + 1 < decls.length ? decls[i + 1].start - 1 : lines.length;
    // A name may be declared more than once across receivers; keep every range
    // so a citation naming it searches all of them and still demands one hit.
    if (!out.has(d.name)) out.set(d.name, []);
    out.get(d.name).push({ start: d.start, end });
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
    if (line.trim().startsWith('//')) {
      const body = line.trim().replace(/^\/\/\s?/, '');
      if (!block) block = { text: '', lines: [] };
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

// checkFile — every violation in one tracked file.
export function checkFile({ root, file }) {
  const text = readFileSync(resolve(root, file), 'utf8');
  const violations = [];
  const cache = new Map();
  const load = (rel) => {
    if (!cache.has(rel)) cache.set(rel, readFileSync(resolve(root, rel), 'utf8').split('\n'));
    return cache.get(rel);
  };

  for (const unit of units(text.split('\n'))) {
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
      const lines = load(rel);
      let ranges = null;
      if (c.scope !== null) {
        const found = funcRanges(lines).get(c.scope);
        if (!found) {
          violations.push(`${where}: citation scopes to \`${c.scope}\`, which is not a top-level func in ${rel}.`);
          continue;
        }
        ranges = found;
      }
      const hits = matchLines({ lines, construct: c.construct, ranges });
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
  }
  return violations;
}

export function scan({ root = process.cwd(), pathspecs = ['*.go'] } = {}) {
  const files = lsFiles(pathspecs, { cwd: root });
  const violations = [];
  for (const f of files) violations.push(...checkFile({ root, file: f }));
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
      `${violations.length} source citation(s) do not resolve. A citation names a construct, not a line: ` +
        `\`file.go:${DELIM}construct${DELIM}\`, or \`file.go:func:${DELIM}construct${DELIM}\` when the construct repeats. See #2953.`,
    );
    return 1;
  }
  log(`verify-code-citations: ${files} Go file(s) scanned, every construct citation resolves.`);
  notice(`verify-code-citations: every construct citation across ${files} Go file(s) resolves`);
  return 0;
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exit(main());
}
