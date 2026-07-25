# `forbid-skip` — canonical pattern reference

The `forbid-skip` CI gate (and its `lefthook.yml` pre-push mirror) is the
machine-enforced arm of cerberus's GA test-discipline rule: **no `t.Skip`,
no "not implemented" in production code, no soft assertions, no silent
panic recovery, no untracked `should_skip` overlay entries, no test
escape-hatch primitives.**

The gate enforces test-skipping *behaviour*. An earlier `wording-tests`
scan that banned the *words* "not implemented" / "skipped" / "deferred"
in test files + TXTAR fixtures was removed: it policed vocabulary rather
than behaviour, false-positived on honest descriptions of correct,
version-gated tests (e.g. a CH function gated above the chDB floor), and
caught nothing the behavioural scans miss.

The gate grew iteratively — several PRs widened it as new offenders
escaped prior regex shapes. This document is the canonical, frozen
reference for the full pattern set so future widenings start from a
known baseline instead of re-deriving from CI failure tickers.

## Where the patterns live

The patterns run in **two places**, and the two MUST stay in sync:

1. `.github/workflows/ci.yml` job `forbid-skip` — required status check
   on `main`.
2. `lefthook.yml` `pre-push` hook — local-mirror of the same gate so a
   push that would have failed CI fails locally first.

`scripts/test-forbid-skip.sh` is the assertion that the regexes still
match their canonical positive examples and still reject the matching
counter-examples below. It runs both as a standalone unit-test (invoke
the script directly) and as a step inside the `forbid-skip` CI job. The
lefthook `forbid-skip-self-test` command runs the same script on
pre-push.

The two locations carry **identical** patterns for rows 1–5 of the
summary table below. Row 6 is enforced by the CI `forbid-skip` job step
"Reject should_skip overlay entries", which rejects every non-empty
`should_skip:` block in `compatibility/**/*.{yml,yaml}` outright. Row 7
is enforced by the CI step "Reject test escape-hatch patterns". Rows 8
and 9 are enforced by the CI step "Reject scenario-suppressing tags and
godog skip routes".

## Patterns vs CHECK categories — the count that the gate pins

The summary table below is organised by **regex pattern** — one row per
distinct regex shape, so that each shape has its own match-example and
counter-example. The CI gate, however, dispatches by **CHECK category**:
`.github/scripts/doc-counts.mjs` derives the canonical scan count LIVE
from the `case '<name>':` arms of the `CHECK` switch in
`.github/scripts/forbid-skip.mjs`, and that count is **6**:

| CHECK category       | Covers regex pattern row(s) |
| -------------------- | --------------------------- |
| `t-skip`             | 1                           |
| `not-implemented`    | 2                           |
| `soft-assert`        | 3, 4, 5                     |
| `should-skip`        | 6                           |
| `escape-hatch`       | 7                           |
| `feature-discipline` | 8, 9                        |

The `soft-assert` scan runs three regex shapes (the two soft-assertion
forms plus the silent-recover slurp) inside one CHECK, and
`feature-discipline` runs two (the `.feature` tag scan plus the
harness-Go skip scan), which is why the **9** pattern rows collapse to
**6** dispatched scans. The `doc-counts.mjs` gate asserts every "N
patterns/checks/scans" claim in this document equals the live CHECK-arm
count (6), so the number can never drift from the source switch.

## Adding a new pattern

When a new offender shape is discovered:

1. Add the new regex to **both** `.github/workflows/ci.yml` and
   `lefthook.yml` in the same PR.
2. Add a new row to the summary table below + a detailed subsection
   covering the regex, its intent, a match-example, and a
   counter-example.
3. Add a matching `case_N_*` block to `scripts/test-forbid-skip.sh`
   covering both directions.
4. Note the originating PR number.

Never weaken an existing regex without a recorded rationale — every
widening so far traces back to a real offender that escaped a narrower
shape.

## Summary

| #   | Intent                                                                                                      | Scope                                                           | Origin        |
| --- | ----------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------- | ------------- |
| 1   | Reject `t.Skip[fN]?` calls                                                                                  | `*_test.go`                                                     | #309          |
| 2   | Reject "not implemented" wording in production code                                                         | `internal/**/*.go` (non-test)                                   | #197          |
| 3   | Reject `assert.Contains(x, "")` soft assertion (2-arg + testify 3-arg)                                      | `*_test.go`                                                     | #587 / #277   |
| 4   | Reject `assert.ElementsMatch(x, []T{})` soft assertion (2-arg + testify 3-arg)                              | `*_test.go`                                                     | #587 / #277   |
| 5   | Reject silent panic recovery (`defer recover()` and the multi-line `defer func(){ _ = recover() }()` block) | `*_test.go`                                                     | #587 / #648   |
| 6   | Reject any non-empty `should_skip:` block                                                                   | `compatibility/**/*.{yml,yaml}`                                 | #596          |
| 7   | Reject test escape-hatch primitives (allow-list / tolerance / soft-assert)                                  | `*.{ts,tsx,go}` (non-upstream, non-vendor)                      | #712          |
| 8   | Reject scenario-suppressing Gherkin tags (`@wip` / `@skip` / `@ignore` / `@manual` / `@todo` / `@pending`)  | `*.feature`                                                     | #1268         |
| 9   | Reject godog skip / pending routes (`godog.ErrSkip`, `godog.ErrPending`, `.Skip` / `.Skipf` / `.SkipNow`)   | `test/e2e/migration/**/*.go`                                    | #1268         |

Row 6 rejects any non-empty `should_skip:` block outright (see
`.github/workflows/ci.yml` `forbid-skip` job step "Reject should_skip
overlay entries"). The only accepted form is `should_skip: []`; any
element under the key fails CI.

Vendored upstream snapshots under `compatibility/*/upstream/**` are
excluded from every test-file / fixture grep — they sit outside
cerberus's authorship boundary.

## Pattern 1 — `t.Skip[fN]?` (PR #309)

Regex: `t\.Skip[fN]?\(`

`t.Skip` calls are a way to mark known-broken tests as "we'll fix it
later" — and the fix tends never to land, so the skip lingers. The
discipline rule is "fix the bug or delete the test, no skipping"; this
regex is the enforcement.

`[fN]?` covers all three call shapes (`t.Skip`, `t.Skipf`, `t.SkipNow`)
in one alternation. The literal `(` anchors against accidental matches
on identifiers.

- Matches: `func TestFoo(t *testing.T) { t.Skip("upstream bug") }`
- Does NOT match: `func TestFoo(t *testing.T) { tx.Skipper() }` (different
  receiver / method name)

## Pattern 2 — "not implemented" in production code (PR #197)

Regex: `not implemented`

`internal/**.go` carries no `not implemented` wording, and this gate
keeps it that way. Bare `skipped` / `deferred` are NOT in the
production gate — `defer` statements described in docstrings would
false-positive, and runtime prose like "request X is rejected" is the
correct rewrite of "skipped" rather than something to forbid.

- Matches: `return errors.New("not implemented")`
- Does NOT match: `return errUnsupported // rejects unsupported foo`

## Pattern 3 — `assert.Contains(x, "")` soft assertion (PR #587, widened #277)

Regex: `assert\.Contains\(([^,]+,\s*){0,1}[^,]+,\s*""\s*\)`

An `assert.Contains(body, "")` call passes for any input — it looks
like a real assertion in review but verifies nothing. This gate
rejects it.

The regex uses `[^,]+` to clamp the haystack-argument match inside a
single function call (so it doesn't reach into adjacent calls), and
`\s*""\s*` to allow optional whitespace around the empty needle. The
leading `([^,]+,\s*){0,1}` is an optional prefix that consumes a
testify-style first argument (`t *testing.T`) when present, so the
single regex catches BOTH the 2-arg gocheck-style call
(`assert.Contains(haystack, "")`) and the 3-arg testify call
(`assert.Contains(t, haystack, "")`).

- Matches (2-arg gocheck): `assert.Contains(body, "")`
- Matches (3-arg testify): `assert.Contains(t, body, "")`
- Does NOT match: `assert.Contains(body, "error: foo")`
- Does NOT match: `assert.Contains(t, body, "error: foo")`

## Pattern 4 — `assert.ElementsMatch(x, []T{})` soft assertion (PR #587, widened #277)

Regex: `assert\.ElementsMatch\(([^,]+,\s*){0,1}[^,]+,\s*\[\][^)]*\{\s*\}\s*\)`

Sibling of pattern 3. Same regex shape (`[^,]+,` clamp + empty-needle
match) targeting `ElementsMatch` against an empty slice literal. The
optional `([^,]+,\s*){0,1}` prefix matches the testify-style
`t *testing.T` first arg when present, so the regex catches BOTH the
2-arg gocheck-style and the 3-arg testify forms.

- Matches (2-arg gocheck): `assert.ElementsMatch(got, []string{})`
- Matches (3-arg testify): `assert.ElementsMatch(t, got, []string{})`
- Does NOT match: `assert.ElementsMatch(got, []string{"a", "b"})`
- Does NOT match: `assert.ElementsMatch(t, got, []string{"a", "b"})`

## Pattern 5 — silent panic recovery (PR #587 / #648)

Regex (perl-slurp form):
`defer\s+recover\s*\(\s*\)` \| `defer\s+func\s*\(\s*\)\s*\{[^{}]*_\s*=\s*recover\s*\(\s*\)`

The gate covers the single-line `defer recover()`, the same-line
`defer func() { _ = recover() }()`, and the multi-line variant
`defer func() {\n  _ = recover()\n}()` — the last caught via a
`perl -0777` slurp so a newline between the brace and the `recover()`
doesn't dodge it.

The `[^{}]*` clamp keeps the multi-line match inside a single brace
level so it doesn't over-reach. The `_\s*=\s*recover` discriminator
distinguishes a silent swallow from the legitimate
`r := recover(); if r == nil { t.Fatal(...) }` asserted-panic form
used in real tests.

- Matches (bare): `defer recover()`
- Matches (multi-line):

  ```go
  defer func() {
    _ = recover()
  }()
  ```

- Does NOT match (asserted-panic form):

  ```go
  defer func() {
    r := recover()
    if r == nil { t.Fatal("expected panic") }
  }()
  ```

## Pattern 6 — `should_skip:` compatibility overlay

The `forbid-skip` CI job step "Reject should_skip overlay entries"
rejects ANY non-empty `should_skip:` block in
`compatibility/**/*.{yml,yaml}` outright. A compatibility corpus entry
is either scored against the reference or it is not in the corpus —
there is no per-case skip overlay, so the gate forbids the construct
itself rather than auditing each entry's tracking ref.

## Pattern 7 — test escape-hatch primitives (PR #712)

Regex (ERE alternation over `*.ts` / `*.tsx` / `*.go`, excluding
`compatibility/*/upstream/**`, `**/node_modules/**`, `vendor/**`,
`.claude/**`):

```text
EXPECTED_EMPTY|EXPECTED_TOLERATED|isKnownTolerated|tolerated404|
expect\.soft|should_tolerate|skipReason|SkipReason|
APP_NOT_INSTALLED_BANNER_PATTERNS|DRILLDOWN_UPSTREAM_GRAFANA_CONSOLE_NOISE
```

Where patterns 1–6 forbid Go-test skip / soft-assert constructs and the
compatibility-overlay skip, pattern 7 forbids the broader family of
*test-suite escape-hatch primitives* — any allow-list array, tolerance
constant, or soft-assertion the e2e / Playwright / Go suites might reach
for to mask a real failure instead of fixing it at the source. PR #712
deleted every such mechanism from the tree; this scan keeps them from
creeping back. It runs as the CI `forbid-skip` job step "Reject test
escape-hatch patterns" (`CHECK=escape-hatch`).

Each token names a removed anti-pattern:

- `EXPECTED_EMPTY` / `EXPECTED_TOLERATED` / `isKnownTolerated` /
  `tolerated404` — allow-list arrays consulted before a failing
  assertion to swallow it.
- `expect.soft(...)` — Playwright soft assertion that records a failure
  but lets the test continue, easy to miss in CI summaries.
- `should_tolerate` / `skipReason` / `SkipReason` — overlay-driven
  tolerate / skip fields whose consumer code was removed.
- `APP_NOT_INSTALLED_BANNER_PATTERNS` /
  `DRILLDOWN_UPSTREAM_GRAFANA_CONSOLE_NOISE` — named noise allow-lists
  that previously suppressed specific crawler signals.

- Matches: `const EXPECTED_TOLERATED = [/* ... */];` or
  `expect.soft(locator).toBeVisible();`
- Does NOT match: `expect(locator).toBeVisible();` (the loud form)

## Pattern 8 — scenario-suppressing Gherkin tags (PR #1268)

Regex (ERE, case-insensitive, over `*.feature`, excluding
`**/node_modules/**`):

```text
(^|[ \t])@(wip|skip|ignore|manual|todo|pending)([ \t]|$)
```

Case-insensitive (`grep -i`) is deliberate here: this Gherkin tag
vocabulary is closed by construction — the coverage ratchet's own three
recognised forms (`@MIG-nn`, `@tier0`..`@tier2`, `@archetype:<name>`)
are all fixed-case, so nothing legitimate in a `.feature` file ever
needs a mixed-case `@wip`/`@WIP`/`@Skip` spelling. Scanning
case-sensitively would let a wrongly-cased suppression tag merge clean.

The Layer-14 migration lane's scenarios are Gherkin feature files driven
by `godog`. A Cucumber runner's culture carries two suppression routes
that Go's `testing` package does not: a `@wip`-style tag that filters a
`Scenario` out of the run, and an unimplemented step reported as
*pending* rather than failed. The runner closes the second one at
runtime (`godog.Options.Strict` fails a suite on an undefined, pending
or ambiguous step); this pattern closes the first one lexically, on the
required `forbid-skip` PR gate, so a suppressed scenario cannot merge and
wait for the next scheduled lane to notice.

The tag vocabulary is bounded on the other side too: the coverage
ratchet in `.github/scripts/migration-e2e.mjs` rejects any tag that is
not `@MIG-nn`, `@tier0`..`@tier2` or `@archetype:<name>`, so a novel
suppression tag fails there even before this scan names it.

- Matches: `@MIG-04 @tier0 @wip`
- Matches: `@skip`
- Does NOT match: `@MIG-01 @tier0 @archetype:already-otel`
- Does NOT match: an `@archetype:` value that merely contains one of the
  words, e.g. `@archetype:manual-scrape`

## Pattern 9 — godog skip / pending routes (PR #1268)

Regex (ERE over `test/e2e/migration/**/*.go`):

```text
godog\.(ErrSkip|ErrPending)|\.Skip(f|Now)?\(
```

Pattern 1's scope is `*_test.go`, but godog step definitions live in ordinary
non-test `.go` files — the step library is a package the runner imports,
not a test package. A step returning `godog.ErrSkip` skips the rest of
its scenario, and one returning `godog.ErrPending` reports the Cucumber
"pending" status; `godog.T(ctx)` hands a step a `TestingT` whose `Skip`,
`Skipf` and `SkipNow` mark the scenario skipped. Every one of those is
`t.Skip` reached by a different door, and pattern 1's scope cannot see
any of them.

The receiver is deliberately unanchored (`\.Skip(f|Now)?\(` rather than
`godog\.T\(…\)\.Skip`) so binding the `TestingT` to a local variable
first does not evade the scan. Nothing in the harness has a legitimate
reason to call a method named `Skip`.

- Matches: `return godog.ErrSkip`
- Matches: `godog.T(ctx).Skipf("no fixture for %s", archetype)`
- Matches: `t := godog.T(ctx); t.SkipNow()`
- Does NOT match: `w.Skipped = corpus.Skipped` (a field, not a call)

## Redundancy review

A read-through of the remaining patterns shows no strict
redundancies — each catches a shape the others would miss:

- Patterns 3 and 4 both target soft-assertion shapes, but they match
  different `assert.*` calls (`Contains` vs `ElementsMatch`). A single
  combined alternation would work but the two-regex shape keeps the
  error message specific.
- Pattern 5 has two alternatives in a single regex (bare `defer
  recover()` vs the multi-line block). They cannot be merged with
  patterns 3 / 4 because pattern 5 needs the `perl -0777` slurp to
  span lines.
- Patterns 1 and 9 both reject a skip call, but neither scope reaches
  the other's files: pattern 1's scope is `*_test.go` across the tree, while
  godog step definitions are non-test `.go` files under the migration
  harness.

The gate dispatches **6** CHECK scans (`t-skip`, `not-implemented`,
`soft-assert`, `should-skip`, `escape-hatch`, `feature-discipline`),
which together run the **9** regex pattern rows above (the `soft-assert`
scan carries rows 3, 4 and 5 and `feature-discipline` carries rows 8 and
9; see the "Patterns vs CHECK categories" mapping). Patterns 1–5 run
over Go test files / production code; pattern 6 is the strict
overlay-entry rejection over the compatibility YAML; pattern 7 is the
escape-hatch scan over the TS / Go suites; patterns 8 and 9 are the
Gherkin-scenario discipline over the migration harness. The canonical
scan count is derived live from `.github/scripts/forbid-skip.mjs` by
`.github/scripts/doc-counts.mjs`, so this **6** can never drift from the
source switch.
