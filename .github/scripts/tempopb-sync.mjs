// tempopb-sync.mjs — copy Tempo's Apache-2.0 wire types into
// internal/thirdparty/tempopb from the grafana/tempo commit the repository
// tracks.
//
// Source: `pkg/tempopb` of github.com/grafana/tempo at the `upstream_commit`
// recorded in compatibility/tempo/UPSTREAM_VERSION, fetched through the Go
// module proxy (checksum-verified by the Go checksum database). That subtree
// carries its own LICENSE_APACHE2; the rest of the Tempo module is AGPL-3.0
// and is never read.
//
// Every file is copied byte for byte, with exactly three rewrites, and the Go
// files are then run through the repository's formatters (`gofumpt -extra`,
// `goimports -local`, as lefthook and the lint gate apply them), so the copy is
// stable under the pre-commit hooks:
//   1. The import path github.com/grafana/tempo/pkg/tempopb becomes this
//      repository's internal/thirdparty/tempopb.
//   2. The names the generated code registers in gogo/protobuf's global
//      registry (RegisterType / RegisterEnum / RegisterMapType, and the
//      `enum=` struct tags that look those enums up) move from the `tempopb.`
//      namespace to `cerberus.tempopb.`. gogo panics on a duplicate enum
//      registration, and a test binary built with the agpl_oracle tag links
//      Tempo's own copy next to this one. Wire encoding does not carry these
//      names, and the gRPC service names (`ServiceName`, `/tempopb.Svc/Method`)
//      are left untouched, so the protocol is unchanged.
//   3. Each Go file gains a notice, under its `Code generated` line, that it
//      was modified and by what (Apache-2.0 section 4(b)).
//
// Only the files isCopied names are taken; the cerberus-authored files in
// PRESERVED are left as they are. SOURCE is rewritten with the module
// version the files came from.
//
// Modes: with no argument the package is rewritten in place (`just
// tempopb-sync`). With `--check` the sync runs into a scratch copy and the
// result is compared with the committed package, file by file and byte by byte
// (`just tempopb-sync-check`, run by ci.yml's `agpl-clean` job), so a hand edit
// to the copy, a stale copy, or a cerberus-authored file left out of PRESERVED
// fails.
//
// Env: none. Run from the repository root; needs `go`, `gofumpt` and
// `goimports` on PATH.
// Exit: 0 when the package was rewritten, or `--check` found it identical;
// 1 on drift, or on any fetch or I/O failure.

import { copyFileSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { capture, error, notice } from './lib/gh.mjs';

const MODULE = 'github.com/grafana/tempo';
const SOURCE_SUBDIR = 'pkg/tempopb';
const UPSTREAM_IMPORT = `${MODULE}/${SOURCE_SUBDIR}`;
const LOCAL_IMPORT = 'github.com/tsouza/cerberus/internal/thirdparty/tempopb';
const PIN_FILE = 'compatibility/tempo/UPSTREAM_VERSION';
const TARGET_DIR = 'internal/thirdparty/tempopb';
// Cerberus-authored files in the target directory, kept across a sync.
const PRESERVED = new Set(['README.md', 'preallocbytes.go', 'preallocbytes_test.go', 'wire_roundtrip_test.go', 'wire_tempo_agpl_test.go']);
// The upstream files copied: the generated wire types, the .proto sources
// they are generated from, and the subtree's licence. The hand-written helpers
// beside them are not copied. pool.go and prealloc.go back PreallocBytes with
// Tempo's ingester byte-slice pool, which registers a Prometheus counter at
// init (a second copy of the package in one binary panics on it) and reads
// PREALLOC_* tuning variables; preallocbytes.go supplies the same type without
// it. utils.go and trace_utils.go hold helpers cerberus does not call.
export function isCopied(rel) {
  return rel.endsWith('.pb.go') || rel.endsWith('.proto') || rel === 'LICENSE_APACHE2';
}
const REGISTRY_NAMESPACE = 'cerberus.tempopb.';

// upstreamCommit reads the `upstream_commit:` field of the pin file.
export function upstreamCommit(pinText) {
  const match = /^upstream_commit:\s*([0-9a-f]{40})\s*$/m.exec(pinText);
  if (!match) throw new Error(`${PIN_FILE} has no 40-hex upstream_commit field`);
  return match[1];
}

// rewriteGo applies the two documented rewrites to one Go source file.
export function rewriteGo(source) {
  return source
    .split(UPSTREAM_IMPORT)
    .join(LOCAL_IMPORT)
    .replace(/(proto\.Register(?:Type|Enum|MapType)\([^"\n]*")tempopb\./g, `$1${REGISTRY_NAMESPACE}`)
    .replace(/\benum=tempopb\./g, `enum=${REGISTRY_NAMESPACE}`);
}

// The Apache-2.0 section 4(b) notice each copied Go file carries.
export const MODIFIED_NOTICE = [
  '// Modified by the cerberus authors: copied from github.com/grafana/tempo/pkg/tempopb',
  '// with its import paths and gogo registry names rewritten by',
  '// .github/scripts/tempopb-sync.mjs. See README.md in this package.',
].join('\n');

// addModifiedNotice puts MODIFIED_NOTICE under the file's `Code generated`
// line, which has to stay first for tools to treat the file as generated.
export function addModifiedNotice(source) {
  const nl = source.indexOf('\n');
  if (!source.startsWith('// Code generated') || nl < 0) return `${MODIFIED_NOTICE}\n\n${source}`;
  return `${source.slice(0, nl + 1)}${MODIFIED_NOTICE}\n${source.slice(nl + 1)}`;
}

// diffTrees lists the relative paths that differ between two directories:
// present in only one of them, or present in both with different bytes.
export function diffTrees(want, got) {
  const a = existsSync(want) ? listFiles(want) : [];
  const b = existsSync(got) ? listFiles(got) : [];
  const out = [];
  for (const rel of new Set([...a, ...b])) {
    if (!a.includes(rel)) out.push(`${rel}: produced by the sync but not committed`);
    else if (!b.includes(rel)) out.push(`${rel}: committed but not produced by the sync (add it to PRESERVED if cerberus wrote it)`);
    else if (!readFileSync(path.join(want, rel)).equals(readFileSync(path.join(got, rel)))) out.push(`${rel}: content differs`);
  }
  return out.sort();
}

function listFiles(dir, base = dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const full = path.join(dir, entry);
    if (statSync(full).isDirectory()) out.push(...listFiles(full, base));
    else out.push(path.relative(base, full));
  }
  return out.sort();
}

function download(commit) {
  // Outside any module, so the root go.mod's `replace` of grafana/tempo never
  // applies: this is upstream's own tree at the pinned commit.
  const cwd = mkdtempSync(path.join(tmpdir(), 'tempopb-sync-'));
  try {
    const res = capture('go', ['mod', 'download', '-json', `${MODULE}@${commit}`], { cwd });
    if (res.status !== 0) throw new Error(`go mod download ${MODULE}@${commit} failed: ${res.stderr}`);
    const info = JSON.parse(res.stdout);
    if (!info.Dir) throw new Error(`go mod download reported no Dir: ${res.stdout}`);
    return { dir: info.Dir, version: info.Version };
  } finally {
    rmSync(cwd, { recursive: true, force: true });
  }
}

function sync(targetDir) {
  const commit = upstreamCommit(readFileSync(PIN_FILE, 'utf8'));
  const { dir, version } = download(commit);
  const source = path.join(dir, SOURCE_SUBDIR);
  if (!existsSync(path.join(source, 'LICENSE_APACHE2'))) {
    throw new Error(`${UPSTREAM_IMPORT}@${version} carries no LICENSE_APACHE2; refusing to copy it`);
  }

  for (const entry of existsSync(targetDir) ? readdirSync(targetDir) : []) {
    if (!PRESERVED.has(entry)) rmSync(path.join(targetDir, entry), { recursive: true, force: true });
  }
  for (const rel of listFiles(source)) {
    if (!isCopied(rel)) continue;
    const from = path.join(source, rel);
    const to = path.join(targetDir, rel);
    mkdirSync(path.dirname(to), { recursive: true });
    const body = readFileSync(from, 'utf8');
    writeFileSync(to, rel.endsWith('.go') ? addModifiedNotice(rewriteGo(body)) : body, { mode: 0o644 });
  }
  writeFileSync(path.join(targetDir, 'SOURCE'), `${UPSTREAM_IMPORT}@${version}\n`, { mode: 0o644 });
  // Files named explicitly, as lefthook names staged files: gofumpt skips a
  // generated file found by walking a directory but formats one it is given.
  const goFiles = listFiles(targetDir)
    .filter((rel) => rel.endsWith('.go'))
    .map((rel) => path.join(targetDir, rel));
  for (const [tool, args] of [
    ['gofumpt', ['-extra', '-w', ...goFiles]],
    ['goimports', ['-w', '-local', 'github.com/tsouza/cerberus', ...goFiles]],
  ]) {
    const res = capture(tool, args);
    if (res.status !== 0) throw new Error(`${tool} failed (install it with \`just install-tools\`): ${res.stderr}`);
  }
  return version;
}

function main(argv) {
  if (argv.length === 0) {
    const version = sync(TARGET_DIR);
    notice(`tempopb-sync: ${TARGET_DIR} copied from ${UPSTREAM_IMPORT}@${version}`);
    return;
  }
  if (argv.length !== 1 || argv[0] !== '--check') throw new Error(`unknown arguments ${JSON.stringify(argv)}; the only flag is --check`);
  // The scratch copy sits inside the module, at the same depth as the target,
  // so the formatters read the same go.mod and resolve the same local prefix.
  const scratch = mkdtempSync(path.join(path.dirname(TARGET_DIR), '.tempopb-check-'));
  try {
    for (const name of PRESERVED) {
      if (existsSync(path.join(TARGET_DIR, name))) copyFileSync(path.join(TARGET_DIR, name), path.join(scratch, name));
    }
    const version = sync(scratch);
    const drift = diffTrees(TARGET_DIR, scratch);
    if (drift.length > 0) {
      for (const line of drift) error(`tempopb-sync --check: ${TARGET_DIR}/${line}`, { title: 'tempopb drift' });
      throw new Error(
        `${TARGET_DIR} is not what \`just tempopb-sync\` produces from ${UPSTREAM_IMPORT}@${version} ` +
          `(${drift.length} file(s) above). Regenerate it with \`just tempopb-sync\`; never edit the copy by hand.`,
      );
    }
    notice(`tempopb-sync --check: ${TARGET_DIR} matches ${UPSTREAM_IMPORT}@${version}`);
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  try {
    main(process.argv.slice(2));
  } catch (err) {
    error(`tempopb-sync: ${err.message}`);
    process.exit(1);
  }
}
