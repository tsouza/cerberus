# `forbid-skip` — background

Why the pattern set in [`forbid-skip.md`](forbid-skip.md) has the shape it has: the offenders that
widened each regex, the scan that was removed, and the alternatives rejected. Nothing here changes
what the gate does; it explains why it does it.

## Why the gate scans behaviour and not vocabulary

An earlier `wording-tests` scan banned the *words* "not implemented" / "skipped" / "deferred" in test
files and TXTAR fixtures. It was removed because it policed vocabulary rather than behaviour: it
false-positived on honest descriptions of correct, version-gated tests (a ClickHouse function gated
above the chDB floor, for instance) and caught nothing the behavioural scans miss. A test that skips
is caught by the shape of the skip, not by the word it uses to describe itself.

## Why the pattern set is a frozen reference

The gate grew iteratively — several PRs widened it as new offenders escaped prior regex shapes. A
single canonical reference for the full set exists so future widenings start from a known baseline
instead of being re-derived from CI failure tickers, and so every regex has a match-example and a
counter-example that a later edit is held to.

## Why rows 5 and 6 are proved end-to-end, and what happened before they were

Rows 5 and 6 read a corpus (the compatibility YAML, the TS/Go suites) that a fixture file expresses
more honestly than a bare regex, which is why their red-proving tests drive the real CLI against a
throwaway repository rather than asserting a regex copy.

They used to be proved nowhere, and the contract doc described that as a design: their regexes were
said to be "pinned by the CI and lefthook copies alone", which pins nothing. Those two are RUNNERS of
the regex, not assertions about it — neither can fail on a clean tree, so a regex mutated to match
nothing stayed green in both, twice over (#3182). The same incident is why every row's proof is
CLI-driven: a proof that asserts a copy of the regex (as a separate self-test script once did)
reaches the gate only through a lock-step rule, whereas a proof that drives the real CLI asserts the
regex the gate actually runs.

## Why pattern 6 exists

PR #712 deleted every allow-list array, tolerance constant and soft-assertion helper from the e2e,
Playwright and Go suites — `EXPECTED_EMPTY`, `EXPECTED_TOLERATED`, `isKnownTolerated`,
`tolerated404`, `should_tolerate`, `skipReason` / `SkipReason`, `APP_NOT_INSTALLED_BANNER_PATTERNS`,
`DRILLDOWN_UPSTREAM_GRAFANA_CONSOLE_NOISE` and Playwright's `expect.soft`. Each had been consulted
before a failing assertion to swallow it, or had suppressed a specific crawler signal by name. The
escape-hatch scan keeps those primitives from creeping back by rejecting the identifiers themselves.

## Why pattern 7 is case-insensitive and why it exists at all

The Layer-14 migration lane's scenarios are Gherkin feature files driven by `godog`. A Cucumber
runner's culture carries two suppression routes that Go's `testing` package does not: a `@wip`-style
tag that filters a `Scenario` out of the run, and an unimplemented step reported as *pending* rather
than failed. The runner closes the second one at runtime (`godog.Options.Strict` fails a suite on an
undefined, pending or ambiguous step); pattern 7 closes the first one lexically, on the required
`forbid-skip` PR gate, so a suppressed scenario cannot merge and wait for the next scheduled lane to
notice.

Case-insensitive matching is deliberate: the Gherkin tag vocabulary is closed by construction — the
coverage ratchet's own three recognised forms (`@MIG-nn`, `@tier0`..`@tier2`, `@archetype:<name>`)
are all fixed-case, so nothing legitimate in a `.feature` file ever needs a mixed-case
`@wip`/`@WIP`/`@Skip` spelling. Scanning case-sensitively would let a wrongly-cased suppression tag
merge clean.

## Why pattern 8 is a separate scan from pattern 1

Pattern 1's scope is `*_test.go`, but godog step definitions live in ordinary non-test `.go` files —
the step library is a package the runner imports, not a test package. Every godog skip route
(`godog.ErrSkip`, `godog.ErrPending`, the `TestingT`'s `Skip` / `Skipf` / `SkipNow`) is `t.Skip`
reached by a different door, and pattern 1's scope cannot see any of them. The two scans are not
redundant: neither scope reaches the other's files.

## Why pattern 9 rejects the conditional skip

The conditional form `test.skip(!process.env.X, …)` is not hypothetical: it is what let
`tempo_two_phase_compare.spec.ts` look healthy while running in no lane at all, so the A/B behind a
default-on structural split had never once executed. Removing the skip made the spec fail loudly on
a missing environment, which is what forced the lane that runs it. A missing environment is a hard
failure and a reason to wire the spec into a lane, never a reason to stand the spec down.

## Why the pattern rows are not merged

A read-through of the patterns shows no strict redundancies — each catches a shape the others would
miss:

- Patterns 2 and 3 both target soft-assertion shapes, but they match different `assert.*` calls
  (`Contains` vs `ElementsMatch`). A single combined alternation would work, but the two-regex shape
  keeps the error message specific.
- Pattern 4 has two alternatives in a single regex (bare `defer recover()` vs the multi-line block).
  They cannot be merged with patterns 2 / 3 because pattern 4 needs the `perl -0777` slurp to span
  lines.
- Patterns 1 and 8 both reject a skip call, but neither scope reaches the other's files: pattern 1's
  scope is `*_test.go` across the tree, while godog step definitions are non-test `.go` files under
  the migration harness.
