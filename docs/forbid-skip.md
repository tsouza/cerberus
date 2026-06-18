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

The scans are implemented **once**, in `.github/scripts/forbid-skip.mjs`,
which runs ONE discipline check per invocation selected by the `$CHECK`
env var (`t-skip`, `not-implemented`, `soft-assert`, `should-skip`,
`escape-hatch`). The regexes were extracted out of the old inline
`ci.yml` bash into that script so the CI job and the local pre-push hook
share a single source of truth. The script runs from **two callers**, and
the two MUST stay in sync:

1. `.github/workflows/ci.yml` job `forbid-skip` — required status check
   on `main`. Each scan is a step that runs
   `node .github/scripts/forbid-skip.mjs` with the matching `CHECK`.
2. `lefthook.yml` `pre-push` hook — local-mirror of the same gate so a
   push that would have failed CI fails locally first.

`scripts/test-forbid-skip.sh` is the assertion that the regexes still
match their canonical positive examples and still reject the matching
counter-examples below. It runs both as a standalone unit-test (invoke
the script directly) and as the "Self-test forbid-skip regex set" step
inside the `forbid-skip` CI job. The lefthook `forbid-skip-self-test`
command runs the same script on pre-push.

The `should_skip:` overlay rejection is the `should-skip` check, and the
escape-hatch rejection is the `escape-hatch` check; both are CHECK
selectors in the same `forbid-skip.mjs`, not separate inline steps.

## Adding a new pattern

When a new offender shape is discovered:

1. Add the new regex to the matching `$CHECK` scan in
   `.github/scripts/forbid-skip.mjs` (or add a new `$CHECK` and wire a
   step for it in **both** `.github/workflows/ci.yml` and `lefthook.yml`)
   in the same PR.
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

The gate is **five `$CHECK` scans** (one `forbid-skip.mjs` invocation
each). Two of them — `t-skip` and `not-implemented` — are a single regex
apiece; the `soft-assert` scan bundles three related shapes (patterns 3-5
below) into one invocation; `should-skip` and `escape-hatch` are the
structural overlay / allow-list rejections. The pattern rows below
expand the regexes; the `CHECK` column maps each to the scan that runs it.

| #   | CHECK             | Intent                                                                                                      | Scope                                                           | Origin        |
| --- | ----------------- | ----------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------- | ------------- |
| 1   | `t-skip`          | Reject `t.Skip[fN]?` calls                                                                                  | `*_test.go`                                                     | #309          |
| 2   | `not-implemented` | Reject "not implemented" wording in production code                                                         | `internal/**/*.go` (non-test)                                   | #197          |
| 3   | `soft-assert`     | Reject `assert.Contains(x, "")` soft assertion (2-arg + testify 3-arg)                                      | `*_test.go`                                                     | #587 / #277   |
| 4   | `soft-assert`     | Reject `assert.ElementsMatch(x, []T{})` soft assertion (2-arg + testify 3-arg)                              | `*_test.go`                                                     | #587 / #277   |
| 5   | `soft-assert`     | Reject silent panic recovery (`defer recover()` and the multi-line `defer func(){ _ = recover() }()` block) | `*_test.go`                                                     | #587 / #648   |
| 6   | `should-skip`     | Reject any non-empty `should_skip:` block                                                                   | `compatibility/**/*.{yml,yaml}`                                 | #596          |
| 7   | `escape-hatch`    | Reject test-suite escape-hatch / tolerance primitives                                                       | `*.ts`, `*.tsx`, `*.go`                                         | #712 / #844   |

So: **seven documented pattern rows across five `$CHECK` scans**. The
prior "6 patterns" framing both predated the escape-hatch row (a
pre-existing documentation gap — the scan has shipped since #712/#844 but
was never tabled here) and counted the three `soft-assert` shapes as
separate "patterns" while the gate runs them as one check.

Row 6 (`should-skip`) rejects any non-empty `should_skip:` block outright
(`forbid-skip.mjs` `CHECK=should-skip`, the "Reject should_skip overlay
entries" step). The only accepted form is `should_skip: []`; any element
under the key fails CI.

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

The `should-skip` scan (`forbid-skip.mjs` `CHECK=should-skip`, the
"Reject should_skip overlay entries" CI step) rejects ANY non-empty
`should_skip:` block in `compatibility/**/*.{yml,yaml}` outright. A
compatibility corpus entry is either scored against the reference or it
is not in the corpus — there is no per-case skip overlay, so the gate
forbids the construct itself rather than auditing each entry's tracking
ref.

## Pattern 7 - test escape-hatch primitives (PR #712, widened #844)

The `escape-hatch` scan (`forbid-skip.mjs` `CHECK=escape-hatch`, the
"Reject test escape-hatch patterns" CI step) rejects the allow-list /
tolerance / soft-assert primitives that mask a real failure instead of
surfacing it. The deletion of the harness escape-hatch mechanisms landed
in #712; the scan keeps them from coming back.

Regex (ERE alternation over `*.ts`, `*.tsx`, `*.go`, excluding
`compatibility/*/upstream/**`, `**/node_modules/**`, `vendor/**`,
`.claude/**`):

```text
EXPECTED_EMPTY|EXPECTED_TOLERATED|isKnownTolerated|tolerated404|
expect\.soft|should_tolerate|skipReason|SkipReason|
APP_NOT_INSTALLED_BANNER_PATTERNS|DRILLDOWN_UPSTREAM_GRAFANA_CONSOLE_NOISE
```

Each alternative is a documented anti-pattern: `EXPECTED_EMPTY` /
`EXPECTED_TOLERATED` / `isKnownTolerated*` / `tolerated404` are allow-list
arrays consulted before failing; `expect.soft(...)` is a Playwright
assertion that records a failure but lets the test continue; `should_skip`
/ `should_tolerate` in code re-introduce the removed YAML schema;
`skipReason` / `SkipReason` is the removed loki-driver overlay skip field.
The rule: every assertion must fail loud — fix the bug at the source
(cerberus code, seed, dashboard, panel) rather than mask it.

- Matches: `if (EXPECTED_TOLERATED.includes(name)) return;`
- Matches: `expect.soft(rows).toHaveLength(0)`
- Does NOT match: `expect(rows).toHaveLength(0)` (a hard assertion)

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

The gate's total active pattern count: **7** documented pattern rows
(patterns 1–7 above), run as **5 `$CHECK` scans** (patterns 3–5 share the
single `soft-assert` scan). Patterns 1–5 run over Go test files /
production code; pattern 6 is the strict overlay-entry rejection over the
compatibility YAML; pattern 7 (`escape-hatch`) runs over the `*.ts` /
`*.tsx` / `*.go` tree.
