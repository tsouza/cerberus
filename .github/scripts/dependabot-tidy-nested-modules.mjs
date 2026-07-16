// dependabot-tidy-nested-modules.mjs — .github/workflows/dependabot-tidy-nested-modules.yml,
// the "tidy" step.
//
// test/oracle is a nested Go module (its own go.mod) that carries
// `replace github.com/tsouza/cerberus => ../..` so its AGPL-quarantined
// oracle tests can import cerberus's internal/** packages (see the module
// doc-comment in test/oracle/go.mod and docs re: the agpl-clean gate). That
// local-path replace entangles its module graph with the root's: almost
// every dependency Dependabot bumps in the root go.mod is also an indirect
// require in test/oracle/go.mod, so a root-only bump leaves the nested
// module's go.mod/go.sum stale. `.github/dependabot.yml` only watches
// `directory: /` (by design — a second Dependabot directory would open a
// SEPARATE, unsynced PR, not fix this), so nothing ever tidies test/oracle
// on its own. Left alone, `ci.yml`'s "test oracle module" step fails on
// nearly every Dependabot PR with "go: updates to go.mod needed; to update
// it: go mod tidy" (see PR #1211) and a human has to notice, run
// `go mod tidy` locally, and push a fixup commit.
//
// This script closes that loop: run inside a Dependabot PR's checkout
// (after Dependabot's own go.mod/go.sum edits are already on disk), it runs
// `go mod tidy` in each nested module, and — only if that produced a diff —
// commits and pushes a fixup straight to the PR branch. bench/histogram is
// a second nested module but deliberately does NOT depend on the root
// module (see its go.mod doc-comment), so it never needs this and is not
// included by default.
//
// Env contract:
//   NESTED_MODULE_DIRS  space-separated dirs, each containing a go.mod that
//                        must be re-tidied (default: "test/oracle")
//   BRANCH               branch to push the fixup commit to (required)
//   GIT_USER_NAME         commit author name (default: "github-actions[bot]")
//   GIT_USER_EMAIL        commit author email
//                        (default: "github-actions[bot]@users.noreply.github.com")
//
// Exit codes: 0 = nothing to do, or fixup committed + pushed; 1 = `go mod
// tidy` or a git command failed.

import process from 'node:process';
import { error, exec, git, notice } from './lib/gh.mjs';

const dirs = (process.env.NESTED_MODULE_DIRS || 'test/oracle').split(/\s+/).filter(Boolean);
const branch = process.env.BRANCH;
const gitUserName = process.env.GIT_USER_NAME || 'github-actions[bot]';
const gitUserEmail =
  process.env.GIT_USER_EMAIL || 'github-actions[bot]@users.noreply.github.com';

if (!branch) {
  error('dependabot-tidy-nested-modules.mjs: BRANCH env var is required');
  process.exit(1);
}

for (const dir of dirs) {
  notice(`go mod tidy in ${dir}`);
  exec('go', ['mod', 'tidy'], { cwd: dir });
}

const status = git(['status', '--porcelain', '--', ...dirs]);
if (status.status !== 0) {
  error(`git status failed: ${status.stderr.trim()}`);
  process.exit(1);
}

if (status.stdout.trim() === '') {
  notice(`nested module(s) already tidy: ${dirs.join(', ')}`);
  process.exit(0);
}

exec('git', ['config', 'user.name', gitUserName]);
exec('git', ['config', 'user.email', gitUserEmail]);
exec('git', ['add', '--', ...dirs]);
exec('git', [
  'commit',
  '-m',
  `chore: go mod tidy nested modules\n\nDependabot bumped a dependency shared with test/oracle's module\ngraph (entangled via its local-path replace on the root module);\nre-tidying keeps its go.mod/go.sum consistent.`,
]);
exec('git', ['push', 'origin', `HEAD:${branch}`]);

notice(`pushed go mod tidy fixup for ${dirs.join(', ')} to ${branch}`);
