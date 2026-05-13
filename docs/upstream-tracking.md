# Upstream tracking — staying current with parser & infra deps

Cerberus depends on three upstream parsers as Go libraries:

- `github.com/prometheus/prometheus/promql/parser`
- `github.com/grafana/loki/v3/pkg/logql/syntax`
- `github.com/grafana/tempo/pkg/traceql`

Plus the wider Grafana ecosystem (`dskit`, the forked `memberlist`) and
the Playwright / Go toolchain. This doc covers two complementary
strategies:

1. **Auto-bump** — pull new upstream versions as soon as they ship, so
   cerberus stays inside the latest release.
2. **Forks** — when upstream's API isn't enough (unexported fields,
   missing accessors), ship a fork with the narrow patch series
   cerberus needs.

---

## Auto-bump: Dependabot grouping + auto-merge

`.github/dependabot.yml` runs **daily** for Go modules with two groups:

- `upstream-parsers` — Prom + Loki + Tempo + dskit + memberlist
  bumped together. They share state via the forked memberlist
  `replace` (see CLAUDE.md "Transitive-dep gotcha"); bumping one
  without the others tends to fail the build.
- `go-deps` — everything else, also daily, grouped.

Patch + minor bumps land in those grouped PRs. **Major bumps stay
one-per-PR** — they usually need code changes and benefit from
individual review.

GitHub Actions get a weekly bump (slower-moving) under
`github-actions`. Playwright / npm deps under
`test/e2e/playwright` get weekly under `playwright-deps`.

### Auto-merge on green CI

`.github/workflows/auto-merge-deps.yml` watches Dependabot PRs. When
the PR is **patch-only** for an explicitly-trusted set of deps, the
workflow enables auto-merge after CI passes. Trusted today:

- `github.com/prometheus/prometheus` (patch-only)
- `github.com/grafana/loki/v3` (patch-only)
- `github.com/grafana/tempo` (patch-only)
- `github.com/grafana/dskit` (patch-only)
- All `actions/*` GitHub Actions (patch + minor)
- Playwright (`@playwright/test`) (patch-only)

Minor bumps for parsers stay manual — even patch-named upstream
releases sometimes ship semver-violating parser changes.

**Branch protection rules still apply.** The auto-merge marks the PR;
merge happens only after `check + lint + dashboard` all go green.
`enforce_admins: true` prevents bypass.

### Faster than daily?

If Dependabot's once-daily cadence proves too slow (e.g., we need to
absorb a hot-fix from upstream within hours), add a release-watcher
GitHub Action that runs hourly:

- Fetch latest release tag of each parser via the GitHub API.
- If `go.mod`'s pinned version is older, run `go get` + `go mod tidy`
  on a branch and open a PR.

Not implemented today — daily Dependabot handles the realistic cadence
of upstream Prom / Loki / Tempo (typically weekly to monthly minor
releases, hot-fixes within days).

---

## Forks: when upstream's API isn't enough

The parsers ship AST node types where **important fields are
unexported** with no public accessors. Cerberus reads them anyway via
reflection or `unsafe.Pointer` shims — see
`internal/traceql/aggregate.go`'s `readAggregateExpr` for the canonical
example (added for P0 #7 in RC2).

The shim works but is **fragile**: field renames go silent, type
changes corrupt the read, and field reordering breaks `UnsafeAddr`
math — each failure mode silent at compile time and caught (if at all)
by the fixture suite.

### Status — the Tempo fork is live (RC2)

**This section's long-term plan is no longer "tentative RC4" — it
shipped at RC2 and the consumer-side migration is in flight.** The
concrete migration plan, accessor inventory, and per-PR sequence live
in [`docs/fork-tempo-plan.md`](fork-tempo-plan.md); this section is
kept as the cross-QL strategy doc.

What has landed:

- **`tsouza/tempo:cerberus-accessors`** — a fork of `grafana/tempo`
  carrying narrow accessors on `pkg/traceql` (`Aggregate.Op()`,
  `Aggregate.Expr()`, `SelectOperation.Attrs()`, plus the
  MetricsPipeline accessors needed for RC2 metrics lowering). Wired
  into cerberus via `replace github.com/grafana/tempo =>
  github.com/tsouza/tempo …` in `go.mod` (PR #143).
- The shim retirement itself is a follow-up PR sequence — see
  [`docs/fork-tempo-plan.md`](fork-tempo-plan.md) § "Migration
  sequence". As of writing,
  `internal/traceql/aggregate.go`'s `readAggregateExpr`
  (`unsafe.Pointer`) and `internal/traceql/select.go`'s reflect loop
  are still in place and slated for retirement on the next
  fork-consumer PR.

What's *not* forked yet:

- **Prometheus** — the upstream parser exports what we need today.
  The `Call` / `MatrixSelector` private fields could surface needs
  later (e.g. `predict_linear` upstream-internal state). If/when that
  happens, this doc — not `fork-tempo-plan.md` — captures the second
  fork.
- **Loki** — the parser-stage rejection path doesn't need unexported
  access. May change as we lower more LogQL stages.

### Automated upstream tracking on the fork

The forks can't be one-shot — upstream releases continuously and
cerberus needs to absorb fixes. The workable automation pattern (still
to be wired on `tsouza/tempo`):

- **Track upstream tags via a GitHub Action** scheduled hourly on the
  fork: when a new tag appears upstream, the workflow creates a branch
  on our fork, re-applies the patch series via `git am < patches/*`,
  opens a PR on the fork, and optionally runs the upstream test suite
  to verify the patches still apply.
- **Tag releases** as `<upstream-tag>-cerberus.<n>` so cerberus's
  `go.mod` can pin a specific patch-set version.
- **Dependabot in cerberus** picks up the new fork tag like any other
  Go dep; the grouped daily bump catches it.
- **Conflicts** surface as failed `git am` steps — the auto-PR halts
  and the maintainer reviews. Cerberus's TXTAR + Playwright suites
  guard correctness end-to-end so any silent semantic drift surfaces
  before merge.

---

## References

- `.github/dependabot.yml` — the daily-grouped config described above.
- `.github/workflows/auto-merge-deps.yml` — auto-merge on green CI for
  trusted patch-only bumps.
- `internal/traceql/aggregate.go` — `readAggregateExpr` is the
  remaining unsafe shim slated for retirement once the fork accessors
  are wired through (see [`docs/fork-tempo-plan.md`](fork-tempo-plan.md)).
- `internal/traceql/select.go` — `reflect.FieldByName("attrs")` shim,
  same retirement plan.
- [`docs/fork-tempo-plan.md`](fork-tempo-plan.md) — concrete Tempo
  fork plan + per-PR migration sequence.
- `docs/roadmap.md` — RC2 backlog references this doc + fork-tempo-plan.
