#!/usr/bin/env bash
# Prints the value of COMPOSE_PROJECT_SUFFIX for this checkout: the string every
# compose stack in the tree appends to its own project name.
#
# Docker Compose scopes container, network and volume names by project name, so
# two stacks with the same project name are the SAME stack as far as the daemon
# is concerned — `up` adopts the other one's containers and `down -v` destroys
# them. Every compose file here declares a fixed `name:`, which is what makes
# the stacks addressable by `just` recipes and CI jobs. It also means two
# checkouts of this repository on one machine resolve identical project names:
# the derived-from-directory default compose would otherwise use is
# `compatibility/loki` -> `loki`, `tier1-dual` -> `tier1-dual`, the same in
# every checkout, so dropping `name:` would not buy isolation either.
#
# The suffix closes that: each compose file spells its project name
# `<stable-base>${COMPOSE_PROJECT_SUFFIX:-}`, so
#
#   suffix empty       ->  cerberus-migration-tier1
#   suffix -0ecfe87b   ->  cerberus-migration-tier1-0ecfe87b
#
# and the stacks stay distinct from each other either way — a single global
# COMPOSE_PROJECT_NAME would instead collapse all seven into one project, where
# five different `clickhouse` services would fight over one container. The same
# suffix goes onto the tag of every image this tree builds into the local
# daemon, which is namespaced by the daemon rather than by the project.
#
# Who gets a suffix:
#
#   primary checkout  ->  empty. A plain clone and every CI checkout are
#                         primary, so their project, container, volume and
#                         image names are exactly what they would be with no
#                         suffix at all.
#   linked worktree   ->  `-` + a short hash of the worktree's root path. Agents
#                         work in `.claude/worktrees/<id>/` (see CLAUDE.md
#                         "Subagent worktree isolation"), so each agent's stack
#                         is a distinct set of objects without opting in.
#
# What this does NOT isolate: published host ports. Every stack still binds
# fixed literals, so two worktrees cannot run the SAME stack at the same time —
# the second `up` fails on a port bind. That is the intended outcome: a loud
# refusal is the honest failure, and it is a different bug from the one fixed
# here, where the second checkout silently ADOPTED the first's containers and
# `down -v --remove-orphans` destroyed them.
#
# The hash is derived from the path and nothing else — never from a timestamp or
# a random value — because `down` runs in a later process than `up` and has to
# resolve the same project name.
#
# Wiring (the callers that populate the variable, all reading this one script):
#   - Justfile        `export COMPOSE_PROJECT_SUFFIX` reaches every recipe.
#   - .envrc          direnv exports it into interactive dev shells, so a bare
#                     `docker compose up` in a worktree is isolated too.
#   - the standalone harness entrypoints (compatibility/*/scripts/*.sh,
#     bench/histogram/run.sh), which CI invokes directly rather than via `just`.
#
# test/regression/compose_project_isolation_test.go discovers those callers
# rather than listing them: every shell script in the tree that invokes
# `docker compose` has to derive and export the variable through this script.
#
# An already-set value is echoed back unchanged, so those wirings compose
# without fighting each other and a caller can pin a project suffix explicitly.

set -eu -o pipefail

if [ -n "${COMPOSE_PROJECT_SUFFIX:-}" ]; then
    printf '%s' "$COMPOSE_PROJECT_SUFFIX"
    exit 0
fi

# The checkout to describe is the one holding THIS script, not whichever
# directory the caller happens to be in: `just` is invoked with a working
# directory of its own choosing, and the harness scripts cd into their own
# subtree before calling here.
cd "$(dirname "$0")"

# Not a git checkout (an extracted release tarball, say): nothing path-stable to
# derive from, so the compose files keep their bare defaults.
git_dir=$(git rev-parse --path-format=absolute --absolute-git-dir 2>/dev/null) || exit 0
common_git_dir=$(git rev-parse --path-format=absolute --git-common-dir)

# The primary checkout's git dir IS the common git dir; a linked worktree's is
# `<common>/worktrees/<id>`.
if [ "$git_dir" = "$common_git_dir" ]; then
    exit 0
fi

# Wide enough that the handful of worktrees a machine holds at once will not
# collide, short enough to keep `docker ps` readable.
suffix_hex_chars=8

# Each step lands in its own variable so `set -e` aborts on a failed git or cut
# rather than letting an empty substitution print a bare `-`, which would be a
# legal project-name fragment shared by every checkout that hit the same
# failure.
worktree_root=$(git rev-parse --show-toplevel)
worktree_hash=$(printf '%s' "$worktree_root" | git hash-object --stdin)
suffix=$(printf '%s' "$worktree_hash" | cut -c "1-${suffix_hex_chars}")
printf -- '-%s' "$suffix"
