# === Release (controlled local cut) ===
#
# These mirror prepare-release.yml for when the workflow_dispatch isn't
# available, and — critically — ENFORCE the canonical release-branch names
# so nobody hand-rolls an inconsistent one:
#   - tip-of-main release PR : release/v<version>-chart-<chartVersion>
#   - maintenance backport   : release/<major>.<minor>.x   (publishing is
#     gated on merge of a validated release PR — resolve-release-trigger.mjs
#     + release-version-gate.mjs key off this branch name, so a wrong one
#     makes the publish step REJECT the run)
# Both wrap the same .github/scripts/prepare-release.mjs + helm-docs the CI
# uses, so the opened PR is drift-clean.

# Stage a tip-of-main release and open its PR on the canonical branch.
# Usage: just release-prep 1.4.0            (chart bump defaults to patch)
#        just release-prep 1.5.0 minor

# Stage a tip-of-main release and open its PR on the canonical branch.
release-prep version chart_bump="patch":
    #!/usr/bin/env bash
    set -euo pipefail
    git fetch origin main
    git switch -c "release/v{{version}}-staging" origin/main
    VERSION="{{version}}" CHART_BUMP="{{chart_bump}}" PR_BODY_FILE=release-pr-body.md \
        node .github/scripts/prepare-release.mjs
    chart="$(awk '/^version:/{print $2; exit}' deploy/helm/cerberus/Chart.yaml)"
    docker run --rm -v "$PWD/deploy/helm:/helm-docs" -u "$(id -u)" \
        jnorwood/helm-docs:v1.14.2 --chart-search-root=/helm-docs --template-files=README.md.gotmpl
    branch="release/v{{version}}-chart-${chart}"
    git branch -m "$branch"
    git add deploy/helm/cerberus/Chart.yaml deploy/helm/cerberus/README.md CHANGELOG.md
    git commit -m "chore(release): cerberus v{{version}} / chart ${chart}"
    git push -u origin "$branch"
    gh pr create --base main --head "$branch" \
        --title "chore(release): cerberus v{{version}} / chart ${chart}" \
        --body-file release-pr-body.md
    echo "Opened release PR on $branch. MERGING it publishes — release.yml runs on push-to-main, gates on the staged version bumps, and creates v{{version}} itself. Do not tag by hand."

# Create/switch to the release/<major>.<minor>.x maintenance line off the
# latest matching tag, ready for cherry-picking fix: commits to backport.
# Usage: just backport-line 1.3   then  git cherry-pick <sha>...  then
#        just release-prep-backport 1.3.2

# Create or switch to the release/X.Y.x maintenance line, ready for cherry-picks.
backport-line minor:
    #!/usr/bin/env bash
    set -euo pipefail
    git fetch origin --tags --quiet
    base="$(git tag --list 'v{{minor}}.*' --sort=-v:refname | head -1)"
    test -n "$base" || { echo "no v{{minor}}.* tag to branch from"; exit 1; }
    git switch "release/{{minor}}.x" 2>/dev/null || git switch -c "release/{{minor}}.x" "$base"
    echo "On release/{{minor}}.x (off $base). Cherry-pick the fix commits, then: just release-prep-backport <{{minor}}.Z>"

# Stage a backport release on the current release/X.Y.x branch (after the
# fix: commits are cherry-picked) and push the branch. Pushing IS the trigger:
# release.yml runs on `release/*.x` pushes, gates on the staged version bumps,
# and creates the tag itself. No PR, and no manual tag.
# Usage: just release-prep-backport 1.3.2

# Stage a backport release on the current release/X.Y.x branch and push it. The push publishes.
release-prep-backport version chart_bump="patch":
    #!/usr/bin/env bash
    set -euo pipefail
    case "$(git rev-parse --abbrev-ref HEAD)" in release/*.x) ;; *) echo "not on a release/X.Y.x maintenance branch"; exit 1;; esac
    VERSION="{{version}}" CHART_BUMP="{{chart_bump}}" PR_BODY_FILE=release-pr-body.md \
        node .github/scripts/prepare-release.mjs
    chart="$(awk '/^version:/{print $2; exit}' deploy/helm/cerberus/Chart.yaml)"
    docker run --rm -v "$PWD/deploy/helm:/helm-docs" -u "$(id -u)" \
        jnorwood/helm-docs:v1.14.2 --chart-search-root=/helm-docs --template-files=README.md.gotmpl
    git add deploy/helm/cerberus/Chart.yaml deploy/helm/cerberus/README.md CHANGELOG.md
    git commit -m "chore(release): cerberus v{{version}} / chart ${chart}"
    git push -u origin "$(git rev-parse --abbrev-ref HEAD)"
    echo "Pushed v{{version}} to $(git rev-parse --abbrev-ref HEAD) — that push IS the trigger; release.yml builds, publishes, and tags. Do not tag by hand."

# There is deliberately no `release-tag` recipe. release.yml publishes on
# merge — it has no raw-tag trigger — and `release-version-gate.mjs` decides
# whether to publish by asking whether `v<appVersion>` already exists.
# So pre-creating the tag by hand does not start a release — it PERMANENTLY
# cancels one: the gate sees the tag, sets publish=false, and goreleaser,
# publish and chart-release all skip. No job fails, and nothing ships.
# `TestNoJustfileRecipePushesAReleaseTag` in
# `test/regression/release_required_checks_test.go` keeps it from coming back:
# it rejects a recipe named `release-tag*` and any recipe line that cuts a tag
# (`git tag`, `git push --tags`, `gh release create`).
