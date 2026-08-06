# Agent workflow

How a change gets from an idea to a merged PR, and the harness that enforces it. `CLAUDE.md` states
the invariants in one line each; this document holds the mechanics behind them.

## The change lifecycle

A non-trivial change runs through five stages in order. A trivial change — a typo, a comment, a
one-line fix with an obvious test — goes straight to the PR.

1. **Plan mode.** Read before writing. Establish where the change belongs using the architecture
   decision tree in `CLAUDE.md`, and which test layer will pin it using `docs/test-strategy.md`.
   Produce no edits at this stage.
2. **Requirements interview.** Resolve the ambiguities that would otherwise be resolved silently at
   implementation time: which of the three heads is in scope, what the reference backend answers
   today, whether the change is observable on the wire or only in the plan, and which generated
   artefacts will need regenerating. When the answer is unavailable — an agent running unattended,
   for instance — make the call, state it explicitly, and continue rather than blocking.
3. **Spec at `docs/specs/<feature>.md`.** One file per change, named for the change rather than the
   issue number so it stays readable. A spec states, in this order: the problem with evidence (file
   and line, or the reference backend's answer next to cerberus's); the scope boundary, meaning both
   what is included and what is explicitly not; the design, at the level of which packages gain which
   behaviour; the verification plan naming the specific test layers and files; and the risks. A spec
   that cannot name its verification layer is not finished.
4. **Numbered task list for owner review.** Each task independently reviewable, ordered so the tree
   is green between any two of them, with its verification named. This is the artefact the owner
   approves; approval of the spec is not approval of the task list.
5. **Implementation.** One PR per coherent change, following the shipping ritual below.

Specs are living documents while the change is in flight and archival once it merges. They record the
decision, not the history of arriving at it.

## Shipping ritual

Branch off the current `origin/main`. Never branch off another in-flight branch: if that parent
squash-merges while the work proceeds, the base no longer exists in `main`'s history and the PR shows
a doubled diff — every file the parent touched, replayed. The recovery is to cherry-pick onto a fresh
branch off `origin/main`, so avoid the situation instead. Re-check the base before pushing.

The push and the `gh pr create` are a single step. A pushed branch with no PR appears in no
`gh pr list`, gets no check runs, and reaches no reviewer; if the work it belongs to merges without
it, its commits are stranded. A branch that is not ready for review is a draft PR, not an absent one.

Verify the push landed by SHA rather than by the client's own report:

```bash
rtk proxy git ls-remote origin <branch>
```

Never push to the head branch of a PR that has already merged. A merged PR is closed: its head stops
advancing, and the commit reaches no branch anyone reads. Confirm with
`gh pr view <n> --json state` first; if it has merged, cut a fresh branch off `origin/main`.

Merge with `gh pr merge --squash --delete-branch` once every required check is green. Never
`gh pr merge --admin`. Branch protection sets `enforce_admins: false`, which makes this policy
discipline rather than a mechanical block — honour it regardless. A red required check is fixed at
the code or at the workflow, never bypassed.

A one-commit PR squashes the **commit** subject, not the PR title, so a `chore:` commit under a
`feat:` PR title lands in the changelog as a chore.

## Required checks and the gate posture

The authoritative list of required contexts is the API, not any document:

```bash
gh api repos/tsouza/cerberus/branches/main/protection --jq '.required_status_checks.contexts[]'
```

`docs/test-strategy.md` carries the per-gate reference: what each context proves, what it cannot see,
and which layer covers the residue. Three postures are worth knowing:

- **PR gates** run on every pull request, the `mutation` lane among them: on a PR it runs the
  gremlins legs whose scope that PR changed, and sweeps the full matrix on push, nightly, dispatch,
  and `release/*` PRs.
- **Release gates** — `compose-smoke`, `dashboard` (k3d + Grafana + Playwright), and `profile` —
  short-circuit to a green no-op on an ordinary PR and do their real work on the merge commit. They
  are named in `release.yml`'s `RELEASE_REQUIRED_CHECKS`, so nothing publishes until each posts green
  on the commit being shipped. `compose-smoke` narrows that short-circuit: it is the only lane that
  runs against a real ClickHouse server rather than chDB, so a PR touching `internal/chsql`,
  `internal/api`, `internal/chclient`, or `cmd/cerberus` boots the stack on the PR itself. The scope
  rule lives in `.github/scripts/compose-smoke-scope.mjs`.
- **Merge-queue posture.** Every workflow owning a required context also declares `merge_group:`, so
  the same check runs report under byte-identical names on the projected trunk, and a queued entry is
  held to the pull-request posture rather than the push posture — same short-circuits, same
  diff-scoped lane selection, read off the merge group's own `base_sha..head_sha`. `CodeQL` is the
  one context with no `merge_group` half, because code-scanning default setup dispatches on `push`
  and `pull_request` only. `test/regression/merge_queue_test.go` pins both invariants, including that
  no `cancel-in-progress` reachable from a merge-group run can be true: GitHub reads a cancelled
  check run as a failure and dequeues the PR.

Force-push and deletion are off on `main`, and linear history is off so the GitHub "Update branch"
button works for stale PRs.

## Work that belongs outside this change

Work that surfaces mid-change and genuinely belongs elsewhere becomes a GitHub Issue, created before
the PR that found it merges. Prose in a PR body is not a resting place: once the PR merges, the
sentence appears in no list, no gate reports on it, and nobody is assigned. An Issue stays open until
someone closes it, which is the point.

Verify the finding before filing — read the function, grep the site. A phantom issue is as costly as
a dropped one, and a plausible filename is not evidence. Verify negative verdicts to the same
standard: "this is already fixed, close it" needs the same check at the issue's own cited file and
line as "this is real" does.

The PR body may reference the issue; it may never substitute for one. Add `Closes #N` when the PR
actually resolves that issue, and never for issues spun out of the PR's own findings — those stay
open.

This is enforced, not conventional. The required `forbid-deferral` check
(`.github/scripts/forbid-deferral.mjs`) scans the change's own additions — the PR description, the
commit messages in its range, and the `+` lines of its diff — for the marker classes in that module's
`DEFERRAL_MARKERS` table, and fails unless each match cites an issue in this repository that is open
and is an issue rather than a pull request. Citation scope follows the author's own structure: a
marker on a markdown heading is satisfied anywhere in the section that heading introduces, any other
prose marker is satisfied within its own paragraph, and a marker in the diff is satisfied within a
fixed line window. The gate deliberately does not scan the tree at large, because the same phrases
are ordinary architecture prose elsewhere and a gate that fired on those would get routed around.
There is no tolerance file and no exemption list.

## Worktree isolation

Each agent gets its own worktree and its own branch. Every git and filesystem operation belongs
inside that path, passed absolutely on every call — `Bash`, `Read`, `Write`, and `Edit` alike. If a
tool call resets the working directory between invocations, re-anchor with an absolute path rather
than trusting the inherited one.

The protection git offers is path-based, not agent-based. The same branch cannot be checked out in
two worktrees, and each worktree has its own `HEAD` ref, but none of that helps when git is run from
the wrong path. Every checkout of this repository shares one object store while sitting on a
different branch, so a `git commit` issued from another checkout stacks onto whichever branch that
checkout happens to have out — somebody else's PR. Three post-mortems (issues #207, #209, and #210)
report exactly that shape, and the investigation under task #213 traced every one of them to an agent
using another checkout's path because a briefing had pasted it in as "the repo".

### Recovery when contamination is suspected

1. Return to the assigned worktree path.
2. `git worktree list` — confirm the worktree appears with the expected branch. If it does not, the
   work has been happening in the wrong tree from the start.
3. From the assigned worktree, `git log --oneline origin/main..HEAD`. Any commit that is not
   recognisable is contamination.
4. In the other checkout, `git status` and `git log --oneline` on whatever branch it has out.
   Uncommitted edits or commits authored elsewhere that do not belong to that branch's PR are the
   bleed-through.
5. Uncommitted bleed-through: `git checkout -- <files>` there, but only when nothing else is in
   flight in that tree. When in doubt, ask rather than reverting a shared tree. Re-apply the change
   in the correct worktree using the correct absolute path.
6. Committed bleed-through: `git cherry-pick <sha>` from inside the correct worktree, then
   `git revert <sha>` and push from the wrong one. Never `git reset --hard` a shared branch — that
   rewrites history other agents may have pushed.
7. Record the contamination SHAs, file paths, and worktree paths on task #213 so the pattern does not
   repeat silently.

## The checked-in Claude Code harness

`.claude/` is version-controlled so every agent and contributor gets the same behaviour. Only
`.claude/settings.local.json` and `.claude/worktrees/` are ignored.

### Hooks (`.claude/settings.json`)

- **PostToolUse on `Edit` / `Write`** runs `.claude/hooks/format-touched-file.mjs`, which applies
  `gofumpt -extra -w` and `goimports -w -local github.com/tsouza/cerberus` to the single `.go` file
  just written. `-extra` is not optional: `.golangci.yml` sets `gofumpt.extra-rules: true`, so the
  CI `lint` gate enforces the extra rules and a plain `gofumpt` leaves formatting CI will reject.
  The hook is scoped to one file and runs no repository-wide linter.
- **PreToolUse on `Bash`** runs `.claude/hooks/guard-git.mjs` for `git commit` and `git push`
  invocations. It is a fast guard, not a second validation layer: it blocks a commit or push aimed at
  `main`, and it blocks when lefthook's git hooks are not installed, because lefthook is the layer
  that genuinely owns pre-commit and pre-push validation. Setting `CERBERUS_PRECOMMIT_FULL_CI=1`
  additionally runs `just ci` before each commit and push; it is off by default because that
  duplicates a better-targeted layer at a cost of minutes per commit.

The guard deliberately does not run the test suite or `golangci-lint`. `lefthook.yml` already mirrors
the CI `check`, `lint`, and `forbid-skip` jobs on `pre-push`, once per push instead of once per
commit, and a linter invoked from a fresh agent worktree can report a clean tree without having
analysed it — a false green is worse than no local check, because it is trusted.

### Reproducing a red check

A speculative pre-flight over the whole tree is waste; reproducing a *known* red check is not. Once
CI names a failure, that failure is reproduced locally, narrowed to the thing that failed, and the
fix is confirmed locally before the next push. A full run is roughly nineteen checks and forty
minutes, so pushing a guess and waiting to find out spends CI credits and wall-clock on feedback a
targeted local run returns in seconds.

Narrowing means the failing unit and nothing around it:

| Red check                                                         | Local reproduction                                       |
| ----------------------------------------------------------------- | -------------------------------------------------------- |
| `check-test`                                                      | `go test -run '^TestName$/^subtest$' ./internal/<pkg>/`  |
| `forbid-skip`, `forbid-deferral`, `forbid-sql-raw`, `config-docs` | `node .github/scripts/<gate>.mjs` from the worktree root |
| `check-build`                                                     | `go build ./<pkg>/` for the package named in the log     |

Where a recipe exists, invoke the recipe rather than the underlying command: the recipe carries the
build tags, the race flag and the timeouts, and a direct `go test` inherits Go's own ten-minute
default instead — a bypass that manufactures a timeout the recipe would never have hit.

Three lanes genuinely resist local reproduction, and only these justify a CI round-trip:

- **`lint`** — golangci-lint run from an agent worktree reports "No issues found" without analysing.
  Read the CI log for the linter, the rule, and the `file:line`, then reason about that line.
- **`strict-scan` and the compatibility lanes** — these need a real ClickHouse. chDB coerces column
  types production rejects, so a green chDB golden is not evidence for either.
- **`e2e`, `compose-smoke`, and the crawl ratchet** — these need k3d or Docker Compose.

The Compose lanes carry a concurrency hazard: there is no per-worktree Compose project-name
isolation, so a compose run started from one worktree corrupts another's containers. Compose stays
down while any other agent is live.

### Project subagents (`.claude/agents/`)

- `code-reviewer` — reviews the working diff for regressions, missing tests, and invariant breaches
  before a commit. Read-only by construction.
- `explorer` — cheap, fast codebase search, pinned to a small model.
- `test-runner` — runs a suite and reports failures only.
- `chsql-emitter` — domain specialist for `internal/chsql`, the typed ClickHouse SQL emitter.

Subagents have no persistent memory between invocations. Each starts from its prompt and whatever the
caller passes in, which is why each agent file points at the specific documents and scripts it needs
rather than assuming recall from a previous run.
