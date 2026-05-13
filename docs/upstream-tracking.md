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
unexported** with no public accessors. Cerberus used to read them
via reflection or `unsafe.Pointer` shims; the canonical example was
`internal/traceql/aggregate.go`'s `readAggregateExpr` (added for
P0 #7 in RC2). That shim has since been retired against the
`github.com/tsouza/tempo` fork (`cerberus-accessors` branch); see
[`docs/fork-tempo-plan.md`](fork-tempo-plan.md). A `forbidigo` rule
in `.golangci.yml` prevents either pattern from being reintroduced
under `internal/traceql/` or `internal/api/tempo/`.

The shim works but is **fragile** in three ways:

1. **Field renames go silent**: `agg.e` could become `agg.expr` in a
   future Tempo release and our `reflect.FieldByName("e")` returns
   zero-value with no compile error.
2. **Type changes corrupt**: the `unsafe.Pointer` cast assumes the
   field's underlying type is exactly `traceql.FieldExpression`. A
   type change there corrupts the read with no warning.
3. **Field reordering breaks `UnsafeAddr` math**: less likely than
   rename but possible — and equally silent.

We catch most of these as **fixture-suite failures** when bumping
parser deps, but only because we have decent fixture coverage. As the
unsafe surface grows (P0 #7 today; likely more for LogQL parser stages,
`predict_linear` upstream-internal state, etc.), the failure cone
grows.

### Strategy

The long-term fix is to **fork each upstream parser** under
`github.com/tsouza/` and add the narrow set of accessors cerberus
needs. Steps:

1. **Fork** `prometheus/prometheus`, `grafana/loki`, `grafana/tempo`
   under `tsouza`. Use the standard GitHub fork; tag a `-cerberus.N`
   suffix on cerberus-specific releases (`v1.5.1-cerberus.1`, etc.).
2. **Add accessors** as a thin patch series under `patches/` on the
   fork:
   - For Tempo `Aggregate`: `func (a Aggregate) Op() AggregateOp` and
     `func (a Aggregate) Expr() FieldExpression`.
   - For Loki / Prom: whichever unexported fields cerberus currently
     touches via `unsafe`.
3. **`replace` directives** in cerberus's `go.mod` point at the forks.
4. **Replace the `unsafe` shims** with the new accessors. Delete
   `readAggregateExpr` and friends; the `reflect`-only paths can stay
   for fields where the accessor doesn't exist yet.

### Automated upstream tracking on the fork

The forks can't be one-shot — upstream releases continuously and
cerberus needs to absorb fixes. A workable automation pattern:

- **Track upstream tags via a GitHub Action** scheduled hourly on the
  fork: when a new tag appears upstream, the workflow creates a branch
  on our fork, re-applies the patch series via `git am < patches/*`,
  opens a PR on the fork, and optionally runs the upstream test suite
  to verify the patches still apply.
- **Tag releases** as `<upstream-tag>-cerberus.<n>` so cerberus's
  go.mod can pin a specific patch-set version.
- **Dependabot in cerberus** picks up the new fork tag like any other
  Go dep; the grouped daily bump catches it.
- **Conflicts** surface as failed `git am` steps — the auto-PR halts
  and the maintainer reviews. Cerberus's TXTAR + Playwright suites
  guard correctness end-to-end so any silent semantic drift surfaces
  before merge.

### What goes in each fork's patch series

Minimal set as of RC2:

- **Tempo** — `Aggregate.Op()` accessor, `Aggregate.Expr()` accessor.
  Possibly `BinaryOperation` field exposure if we discover more
  unexported state. Possibly `SubqueryExpr` analogue if Tempo
  introduces one and keeps fields private.
- **Prometheus** — none yet; the parser exports what we need today.
  The `Call` / `MatrixSelector` private fields could surface needs
  later.
- **Loki** — none yet; the parser-stage rejection path doesn't need
  unexported access. May change as we lower more LogQL stages.

### When to start

Not blocking RC2. The current unsafe shims are limited in number (one
as of P0 #7) and the fixture suite catches drift. The fork strategy
lands when:

- The cost of one of: a CI red after a parser-dep bump, OR adding a
  third unsafe shim, exceeds the cost of standing up the fork +
  automation (estimated one-day setup, ongoing maintenance ~zero per
  release as long as upstream is stable).
- OR cerberus's compatibility goal (M6 `prometheus/compliance` gate)
  needs parser-internal state that's currently unreachable without
  forking.

Tentative target: **RC4**, alongside the self-observability work — by
then we'll know which additional accessors we need.

---

## References

- `.github/dependabot.yml` — the daily-grouped config described above.
- `.github/workflows/auto-merge-deps.yml` — auto-merge on green CI for
  trusted patch-only bumps.
- `internal/traceql/aggregate.go` — historically housed
  `readAggregateExpr` (the `unsafe.Pointer` shim this strategy replaced);
  now uses fork accessors from `tsouza/tempo:cerberus-accessors`.
- `.golangci.yml` — the `forbidigo` rule blocking `unsafe.Pointer` /
  `reflect.Value.FieldByName` from being reintroduced.
- `docs/roadmap.md` — RC2 backlog references this doc.
