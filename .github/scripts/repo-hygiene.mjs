// repo-hygiene.mjs — the committed-artefact discipline gate.
//
// Two scans that together close one class: a build/scratch artefact gets
// `git add`-ed by accident and nobody notices, because nothing in CI ever
// looks at what the tree CONTAINS — only at whether it compiles and passes.
// A 2.3 MB compiled ELF binary and a generated release-PR-body scratch file
// both survived at the repository root that way.
//
//   CHECK=binary          No tracked blob may be a compiled binary /
//                         executable. Detection is by CONTENT, never by
//                         extension — the motivating artefact had no
//                         extension at all. A blob is rejected when its
//                         leading bytes carry a known executable magic
//                         (ELF / Mach-O / PE / WebAssembly / ar archive)
//                         or when the sniff window contains a NUL byte
//                         (the same heuristic git itself uses to classify
//                         a blob as binary).
//
//   CHECK=root-allowlist  The repository root is a curated surface. Every
//                         top-level tracked entry must appear in
//                         ROOT_ALLOWLIST below, and every ROOT_ALLOWLIST
//                         entry must still be tracked. The list is
//                         deliberately exhaustive rather than pattern-based
//                         (no blanket "dotfiles are fine" rule), so a stray
//                         `.perf-profile` is caught exactly like a stray
//                         `perf-profile`, and a root file that is deleted
//                         cannot leave a rotting entry behind.
//
// Both scans read the tracked set from `git ls-files -s`, so .gitignored
// paths are out of scope by construction: the companion fix for an artefact
// this gate catches is usually a `git rm` PLUS a .gitignore rule, and the
// ignore rule alone is what keeps it from coming back.
//
// The gate fails CLOSED. A tracked blob whose content cannot be read is an
// error, not a pass: we retry it out of the object store via `git cat-file`
// and, failing that, exit non-zero rather than assume it is text.
//
// Env contract:
//   CHECK      one of: binary | root-allowlist   (required)
//   REPO_ROOT  optional; directory to scan (default: the process cwd, which
//              on a runner is the checkout root). The self-test points this
//              at a synthetic fixture repo to prove the gate FIRES.
//
// Exit codes: 0 = clean, 1 = a violation was found (or bad $CHECK).

import { closeSync, openSync, readSync } from 'node:fs';
import process from 'node:process';

import { capture, error, git, log, notice } from './lib/gh.mjs';

// git classifies a blob as binary when a NUL byte appears in the leading
// block it sniffs (FIRST_FEW_BYTES in git's buffer_is_binary). Matching that
// window keeps this gate's verdict aligned with what `git diff` already
// reports as "Binary files differ", so the two never disagree.
export const gitBinarySniffBytes = 8000;

// Executable container magics, longest-first matching is unnecessary because
// no signature here is a prefix of another. Values are byte sequences at
// offset 0. `0xCAFEBABE` is shared by the Mach-O universal (fat) header and
// the Java class format — both are compiled output, so one label covers it.
const executableMagics = [
  { name: 'ELF', bytes: [0x7f, 0x45, 0x4c, 0x46] },
  // Named by the Mach-O constant each on-disk byte order corresponds to;
  // "32/64-bit" alone would not say which of the two layouts is meant, and
  // MH_CIGAM_64 is the layout an ordinary arm64/x86_64 macOS binary has.
  { name: 'Mach-O (MH_MAGIC)', bytes: [0xfe, 0xed, 0xfa, 0xce] },
  { name: 'Mach-O (MH_MAGIC_64)', bytes: [0xfe, 0xed, 0xfa, 0xcf] },
  { name: 'Mach-O (MH_CIGAM)', bytes: [0xce, 0xfa, 0xed, 0xfe] },
  { name: 'Mach-O (MH_CIGAM_64)', bytes: [0xcf, 0xfa, 0xed, 0xfe] },
  { name: 'Mach-O universal / Java class', bytes: [0xca, 0xfe, 0xba, 0xbe] },
  { name: 'PE / DOS executable', bytes: [0x4d, 0x5a] },
  { name: 'WebAssembly module', bytes: [0x00, 0x61, 0x73, 0x6d] },
  { name: 'ar archive / static library', bytes: [0x21, 0x3c, 0x61, 0x72, 0x63, 0x68, 0x3e] },
];

// Git index modes. Only regular blobs carry content this gate can sniff:
// a gitlink (160000) is a submodule pin with no blob in THIS repository,
// and a symlink (120000) stores its target path, not file content.
const modeRegularFile = '100644';
const modeExecutableFile = '100755';

// The complete set of tracked top-level entries. Adding a file or directory
// to the repository root is a deliberate act — it widens the surface every
// contributor sees first — so it goes through this list and a reviewer.
export const ROOT_ALLOWLIST = [
  // Dotfiles and dotdirs: tool configuration, enumerated rather than
  // wildcarded so a scratch artefact cannot hide behind a leading dot.
  '.claude',
  '.commitlintrc.json',
  '.dockerignore',
  '.editorconfig',
  '.envrc',
  '.gitattributes',
  '.github',
  '.gitignore',
  '.gitmodules',
  '.go-arch-lint.yml',
  '.golangci.yml',
  '.goreleaser.yml',
  '.gremlins.yaml',
  '.markdownlint.yaml',
  '.markdownlintignore',
  '.serena',
  // Project prose and policy.
  'AGENTS.md',
  'CHANGELOG.md',
  'CLAUDE.md',
  'CODE_OF_CONDUCT.md',
  'CONTRIBUTING.md',
  'LICENSE',
  'NOTICE',
  'README.md',
  'SECURITY.md',
  // Build, packaging, and task-runner entrypoints.
  'Dockerfile',
  'Dockerfile.local',
  'Justfile',
  'docker-compose.yml',
  'go.mod',
  'go.sum',
  'lefthook.yml',
  'versions.yaml',
  // Source trees.
  'bench',
  'cmd',
  'compatibility',
  'deploy',
  'docs',
  'internal',
  'scripts',
  'test',
];

// classifyBlob — decide whether a sniffed leading block is binary content.
// Returns { binary, format }: `format` names the magic when one matched and
// is null for the generic NUL-byte verdict. Exported for the self-test.
export function classifyBlob(head) {
  for (const magic of executableMagics) {
    if (head.length < magic.bytes.length) continue;
    let hit = true;
    for (let i = 0; i < magic.bytes.length; i++) {
      if (head[i] !== magic.bytes[i]) {
        hit = false;
        break;
      }
    }
    if (hit) return { binary: true, format: magic.name };
  }
  const window = head.subarray(0, gitBinarySniffBytes);
  if (window.includes(0x00)) return { binary: true, format: null };
  return { binary: false, format: null };
}

// rootEntriesOf — collapse tracked paths to their top-level component.
// Exported for the self-test.
export function rootEntriesOf(paths) {
  return new Set(paths.map((p) => p.split('/')[0]).filter((p) => p.length > 0));
}

// diffAllowlist — compare the live root against the curated list, in BOTH
// directions. `unexpected` is a stray entry nobody sanctioned; `stale` is an
// allow-list entry the tree no longer has, which would otherwise rot into a
// silent pre-approval for a future file of the same name. Exported for the
// self-test.
export function diffAllowlist(entries, allowlist) {
  const allowed = new Set(allowlist);
  const unexpected = [...entries].filter((e) => !allowed.has(e)).sort();
  const stale = [...allowed].filter((e) => !entries.has(e)).sort();
  return { unexpected, stale };
}

// trackedBlobs — `git ls-files -s` -> [{ mode, sha, path }] for the entries
// that carry sniffable content. Submodule gitlinks and symlinks are dropped
// here (they have no blob in this repository / store a path, not content).
function trackedBlobs() {
  const res = git(['ls-files', '-s', '-z']);
  if (res.status !== 0) {
    error(`git ls-files failed: ${res.stderr.trim()}`);
    process.exit(res.status);
  }
  const out = [];
  for (const record of res.stdout.split('\0')) {
    if (record.length === 0) continue;
    // Format: `<mode> <sha> <stage>\t<path>`
    const tab = record.indexOf('\t');
    if (tab < 0) {
      error(`git ls-files produced an unparseable record: ${JSON.stringify(record)}`);
      process.exit(1);
    }
    const [mode, sha] = record.slice(0, tab).split(/\s+/);
    out.push({ mode, sha, path: record.slice(tab + 1) });
  }
  return out;
}

// readHead — the leading sniff block of a tracked blob. Prefers the working
// tree (one bounded read, no subprocess) and falls back to the object store
// when the file is absent or unreadable there. Exits non-zero if neither
// route yields content: an unreadable tracked blob is a gate failure, never
// an implicit pass.
function readHead({ sha, path }) {
  const buf = Buffer.alloc(gitBinarySniffBytes);
  try {
    const fd = openSync(path, 'r');
    try {
      const n = readSync(fd, buf, 0, gitBinarySniffBytes, 0);
      return buf.subarray(0, n);
    } finally {
      closeSync(fd);
    }
  } catch {
    // Fall through to the object store: a tracked path can legitimately be
    // missing from the working tree (sparse checkout, mid-rebase).
  }
  const res = capture('git', ['cat-file', 'blob', sha], { encoding: 'buffer' });
  if (res.status !== 0) {
    error(
      `repo-hygiene: cannot read tracked blob ${path} (${sha}) from the working tree or the object store — ` +
        `refusing to classify it as text`,
    );
    process.exit(1);
  }
  return res.stdout.subarray(0, gitBinarySniffBytes);
}

function scanBinary() {
  const violations = [];
  for (const entry of trackedBlobs()) {
    if (entry.mode !== modeRegularFile && entry.mode !== modeExecutableFile) continue;
    const { binary, format } = classifyBlob(readHead(entry));
    if (binary) violations.push({ path: entry.path, format });
  }
  if (violations.length > 0) {
    for (const v of violations) {
      log(`${v.path}: ${v.format ? `${v.format} executable` : 'binary content (NUL byte in the leading block)'}`);
    }
    error(
      `${violations.length} compiled binary / binary artefact is tracked in git. Build output is never committed: ` +
        `\`git rm --cached\` the file, add the path to .gitignore so it cannot come back, and build it from source ` +
        `via the Justfile recipe that produces it.`,
    );
    process.exit(1);
  }
  notice('repo-hygiene: no tracked binary artefacts');
}

function scanRootAllowlist() {
  const res = git(['ls-files', '-z']);
  if (res.status !== 0) {
    error(`git ls-files failed: ${res.stderr.trim()}`);
    process.exit(res.status);
  }
  const entries = rootEntriesOf(res.stdout.split('\0').filter((p) => p.length > 0));
  const { unexpected, stale } = diffAllowlist(entries, ROOT_ALLOWLIST);
  for (const e of unexpected) log(`${e}: untracked-by-policy entry at the repository root`);
  for (const e of stale) log(`${e}: allow-listed root entry that is no longer tracked`);
  if (unexpected.length > 0 || stale.length > 0) {
    const parts = [];
    if (unexpected.length > 0) {
      parts.push(
        `${unexpected.length} entry/entries at the repository root are not on the allow-list (${unexpected.join(', ')}). ` +
          `The root is a curated surface: if this is build/scratch output, delete it and add a .gitignore rule; ` +
          `if it genuinely belongs at the root, add it to ROOT_ALLOWLIST in .github/scripts/repo-hygiene.mjs.`,
      );
    }
    if (stale.length > 0) {
      parts.push(
        `${stale.length} ROOT_ALLOWLIST entry/entries no longer exist (${stale.join(', ')}); ` +
          `drop them from the list so it cannot pre-approve a future file of the same name.`,
      );
    }
    error(parts.join(' '));
    process.exit(1);
  }
  notice(`repo-hygiene: repository root matches the allow-list (${ROOT_ALLOWLIST.length} entries)`);
}

function isMain() {
  const invoked = process.argv[1] || '';
  return invoked.endsWith('repo-hygiene.mjs');
}

if (isMain()) {
  const root = process.env.REPO_ROOT;
  if (root) process.chdir(root);
  const CHECK = process.env.CHECK || '';
  switch (CHECK) {
    case 'binary':
      scanBinary();
      break;
    case 'root-allowlist':
      scanRootAllowlist();
      break;
    default:
      error(`repo-hygiene.mjs: unknown CHECK="${CHECK}" (expected one of: binary, root-allowlist)`);
      process.exit(1);
  }
  process.exit(0);
}
