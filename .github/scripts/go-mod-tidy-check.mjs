// go-mod-tidy-check.mjs — `go mod tidy` every module and fail when it changed
// go.mod / go.sum, for ci.yml's `check-build` job.
//
// goreleaser's `before` hook runs `go mod tidy` on every release, so a go.mod
// that is not tidy means the release mutates the tree it is cutting from —
// that is how `spf13/pflag` sat marked `// indirect` while cmd/cerberus
// imported it directly. Every module is tidied, because the nested
// `test/oracle` module drifts the same way and the root suite never reaches
// it. The step used to be an inline loop plus a diff branch in the workflow.
//
// Env:
//   MODULES   space-separated module directories (default `. test/oracle`)
//
// Exit: 0 when tidy changed nothing; 1 with the diff printed otherwise, or
// when `go mod tidy` itself fails.

import { spawnSync } from 'node:child_process';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';

import { capture, error, log } from './lib/gh.mjs';

export const DEFAULT_MODULES = ['.', 'test/oracle'];

// GO_MOD_PATHSPECS covers go.mod / go.sum at the root and in every nested
// module.
const GO_MOD_PATHSPECS = ['**/go.mod', '**/go.sum', 'go.mod', 'go.sum'];

export function parseModules(raw) {
  const modules = String(raw ?? '')
    .split(/\s+/)
    .filter(Boolean);
  return modules.length > 0 ? modules : DEFAULT_MODULES;
}

function main() {
  const modules = parseModules(process.env.MODULES);
  for (const mod of modules) {
    const res = spawnSync('go', ['mod', 'tidy'], { cwd: mod, stdio: 'inherit' });
    if (res.status !== 0) throw new Error(`go mod tidy failed in ${mod} (status ${res.status})`);
  }
  const diff = capture('git', ['diff', '--quiet', '--', ...GO_MOD_PATHSPECS]);
  if (diff.status === 0) {
    log(`go.mod / go.sum are tidy in ${modules.join(', ')}`);
    return;
  }
  const shown = capture('git', ['--no-pager', 'diff', '--', ...GO_MOD_PATHSPECS]);
  process.stdout.write(shown.stdout);
  throw new Error('go mod tidy changed go.mod/go.sum — commit the result');
}

const invokedDirectly = process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (e) {
    error(e.message);
    process.exit(1);
  }
}
