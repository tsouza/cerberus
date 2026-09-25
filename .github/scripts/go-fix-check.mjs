// go-fix-check — the tree must be a fixed point of the pinned toolchain's
// `go fix`.
//
// `go fix` runs the Go release's modernizer suite (range-over-int, min/max,
// slices/maps helpers, strings.Cut / SplitSeq, new(expr), WaitGroup.Go, …).
// Without a gate, one change applies it and the next reintroduces the idioms it
// removed. Only a check that fails on a non-empty `go fix -diff` holds the tree
// at the fixed point.
//
// Every module in the repository is checked — the root one and each nested
// `go.mod` (`git ls-files`), since `./...` from the root stops at a nested
// module boundary. Each module gets two passes, mirroring golangci-lint's two
// build configurations (ci.yml's `lint` job): one with every build tag in
// `.golangci.yml`'s `run.build-tags` union, and one untagged. Every `//go:build` line in the tree is a single term,
// so between them they see every file. test/regression/lint_build_tags_test.go
// already holds the union to the tree's constraints. Reading the union from the
// same file keeps this gate on that pinned list instead of a second hand-kept
// copy.
//
// ENV CONTRACT
//   GO_FIX_MODE    — `check` (default): report any pending rewrite and fail.
//                    `apply`: rewrite the tree in place (the `just go-fix`
//                    recipe). Anything else is a usage error.
//   GOLANGCI_FILE  — the config whose `run.build-tags` is the tag union.
//                    Default `.golangci.yml`.
//
// `go fix -diff` exits 1 both when it has a rewrite to report (the diff on
// stdout, stderr empty) and when it cannot run (a load or build error on
// stderr), so the exit status alone does not tell the two apart: a run is a
// pending rewrite only when it exits 1 with a diff and nothing on stderr.
//
// Exit: 0 when both passes leave the tree unchanged (or, in apply mode, when
// both passes ran), 1 on a pending rewrite or a `go fix` failure, 2 on a usage
// error.

import { readFileSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { capture, error, group, log, notice } from './lib/gh.mjs';

const MODES = new Set(['check', 'apply']);

// parseBuildTags — the `build-tags:` list under `run:` in a golangci config.
// A deliberately narrow reader (no YAML dependency): the block is a flat list
// of bare identifiers, and anything this reader cannot parse is an error rather
// than a silently shorter tag set.
export function parseBuildTags(configText) {
  const lines = configText.split('\n');
  const start = lines.findIndex((l) => /^\s+build-tags:\s*$/.test(l));
  if (start < 0) throw new Error('no `build-tags:` block found');
  const tags = [];
  for (const line of lines.slice(start + 1)) {
    if (/^\s*(#.*)?$/.test(line)) continue;
    const m = line.match(/^\s+-\s+([A-Za-z0-9_.]+)\s*(#.*)?$/);
    if (!m) break;
    tags.push(m[1]);
  }
  if (tags.length === 0) throw new Error('`build-tags:` block is empty');
  return tags;
}

// diffFiles — the files a `go fix -diff` output would rewrite, in order.
export function diffFiles(diffText) {
  const files = [];
  for (const m of diffText.matchAll(/^--- (.+?) \(old\)$/gm)) files.push(m[1]);
  return files;
}

// passes — the two build configurations, as `go fix` argument lists.
export function passes(tags) {
  return [
    { name: 'tagged', args: [`-tags=${tags.join(',')}`] },
    { name: 'untagged', args: [] },
  ];
}

// moduleDirs — every directory holding a tracked go.mod, relative to root,
// root itself (`.`) first.
export function moduleDirs(lsFilesOutput) {
  const dirs = lsFilesOutput
    .split('\0')
    .filter((f) => f === 'go.mod' || f.endsWith('/go.mod'))
    .map((f) => dirname(f));
  return [...new Set(dirs)].sort((a, b) => (a === '.' ? -1 : b === '.' ? 1 : a.localeCompare(b)));
}

// classify — what one `go fix` invocation's result means: `clean`, `pending`
// (check mode: a rewrite to report) or `failed`.
export function classify(mode, res) {
  if (res.status === 0) return mode === 'check' && diffFiles(res.stdout).length > 0 ? 'pending' : 'clean';
  if (mode === 'check' && res.status === 1 && res.stderr.trim() === '' && diffFiles(res.stdout).length > 0) {
    return 'pending';
  }
  return 'failed';
}

export function run({ mode = 'check', configPath = '.golangci.yml', root = process.cwd(), runner = capture } = {}) {
  const tags = parseBuildTags(readFileSync(configPath, 'utf8'));
  const ls = runner('git', ['ls-files', '-z', '--', 'go.mod', '*/go.mod'], { cwd: root });
  if (ls.status !== 0) {
    error(`go-fix-check: \`git ls-files\` failed: ${ls.stderr.trim()}`);
    return 1;
  }
  const modules = moduleDirs(ls.stdout);
  if (modules.length === 0) {
    error('go-fix-check: no go.mod is tracked under the repository root');
    return 1;
  }
  const pending = new Set();
  for (const mod of modules) {
    const cwd = join(root, mod);
    for (const pass of passes(tags)) {
      const args = ['fix', ...(mode === 'check' ? ['-diff'] : []), ...pass.args, './...'];
      const res = runner('go', args, { cwd });
      const outcome = classify(mode, res);
      if (outcome === 'failed') {
        error(
          `go-fix-check: \`go ${args.join(' ')}\` failed in ${mod} (${pass.name} pass, exit ${res.status}): ` +
            res.stderr.trim(),
        );
        return 1;
      }
      if (outcome === 'clean') continue;
      for (const f of diffFiles(res.stdout)) {
        const rel = relative(root, f);
        pending.add(rel);
        error(`go fix (${pass.name} pass) would rewrite this file`, { file: rel, title: 'go-fix-check' });
      }
      group(`go fix -diff (${mod}, ${pass.name} pass)`, () => log(res.stdout));
    }
  }
  if (pending.size > 0) {
    error(
      `go-fix-check: ${pending.size} file(s) are not a fixed point of \`go fix\`. ` +
        'Run `just go-fix`, review the diff, and commit it.',
    );
    return 1;
  }
  notice(
    mode === 'apply'
      ? `go-fix-check: applied go fix over the tagged and untagged build configurations of ${modules.length} module(s)`
      : `go-fix-check: ${modules.length} module(s) are a fixed point of go fix in both build configurations`,
  );
  return 0;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const mode = process.env.GO_FIX_MODE || 'check';
  if (!MODES.has(mode)) {
    error(`go-fix-check: GO_FIX_MODE must be one of ${[...MODES].join(', ')}, got ${JSON.stringify(mode)}`);
    process.exit(2);
  }
  process.exit(run({ mode, configPath: process.env.GOLANGCI_FILE || '.golangci.yml' }));
}
