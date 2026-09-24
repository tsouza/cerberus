// go-fix-check — the tree must be a fixed point of the pinned toolchain's
// `go fix`.
//
// `go fix` runs the Go release's modernizer suite (range-over-int, min/max,
// slices/maps helpers, strings.Cut / SplitSeq, new(expr), WaitGroup.Go, …).
// Without a gate, one change applies it and the next reintroduces the idioms it
// removed. Only a check that fails on a non-empty `go fix -diff` holds the tree
// at the fixed point.
//
// Two passes, mirroring golangci-lint's two build configurations (ci.yml's
// `lint` job): one with every build tag in `.golangci.yml`'s `run.build-tags`
// union, and one untagged. Every `//go:build` line in the tree is a single term,
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
// Exit: 0 when both passes leave the tree unchanged (or, in apply mode, when
// both passes ran), 1 on a pending rewrite or a `go fix` failure, 2 on a usage
// error.

import { readFileSync } from 'node:fs';
import { relative } from 'node:path';
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

export function run({ mode = 'check', configPath = '.golangci.yml', root = process.cwd(), runner = capture } = {}) {
  const tags = parseBuildTags(readFileSync(configPath, 'utf8'));
  const pending = new Set();
  for (const pass of passes(tags)) {
    const args = ['fix', ...(mode === 'check' ? ['-diff'] : []), ...pass.args, './...'];
    const res = runner('go', args, { cwd: root });
    if (res.status !== 0) {
      error(`go-fix-check: \`go ${args.join(' ')}\` failed (${pass.name} pass): ${res.stderr.trim()}`);
      return 1;
    }
    if (mode === 'apply') continue;
    const files = diffFiles(res.stdout);
    if (files.length === 0) continue;
    for (const f of files) {
      const rel = relative(root, f);
      pending.add(rel);
      error(`go fix (${pass.name} pass) would rewrite this file`, { file: rel, title: 'go-fix-check' });
    }
    group(`go fix -diff (${pass.name} pass)`, () => log(res.stdout));
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
      ? 'go-fix-check: applied go fix over the tagged and untagged build configurations'
      : 'go-fix-check: the tree is a fixed point of go fix in both build configurations',
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
