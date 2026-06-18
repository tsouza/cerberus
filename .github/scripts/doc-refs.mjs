// doc-refs.mjs — the doc-to-code reference-integrity gate.
//
// Scans the prose docs (docs/**/*.md) for inline references to cerberus
// source files and HARD-FAILS when a referenced path no longer exists. A
// doc that cites `internal/promql/lower.go:1924` or
// `compatibility/prometheus/cmd/seed/prom_remote.go` is making a promise the
// tree must keep; this gate catches the promise rotting after a rename /
// delete (the dead `cmd/seed/prom_remote.go` cite in docs/compatibility.md
// is the canonical motivating case).
//
// What it matches. A reference is a path-like token, bounded by a
// non-path character on both sides, whose segments include one of the
// cerberus top-level source dirs as a path segment:
//
//   (internal|cmd|test|deploy)
//
// allowing an OPTIONAL leading prefix (so a nested module path such as
// `compatibility/prometheus/cmd/seed/prom_remote.go` is captured WHOLE, not
// truncated to its `cmd/...` tail — truncating would make a valid path look
// dead). Two shapes:
//
//   - FILE ref: the token ends in `.go`, optionally pinned with `:line`
//     or `:start-end`. The file must exist (`git ls-files`). For a pin we
//     do a BOUNDS check only: fail iff the (high) line number exceeds the
//     file's line count. Docs pin approximate / tilde line numbers and
//     ranges that drift by a line or two as code moves, so we deliberately
//     do NOT require the cited line to contain anything in particular —
//     only that it is in range. An out-of-range pin is a real staleness
//     signal (the file shrank past the cite); a slightly-off in-range pin
//     is tolerated by design.
//
//   - DIR ref: the token ends in `/` and has no `.go`. The directory must
//     exist as a tracked path (any `git ls-files` entry under it).
//
// Non-goals. This is NOT a markdown link checker (lychee owns that) and
// NOT a symbol/line-content checker. It only answers "does the cited path
// still exist, and is the line pin in range".
//
// Vendored upstream snapshots (`compatibility/*/upstream/**`) are NOT
// scanned for references and NEVER count as the existence target — they
// mirror the markdownlint / forbid-skip exclude set.
//
// Structure mirrors forbid-skip.mjs: pure exported helpers (so
// doc-refs.test.mjs can drive them with `node --test`), an env/argv entry
// point, and a `--self-test` flag that runs the same in-process assertions
// the test file does (parity with forbid-skip's self-test idiom) so the
// detectors can't silently rot into a no-op.
//
// Env / argv contract:
//   argv `--self-test`   run the in-process assertion suite, exit 0/1.
//   DOCS_GLOBS           pathspec list (default `docs/**/*.md`).
//   (no other input)     scan the live tree, exit 0 clean / 1 on any
//                        missing path or out-of-range pin.
//
// Exit codes: 0 = every referenced path exists and pins are in range,
// 1 = at least one dead reference (or a failed --self-test).

import { readFileSync, existsSync, statSync } from 'node:fs';
import { posix as posixPath } from 'node:path';
import process from 'node:process';
import { lsFiles, error, log, notice } from './lib/gh.mjs';

// Top-level cerberus source dirs a reference may anchor on. A token only
// counts as a code reference if one of these appears as a path SEGMENT.
const ANCHOR = '(?:internal|cmd|test|deploy)';

// FILE ref: optional `prefix/` segments, then the anchor segment, then the
// rest of the path ending in `.go`, then an OPTIONAL `:line` / `:start-end`
// pin. Bounded by a non-path char (or string edge) on the left via a
// look-behind and by a non-word char on the right.
const FILE_RE = new RegExp(
  String.raw`(?<![\w./-])((?:[\w.-]+\/)*` + ANCHOR + String.raw`\/[\w./-]*?\.go)(?::(\d+)(?:-(\d+))?)?(?![\w])`,
  'g',
);

// DIR ref: optional `prefix/` segments, the anchor segment, then one or
// more further segments, ending in `/` (and NOT a `.go` path — the `(?![\w])`
// right-bound plus the trailing slash rule out file paths, whose next char
// after a slash is a word char).
const DIR_RE = new RegExp(
  String.raw`(?<![\w./-])((?:[\w.-]+\/)*` + ANCHOR + String.raw`\/(?:[\w.-]+\/)+)(?![\w])`,
  'g',
);

// extractRefs(text) — parse one doc's text into { files, dirs }.
//   files: [{ path, line }]  line = high line number of the pin, or null.
//   dirs:  [path, ...]       each ends in `/`.
// Duplicates are preserved so the caller can report every citing site; the
// reporter dedupes for the summary.
export function extractRefs(text) {
  const files = [];
  const dirs = [];
  let m;
  FILE_RE.lastIndex = 0;
  while ((m = FILE_RE.exec(text)) !== null) {
    const high = m[3] !== undefined ? Number(m[3]) : m[2] !== undefined ? Number(m[2]) : null;
    files.push({ path: m[1], line: high });
  }
  DIR_RE.lastIndex = 0;
  while ((m = DIR_RE.exec(text)) !== null) {
    dirs.push(m[1]);
  }
  return { files, dirs };
}

// candidatePaths(token, docPath) — the set of repo-root-relative paths a
// matched token could legitimately denote. A token is "alive" iff ANY
// candidate exists; only a token dead under EVERY interpretation is a
// violation. Two interpretations are inherently ambiguous in prose:
//
//   - REPO-ROOT: bare tokens (`test/spec/`, `internal/promql/lower.go`) and
//     shell-snippet `./`-prefixed tokens (`go test ./test/rejection-parity/`,
//     where `./` is the cwd = repo root) are root-relative.
//   - DOC-RELATIVE: a markdown link target written `../x` (and sometimes
//     `./x`) is relative to the CITING doc's directory
//     (docs/coverage.md -> `../test/...` = repo-root `test/...`).
//
// Rather than guess which one a given `./`/`../` token is, we try BOTH and
// accept existence under either. A genuinely stale citation (the dead
// `cmd/seed/prom_remote.go`) resolves to a non-existent path under every
// interpretation, so the gate still fires. Trailing slash is preserved for
// dir tokens so the classifier keeps treating them as directories.
export function candidatePaths(token, docPath) {
  const isDir = token.endsWith('/');
  const finish = (p) => {
    let r = posixPath.normalize(p);
    if (isDir && !r.endsWith('/')) r += '/';
    return r;
  };
  const out = new Set();
  // Repo-root interpretation: strip a leading `./`, keep `../` literal (a
  // `../` from repo root escapes the tree and simply won't exist — harmless).
  out.add(finish(token.replace(/^\.\//, '')));
  // Doc-relative interpretation for `./` and `../` tokens.
  if (token.startsWith('./') || token.startsWith('../')) {
    out.add(finish(posixPath.join(posixPath.dirname(docPath), token)));
  }
  return [...out].filter((p) => p && !p.startsWith('../'));
}

// isUpstream(path) — vendored snapshot under compatibility/<head>/upstream/.
// Such paths are excluded as BOTH a scan source and an existence target.
export function isUpstream(path) {
  return /^compatibility\/[^/]+\/upstream\//.test(path);
}

// lineCount(path) — number of lines in a file. A trailing newline does not
// add a phantom line (matches `wc -l + 1`-free intuition for pins: the last
// content line is the max valid pin).
function lineCount(path) {
  const text = readFileSync(path, 'utf8');
  if (text.length === 0) return 0;
  const n = text.split('\n').length;
  // A trailing newline yields a final empty element; the last real line is n-1.
  return text.endsWith('\n') ? n - 1 : n;
}

// checkFileRef(ref, { exists, count }) — pure verdict for one file ref.
// Injectable fs probes keep it unit-testable. Returns null on OK, or a
// violation string. `exists(path)` -> bool, `count(path)` -> number.
export function checkFileRef(ref, probes) {
  if (isUpstream(ref.path)) return null; // never target a vendored snapshot
  if (!probes.exists(ref.path)) {
    return `missing file: ${ref.path}`;
  }
  if (ref.line !== null) {
    const n = probes.count(ref.path);
    if (ref.line > n) {
      return `line pin out of range: ${ref.path}:${ref.line} (file has ${n} lines)`;
    }
  }
  return null;
}

// checkDirRef(path, { dirExists }) — pure verdict for one dir ref.
export function checkDirRef(path, probes) {
  if (isUpstream(path)) return null;
  if (!probes.dirExists(path)) {
    return `missing directory: ${path}`;
  }
  return null;
}

// Live-tree probes backed by git ls-files (tracked-only — an untracked
// scratch file does not satisfy a doc reference) and the filesystem.
function liveProbes() {
  const tracked = new Set(lsFiles(['*.go']).map((p) => p));
  // For dir existence we need ALL tracked paths, not just *.go.
  const allTracked = lsFiles([':/']);
  const trackedPrefixes = allTracked;
  return {
    exists: (p) => tracked.has(p) || (existsSync(p) && allTracked.includes(p)),
    count: (p) => lineCount(p),
    dirExists: (dir) => {
      const norm = dir.endsWith('/') ? dir : `${dir}/`;
      return trackedPrefixes.some((p) => p.startsWith(norm)) || (existsSync(dir) && safeIsDir(dir));
    },
  };
}

function safeIsDir(p) {
  try {
    return statSync(p).isDirectory();
  } catch {
    return false;
  }
}

// scan() — the live-tree gate. Returns { violations, refCount }.
export function scan({ globs } = {}) {
  // `:(glob)` magic makes `**` cross directory boundaries AND match files
  // directly under docs/ (a bare `docs/**/*.md` git pathspec requires an
  // intermediate dir and so silently matches NONE of the top-level docs).
  const pathspecs = (globs && globs.length ? globs : [':(glob)docs/**/*.md']).concat([
    ':!:compatibility/*/upstream/**',
  ]);
  const docFiles = lsFiles(pathspecs);
  const probes = liveProbes();
  const violations = [];
  let refCount = 0;
  for (const doc of docFiles) {
    const text = readFileSync(doc, 'utf8');
    const { files, dirs } = extractRefs(text);
    for (const ref of files) {
      refCount += 1;
      const candidates = candidatePaths(ref.path, doc);
      // Alive iff SOME candidate passes; report the first (repo-root) verdict
      // when all fail, so the message names the canonical interpretation.
      const verdicts = candidates.map((p) => checkFileRef({ path: p, line: ref.line }, probes));
      if (candidates.length === 0 || verdicts.every((v) => v !== null)) {
        violations.push(`${doc}: ${verdicts[0] ?? `missing file: ${ref.path}`}`);
      }
    }
    for (const dir of dirs) {
      refCount += 1;
      const candidates = candidatePaths(dir, doc);
      const verdicts = candidates.map((p) => checkDirRef(p, probes));
      if (candidates.length === 0 || verdicts.every((v) => v !== null)) {
        violations.push(`${doc}: ${verdicts[0] ?? `missing directory: ${dir}`}`);
      }
    }
  }
  return { violations, refCount };
}

// ---------------------------------------------------------------------------
// --self-test: in-process assertions that pin the regex + verdict behaviour,
// mirroring forbid-skip's self-test idiom. doc-refs.test.mjs drives the same
// exported helpers via `node --test`; this flag keeps a dep-free smoke run.
// ---------------------------------------------------------------------------
function selfTest() {
  let failures = 0;
  const ok = (cond, msg) => {
    if (cond) {
      log(`  ok   ${msg}`);
    } else {
      failures += 1;
      log(`  FAIL ${msg}`);
    }
  };

  // 1. Nested-module path is captured WHOLE (not truncated to its cmd/ tail).
  {
    const { files } = extractRefs('see `compatibility/prometheus/cmd/seed/prom_remote.go` here');
    ok(
      files.length === 1 && files[0].path === 'compatibility/prometheus/cmd/seed/prom_remote.go',
      'nested cmd/ path captured whole',
    );
  }

  // 2. Bare anchor path + line range parse; high bound is taken.
  {
    const { files } = extractRefs('`internal/promql/lower.go:1924-1926`');
    ok(
      files.length === 1 && files[0].path === 'internal/promql/lower.go' && files[0].line === 1926,
      'line range high-bound parsed',
    );
  }

  // 3. Single-line pin parses.
  {
    const { files } = extractRefs('`internal/config/config.go:425`');
    ok(files[0] && files[0].line === 425, 'single line pin parsed');
  }

  // 4. A directory ref is classified as a dir, not a file.
  {
    const { files, dirs } = extractRefs('staged under `internal/foo/bar/`');
    ok(files.length === 0 && dirs.length === 1 && dirs[0] === 'internal/foo/bar/', 'dir ref classified');
  }

  // 5. A non-anchored path (e.g. docs/foo.go) is NOT a code reference.
  {
    const { files } = extractRefs('`docs/clickhouse-contrib/example.md` and `pkg/util/x.go`');
    ok(files.length === 0, 'non-anchored paths ignored');
  }

  // 6. checkFileRef HARD-FAILS on a missing file (the motivating case).
  {
    const probes = { exists: () => false, count: () => 0 };
    const v = checkFileRef({ path: 'cmd/seed/prom_remote.go', line: null }, probes);
    ok(v !== null && v.includes('missing file'), 'missing file fails');
  }

  // 7. checkFileRef tolerates an in-range pin, fails an out-of-range pin.
  {
    const probes = { exists: () => true, count: () => 500 };
    ok(checkFileRef({ path: 'internal/x.go', line: 425 }, probes) === null, 'in-range pin passes');
    const v = checkFileRef({ path: 'internal/x.go', line: 9999 }, probes);
    ok(v !== null && v.includes('out of range'), 'out-of-range pin fails');
  }

  // 8. A vendored upstream snapshot is never targeted.
  {
    const probes = { exists: () => false, count: () => 0 };
    ok(
      checkFileRef({ path: 'compatibility/prometheus/upstream/x.go', line: null }, probes) === null,
      'upstream snapshot excluded',
    );
  }

  // 9. checkDirRef fails a missing directory.
  {
    const v = checkDirRef('internal/gone/', { dirExists: () => false });
    ok(v !== null && v.includes('missing directory'), 'missing dir fails');
  }

  if (failures > 0) {
    error(`doc-refs.mjs --self-test: ${failures} assertion(s) failed`);
    process.exit(1);
  }
  notice('doc-refs.mjs --self-test: all assertions passed');
  process.exit(0);
}

// ---------------------------------------------------------------------------
// Entry point.
// ---------------------------------------------------------------------------
function isMain() {
  // True when executed directly (`node doc-refs.mjs`), false when imported.
  const invoked = process.argv[1] || '';
  return invoked.endsWith('doc-refs.mjs');
}

if (isMain()) {
  if (process.argv.includes('--self-test')) {
    selfTest();
  } else {
    const globsEnv = (process.env.DOCS_GLOBS || '').trim();
    const globs = globsEnv ? globsEnv.split(/\s+/) : undefined;
    const { violations, refCount } = scan({ globs });
    if (violations.length > 0) {
      for (const v of violations) log(v);
      error(
        `doc-to-code reference check: ${violations.length} dead reference(s) found across ${refCount} cited paths. ` +
          `Update the doc to the current path/line, or remove the stale citation.`,
      );
      process.exit(1);
    }
    notice(`doc-to-code reference check: all ${refCount} cited code paths exist and pins are in range`);
    process.exit(0);
  }
}
