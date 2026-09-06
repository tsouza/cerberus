# === Lint / format ===

# Run Go linters over both build configurations.
#
# One invocation analyses one build configuration. The first pass takes the tag
# union from `.golangci.yml`, which is every tagged file in the tree; the second
# names an inert tag to clear that union, which is the `!chdb` / `!chaos_sleep`
# stubs. Every `//go:build` line in the tree is a single term, so the two passes
# together leave no file unanalysed.

# Run Go linters over both build configurations.
lint:
    golangci-lint run ./...
    golangci-lint run --build-tags {{LINT_UNTAGGED_BUILD}} ./...

# Validate GitHub Actions workflow files (expression contexts, action
# inputs, shellcheck of run blocks). Deliberately separate from `lint`
# (Go): workflow-file defects otherwise surface only as server-side
# zero-job "invalid workflow file" failure runs, which silently
# prevent required pull_request checks from ever being scheduled —
# the #749 secrets-in-step-if incident left the PR BLOCKED on four
# required contexts that could never report.

# Validate the GitHub Actions workflow files with actionlint.
lint-actions:
    actionlint

# The engine version and the glob set both live in
# `.github/scripts/markdownlint-run.mjs`, which is also what ci.yml's `lint`
# job and lefthook's `pre-commit` hook run. That is the point: this recipe used
# to pin its own older markdownlint, so a rule `.markdownlint.yaml` configures
# (MD060) was enforced in CI and invisible here, and `just lint-md` reported
# success on a file CI then failed (tsouza/cerberus#2997).
#
# NOT everything `lint-md` reports is auto-fixable by `fmt-md`. MD060
# (table-column-style) has no fixer in markdownlint at all — realign tables
# with `python3 scripts/align-md-tables.py <file>...`, which lefthook runs as
# its own `md-table-align` pre-commit hook. `fmt-md` says so when MD060
# survives the fix pass, rather than leaving a clean-looking run that still
# fails `lint-md`.

# Lint all Markdown files, with the same pinned engine CI runs.
lint-md:
    node .github/scripts/markdownlint-run.mjs

# Auto-fix Markdown lint issues where possible (MD060 needs align-md-tables.py).
fmt-md:
    node .github/scripts/markdownlint-run.mjs --fix

# Format Go code.
fmt:
    gofumpt -l -w .
    goimports -l -w -local {{MODULE}} .

