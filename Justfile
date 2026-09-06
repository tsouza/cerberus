# Cerberus task runner. All commands go through `just`.
# Run `just` for the full recipe list.

set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

GOLANGCI_LINT_VERSION := "v2.12.2"
GOFUMPT_VERSION := "v0.7.0"
GOIMPORTS_VERSION := "latest"
GREMLINS_VERSION := "v0.6.0"
ACTIONLINT_VERSION := "v1.7.12"
MODULE := "github.com/tsouza/cerberus"

# The build tag the untagged lint pass runs under. `.golangci.yml` declares the
# union of every tag in the tree, and golangci-lint's flag REPLACES that value
# rather than adding to it — so naming a tag no file constrains on is how the
# second pass gets the plain build configuration, the one that owns the `!chdb`
# and `!chaos_sleep` stubs the union excludes.
# test/regression/lint_build_tags_test.go asserts it stays inert.
LINT_UNTAGGED_BUILD := "cerberus_untagged_build"

# Per-checkout compose project suffix, exported into every recipe so no compose
# call site has to remember it: each docker-compose file spells its project name
# `<stable-base>${COMPOSE_PROJECT_SUFFIX:-}`, and a compose invocation from a
# recipe, from a harness script a recipe runs, or from a Go test a recipe runs
# all inherit the same value. Every image tag this tree builds into the local
# daemon carries it too, because a tag is namespaced by the daemon rather than
# by the project. Empty in a primary checkout and in CI — project, container,
# network, volume and image names are then exactly what they are without this
# mechanism — and a short path-derived hash in a linked worktree, so each agent's
# stack is a distinct set of objects rather than a shared one. Host ports stay
# fixed, so two worktrees still cannot run the same stack at once; that now fails
# on a port bind instead of silently adopting the other checkout's containers.
# See scripts/compose-project-suffix.sh.
#
# `just` evaluates this before running ANY recipe, so the derivation has to hold
# for a checkout with no git in sight (it prints nothing, and the bare project
# names apply) and for an invocation whose working directory is anywhere at all
# (hence the justfile-relative path). What it does not paper over is a checkout
# missing the script: that fails loudly, naming the absolute path, rather than
# defaulting to a suffix that would silently share stacks between worktrees.
export COMPOSE_PROJECT_SUFFIX := shell('exec "$1"', justfile_directory() / "scripts/compose-project-suffix.sh")

# Default: list recipes.
default:
    @just --list

# Recipe bodies live under just/*.just, imported flat (see docs/toolchain.md).
# Every recipe below is callable by bare name exactly as before the split —
# `import` merges these into this file's own namespace, unlike `mod`.
import "just/common.just"
import "just/tools.just"
import "just/build.just"
import "just/generate.just"
import "just/test.just"
import "just/mutation.just"
import "just/lint.just"
import "just/deps.just"
import "just/chdb.just"
import "just/e2e.just"
import "just/e2e-bwc.just"
import "just/compat.just"
import "just/migration.just"
import "just/release.just"

# === CI entry point ===

# Lint + test + build. Used by ci.yml.
ci: lint test build
