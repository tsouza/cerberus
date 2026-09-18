# `forbid-skip` — canonical pattern reference

The `forbid-skip` CI gate (and its `lefthook.yml` pre-push mirror) is the
machine-enforced arm of cerberus's GA test-discipline rule: **no `t.Skip`,
no soft assertions, no silent panic recovery, no untracked `should_skip`
overlay entries, no test escape-hatch primitives.**

The gate enforces test-skipping *behaviour*, never vocabulary: no scan
bans the words "not implemented" / "skipped" / "deferred" in a test file
or fixture. This document is the canonical reference for the full pattern
set; a widening starts from it.

## Where the patterns live

The gate **runs** in two places:

1. `.github/workflows/ci.yml` job `forbid-skip` — required status check
   on `main`. The job holds no regexes of its own: every discipline scan
   is a `run: node .github/scripts/forbid-skip.mjs` step with a `CHECK:`
   naming the arm to dispatch. The job also carries the CLI self-test
   (`node --test .github/scripts/forbid-skip.test.mjs`) and two unrelated
   assert-from-source gates that ride the same lane —
   `clickhouse-version-sync.mjs` (`--self-test` + gate) and
   `doc-counts.mjs` (`--self-test` + gate).
2. `lefthook.yml` `pre-push` hook — one `forbid-skip-<arm>` command per
   registry arm, each `run: node .github/scripts/forbid-skip.mjs` with
   `env: CHECK: <arm>`, so a push that would fail CI fails locally first.
   `test/regression/lefthook_forbid_skip_mirror_test.go` pins that every
   arm is mirrored, that every `CHECK:` names a live arm, and that no hook
   body carries an inline scan.

The regex **text** lives in exactly one file: `.github/scripts/forbid-skip.mjs`,
one `CHECKS` registry entry per scan. Neither runner re-spells a regex.

`.github/scripts/forbid-skip.test.mjs` is the assertion that the regexes
still match their canonical positive examples and still reject the matching
counter-examples below. It drives the real CLI, arm by arm, against a
throwaway repository seeded with each fixture, so a regex mutated to match
nothing turns the `forbid-skip` job red. Every row below has such a pair:

| Row(s) | CI step (`CHECK=`)                                                            | lefthook `pre-push` command      |
| ------ | ----------------------------------------------------------------------------- | -------------------------------- |
| 1      | Reject t.Skip in test files (`t-skip`)                                        | `forbid-skip-t-skip`             |
| 2–4    | Reject soft-assertion / silent-recover patterns (`soft-assert`)               | `forbid-skip-soft-assert`        |
| 5      | Reject should_skip overlay entries (`should-skip`)                            | `forbid-skip-should-skip`        |
| 6      | Reject test escape-hatch patterns (`escape-hatch`)                            | `forbid-skip-escape-hatch`       |
| 7–8    | Reject scenario-suppressing tags and godog skip routes (`feature-discipline`) | `forbid-skip-feature-discipline` |
| 9      | Reject Playwright spec suppression (`playwright-skip`)                        | `forbid-skip-playwright`         |

A runner of a regex is not an assertion about it: CI and lefthook cannot fail
on a clean tree, so they prove nothing about whether a scan still
discriminates. Only the fixture pairs in `forbid-skip.test.mjs` do (#3182).
The file's last test reads the live `CHECKS` key set from the CLI's own
unknown-`CHECK` error and fails when any arm has not been driven to both exits
in that run, so an arm cannot be added to the registry without its pair.

## Patterns vs CHECK categories — the count that the gate pins

The summary table below is organised by **regex pattern** — one row per
distinct regex shape, so that each shape has its own match-example and
counter-example. The CI gate, however, dispatches by **CHECK category**:
`.github/scripts/doc-counts.mjs` derives the canonical scan count LIVE
from the keys of the `CHECKS` registry in
`.github/scripts/forbid-skip.mjs`, and that count is **6** CHECK categories:

| CHECK category       | Covers regex pattern row(s) |
| -------------------- | --------------------------- |
| `t-skip`             | 1                           |
| `soft-assert`        | 2, 3, 4                     |
| `should-skip`        | 5                           |
| `escape-hatch`       | 6                           |
| `feature-discipline` | 7, 8                        |

The `soft-assert` scan runs three regex shapes (the two soft-assertion
forms plus the silent-recover slurp) inside one CHECK, and
`feature-discipline` runs two (the `.feature` tag scan plus the
harness-Go skip scan), which is why the nine pattern rows collapse to
**6** dispatched scans. The `doc-counts.mjs` gate asserts every "N
checks/scans" claim in this document equals the live `CHECKS`
registry size (6), so the number can never drift from the source registry.

## Adding a new pattern

When a new offender shape is discovered:

1. Add the new regex to `.github/scripts/forbid-skip.mjs` — widen an
   existing `CHECKS` registry entry, or add a new entry plus a matching
   `run: node .github/scripts/forbid-skip.mjs` step with `CHECK: <name>`
   in the `forbid-skip` job of `.github/workflows/ci.yml`. That step is the
   gate. Mirror it in `lefthook.yml` so a push fails locally wherever CI
   would; every arm has a mirror.
2. Add a new row to the summary table below + a detailed subsection
   covering the regex, its intent, a match-example, and a
   counter-example.
3. Add a test that proves the new scan can go RED, covering both
   directions — a match fixture and a no-match fixture driven through the
   real CLI in `.github/scripts/forbid-skip.test.mjs`. A scan without that
   pair is not pinned by being run in CI and lefthook: those run it, they
   do not assert anything about it.
4. Record the originating PR number in the summary table's `Origin`
   column — the pattern headings name the shape they reject, not the
   change that added them.

Never weaken an existing regex without a recorded rationale — every
widening traces back to a real offender that escaped a narrower shape.

## Summary

| #   | Intent                                                                                                      | Scope                                                           | Origin        |
| --- | ----------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------- | ------------- |
| 1   | Reject `t.Skip[fN]?` calls                                                                                  | `*_test.go`                                                     | #309          |
| 2   | Reject `assert.Contains(x, "")` soft assertion (2-arg + testify 3-arg)                                      | `*_test.go`                                                     | #587 / #277   |
| 3   | Reject `assert.ElementsMatch(x, []T{})` soft assertion (2-arg + testify 3-arg)                              | `*_test.go`                                                     | #587 / #277   |
| 4   | Reject silent panic recovery (`defer recover()` and the multi-line `defer func(){ _ = recover() }()` block) | `*_test.go`                                                     | #587 / #648   |
| 5   | Reject any non-empty `should_skip:` block                                                                   | `compatibility/**/*.{yml,yaml}`                                 | #596          |
| 6   | Reject test escape-hatch primitives (allow-list / tolerance / soft-assert)                                  | `*.{ts,tsx,go}` (non-upstream, non-vendor)                      | #712          |
<!-- #1538: @todo below is a Gherkin tag name documented here, not a work marker -->
| 7 | Reject scenario-suppressing Gherkin tags (`@wip` / `@skip` / `@ignore` / `@manual` / `@todo` / `@pending`)                   | `*.feature`                                   | #1268 |
| 8 | Reject godog skip / pending routes (`godog.ErrSkip`, `godog.ErrPending`, `.Skip` / `.Skipf` / `.SkipNow`)                    | `test/e2e/migration/**/*.go`                  | #1268 |
| 9 | Reject Playwright spec suppression (`test.skip` / `test.fixme` / `test.only`, and the `it` / `describe` / `suite` spellings) | `*.spec.ts`, `*.spec.js` (non-`node_modules`) | #3180 |

Row 5 rejects any non-empty `should_skip:` block outright (see
`.github/workflows/ci.yml` `forbid-skip` job step "Reject should_skip
overlay entries"). The only accepted form is `should_skip: []`; any
element under the key fails CI.

Vendored upstream snapshots under `compatibility/*/upstream/**` are
excluded from every test-file / fixture grep — they sit outside
cerberus's authorship boundary.

## Pattern 1 — `t.Skip[fN]?` calls

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

## Pattern 2 — `assert.Contains(x, "")` soft assertion (2-arg + testify 3-arg)

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

## Pattern 3 — `assert.ElementsMatch(x, []T{})` soft assertion (2-arg + testify 3-arg)

Regex: `assert\.ElementsMatch\(([^,]+,\s*){0,1}[^,]+,\s*\[\][^)]*\{\s*\}\s*\)`

Sibling of pattern 2. Same regex shape (`[^,]+,` clamp + empty-needle
match) targeting `ElementsMatch` against an empty slice literal. The
optional `([^,]+,\s*){0,1}` prefix matches the testify-style
`t *testing.T` first arg when present, so the regex catches BOTH the
2-arg gocheck-style and the 3-arg testify forms.

- Matches (2-arg gocheck): `assert.ElementsMatch(got, []string{})`
- Matches (3-arg testify): `assert.ElementsMatch(t, got, []string{})`
- Does NOT match: `assert.ElementsMatch(got, []string{"a", "b"})`
- Does NOT match: `assert.ElementsMatch(t, got, []string{"a", "b"})`

## Pattern 4 — silent panic recovery (`defer recover()` and multi-line variants)

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

## Pattern 5 — `should_skip:` compatibility overlay

The `forbid-skip` CI job step "Reject should_skip overlay entries"
rejects ANY non-empty `should_skip:` block in
`compatibility/**/*.{yml,yaml}` outright. A compatibility corpus entry
is either scored against the reference or it is not in the corpus —
there is no per-case skip overlay, so the gate forbids the construct
itself rather than checking each entry's tracking ref.

## Pattern 6 — test escape-hatch primitives (allow-list / tolerance / soft-assert)

Regex (ERE alternation over `*.ts` / `*.tsx` / `*.go`, excluding
`compatibility/*/upstream/**`, `**/node_modules/**`, `vendor/**`,
`.claude/**`):

```text
EXPECTED_EMPTY|EXPECTED_TOLERATED|isKnownTolerated|tolerated404|
expect\.soft|should_tolerate|skipReason|SkipReason|
APP_NOT_INSTALLED_BANNER_PATTERNS|DRILLDOWN_UPSTREAM_GRAFANA_CONSOLE_NOISE
```

Where patterns 1–5 forbid Go-test skip / soft-assert constructs and the
compatibility-overlay skip, pattern 6 forbids the broader family of
*test-suite escape-hatch primitives* — any allow-list array, tolerance
constant, or soft-assertion the e2e / Playwright / Go suites might reach
for to mask a real failure instead of fixing it at the source. It runs as
the CI `forbid-skip` job step "Reject test escape-hatch patterns"
(`CHECK=escape-hatch`).

Each token names an anti-pattern the tree does not carry:

- `EXPECTED_EMPTY` / `EXPECTED_TOLERATED` / `isKnownTolerated` /
  `tolerated404` — allow-list arrays consulted before a failing
  assertion to swallow it.
- `expect.soft(...)` — Playwright soft assertion that records a failure
  but lets the test continue, easy to miss in CI summaries.
- `should_tolerate` / `skipReason` / `SkipReason` — overlay-driven
  tolerate / skip fields.
- `APP_NOT_INSTALLED_BANNER_PATTERNS` /
  `DRILLDOWN_UPSTREAM_GRAFANA_CONSOLE_NOISE` — named noise allow-lists
  that suppress specific crawler signals.

- Matches: `const EXPECTED_TOLERATED = [/* ... */];` or
  `expect.soft(locator).toBeVisible();`
- Does NOT match: `expect(locator).toBeVisible();` (the loud form)

## Pattern 7 — scenario-suppressing Gherkin tags (`@wip` / `@skip` / `@ignore` / `@manual` / `@todo` / `@pending`)

Regex (ERE, case-insensitive, over `*.feature`, excluding
`**/node_modules/**`):

```text
(^|[ \t])@(wip|skip|ignore|manual|todo|pending)([ \t]|$)
```

The scan is case-insensitive (`grep -i`): the only tag forms a `.feature`
file may carry are the coverage ratchet's `@MIG-nn`, `@tier0`..`@tier2`
and `@archetype:<name>`, all fixed-case, so no legitimate tag needs a
mixed-case spelling. The tag vocabulary is bounded on the other side too:
the coverage ratchet in `.github/scripts/migration-e2e.mjs` rejects any
tag outside those three forms, so a novel suppression tag fails there
even before this scan names it. The other Cucumber suppression route — an
unimplemented step reported as *pending* rather than failed — is closed
at runtime by `godog.Options.Strict`.

- Matches: `@MIG-04 @tier0 @wip`
- Matches: `@skip`
- Does NOT match: `@MIG-01 @tier0 @archetype:already-otel`
- Does NOT match: an `@archetype:` value that merely contains one of the
  words, e.g. `@archetype:manual-scrape`

## Pattern 8 — godog skip / pending routes (`godog.ErrSkip` / `godog.ErrPending` / `.Skip*`)

Regex (ERE over `test/e2e/migration/**/*.go`):

```text
godog\.(ErrSkip|ErrPending)|\.Skip(f|Now)?\(
```

godog step definitions live in ordinary non-test `.go` files, outside
pattern 1's `*_test.go` scope. A step returning `godog.ErrSkip` skips the
rest of its scenario, one returning `godog.ErrPending` reports the
Cucumber "pending" status, and `godog.T(ctx)` hands a step a `TestingT`
whose `Skip`, `Skipf` and `SkipNow` mark the scenario skipped. The
receiver is unanchored (`\.Skip(f|Now)?\(` rather than
`godog\.T\(…\)\.Skip`) so binding the `TestingT` to a local variable
first does not evade the scan; nothing in the harness has a legitimate
reason to call a method named `Skip`.

- Matches: `return godog.ErrSkip`
- Matches: `godog.T(ctx).Skipf("no fixture for %s", archetype)`
- Matches: `t := godog.T(ctx); t.SkipNow()`
- Does NOT match: `w.Skipped = corpus.Skipped` (a field, not a call)

## Pattern 9 — Playwright spec suppression (`test.skip` / `test.fixme` / `test.only`)

Regex (ERE over `*.spec.ts` / `*.spec.js`, excluding `**/node_modules/**`):
`(^|[^A-Za-z0-9_$.])(test|it|describe|suite)(\.describe)?\.(skip|fixme|only)\s*\(`

Pattern 1's scope is `*_test.go`, so Playwright's own suppression routes
are invisible to it: they live in `.spec.ts`. `test.skip` and
`test.fixme` silence a spec while the lane still reports green, and a
conditional `test.skip(!process.env.X, …)` is the same move wearing an
environment check — the missing environment must be a hard failure and
the spec wired into a lane, not a reason to stand the spec down.
`test.only` is the mirror image: it silences every OTHER spec in the
file, so a lane can report green having run one test. All three are
`t.Skip` in TypeScript.

The leading `[^A-Za-z0-9_$.]` guard anchors the call to a real
`test` / `it` / `describe` / `suite` receiver rather than a longer
identifier that merely ends in one, and the optional `(\.describe)?`
covers Playwright's `test.describe.skip(…)` group form.

- Matches: `test.skip('renders the panel', async ({ page }) => {…});`
- Matches (conditional): `test.skip(!process.env.CERBERUS_URL, 'no gateway');`
- Matches (group): `test.describe.skip('dashboard sweep', () => {…});`
- Does NOT match: `test('renders the panel', async ({ page }) => {…});`
  (the loud form)
- Does NOT match: `testSkipHelper(page)` (an identifier, not a
  `test.`-receiver call)

## Scan count

The gate dispatches **6** CHECK scans (`t-skip`, `playwright-skip`,
`soft-assert`, `should-skip`, `escape-hatch`, `feature-discipline`), which
together run the nine regex pattern rows above (the `soft-assert` scan carries rows
2, 3 and 4 and `feature-discipline` carries rows 7 and 8; see the
"Patterns vs CHECK categories" mapping). Pattern 1 runs over Go test
files; patterns 2–4 over Go test files for soft-assertion / silent-recover
shapes; pattern 5 is the strict overlay-entry rejection over the
compatibility YAML; pattern 6 is the escape-hatch scan over the TS / Go
suites; patterns 7 and 8 are the Gherkin-scenario discipline over the
migration harness; pattern 9 is the Playwright-spec discipline over the
`.spec.ts` suites. The canonical scan count is derived live from
`.github/scripts/forbid-skip.mjs` by `.github/scripts/doc-counts.mjs`, so
this **6** scan count can never drift from the source registry.

---

For the rationale behind these choices — alternatives considered, incidents,
measurements — see [forbid-skip.background.md](forbid-skip.background.md).
