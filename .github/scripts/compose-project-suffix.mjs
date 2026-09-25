// compose-project-suffix.mjs — print the value of COMPOSE_PROJECT_SUFFIX for
// this checkout: the string every compose stack in the tree appends to its
// own project name.
//
// Docker Compose scopes container, network and volume names by project name,
// so two stacks with the same project name are the SAME stack as far as the
// daemon is concerned — `up` adopts the other one's containers and `down -v`
// destroys them. Every compose file here declares a fixed `name:`, which is
// what makes the stacks addressable by `just` recipes and CI jobs, and which
// also means two checkouts of this repository on one machine resolve
// identical project names.
//
// Each compose file spells its project name
// `<stable-base>${COMPOSE_PROJECT_SUFFIX:-}`, so
//
//   suffix empty       ->  cerberus-migration-tier1
//   suffix -0ecfe87b   ->  cerberus-migration-tier1-0ecfe87b
//
// and the stacks stay distinct from each other either way. The same suffix
// goes onto the tag of every image this tree builds into the local daemon,
// which is namespaced by the daemon rather than by the project.
//
// Who gets a suffix:
//
//   primary checkout  ->  empty. A plain clone and every CI checkout are
//                         primary, so their project, container, volume and
//                         image names are exactly what they would be with no
//                         suffix at all.
//   linked worktree   ->  `-` + the first eight hex characters of
//                         `git hash-object` over the worktree's root path.
//   not a git checkout -> empty.
//
// Published host ports are NOT isolated: every stack binds fixed literals, so
// two worktrees cannot run the SAME stack at the same time — the second `up`
// fails on a port bind.
//
// The suffix is a pure function of the worktree path, because `down` runs in
// a later process than `up` and has to resolve the same project name. The
// checkout described is the one holding THIS file, not the caller's working
// directory.
//
// Callers (all run this one script):
//   - Justfile        `export COMPOSE_PROJECT_SUFFIX` reaches every recipe.
//   - .envrc          direnv exports it into interactive dev shells.
//   - bench/histogram/run.sh, a standalone harness entrypoint.
//   - lib/compat-compose-lifecycle.mjs, for the compatibility harnesses.
// test/regression/compose_project_isolation_test.go discovers every shell
// script that invokes `docker compose` and requires it to derive and export
// the variable through this script.
//
// Env contract:
//   COMPOSE_PROJECT_SUFFIX  optional; an already-set value is printed back
//                           unchanged, so a caller can pin a suffix.
//
// Usage:
//   node .github/scripts/compose-project-suffix.mjs
//
// Output: the suffix on stdout with no trailing newline (empty for a primary
// checkout). Exit codes: 0 = printed; 1 = a git call failed inside a linked
// worktree.

import { spawnSync } from 'node:child_process';
import { dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

import { errorStderr } from './lib/gh.mjs';

// Wide enough that the handful of worktrees a machine holds at once will not
// collide, short enough to keep `docker ps` readable.
const SUFFIX_HEX_CHARS = 8;

const HERE = dirname(fileURLToPath(import.meta.url));

// gitOut — stdout of `git <args>` run in this file's checkout, with trailing
// newlines removed; null when git fails.
function gitOut(args, input) {
  const res = spawnSync('git', args, { cwd: HERE, encoding: 'utf8', input });
  if (res.status !== 0) return null;
  return res.stdout.replace(/\n+$/, '');
}

// gitOrDie — gitOut that aborts rather than returning null. An empty
// substitution would print a bare `-`, a legal project-name fragment shared by
// every checkout that hit the same failure.
function gitOrDie(args, input) {
  const out = gitOut(args, input);
  if (out === null || out === '') {
    errorStderr(`compose-project-suffix: git ${args.join(' ')} failed`);
    process.exit(1);
  }
  return out;
}

export function deriveSuffix() {
  const pinned = process.env.COMPOSE_PROJECT_SUFFIX ?? '';
  if (pinned !== '') return pinned;

  const gitDir = gitOut(['rev-parse', '--path-format=absolute', '--absolute-git-dir']);
  if (gitDir === null) return '';
  const commonGitDir = gitOrDie(['rev-parse', '--path-format=absolute', '--git-common-dir']);

  // The primary checkout's git dir IS the common git dir; a linked worktree's
  // is `<common>/worktrees/<id>`.
  if (gitDir === commonGitDir) return '';

  const worktreeRoot = gitOrDie(['rev-parse', '--show-toplevel']);
  const worktreeHash = gitOrDie(['hash-object', '--stdin'], worktreeRoot);
  return `-${worktreeHash.slice(0, SUFFIX_HEX_CHARS)}`;
}

if (process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href) {
  process.stdout.write(deriveSuffix());
}
