# `forbid-skip` — canonical pattern reference

The `forbid-skip` CI gate (and its `lefthook.yml` pre-push mirror) is the
machine-enforced arm of cerberus's GA test-discipline rule: **no `t.Skip`,
no "not implemented", no "skipped" or "deferred" wording, no soft
assertions, no silent panic recovery, no untracked `should_skip` overlay
entries.**

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

The two locations carry **identical** patterns for rows 1–6 of the
summary table below. A former row 7 (a per-PR `should_skip:` overlay
tracking-ref guard) is no longer separately enforced — the strict
rule in `.github/workflows/ci.yml` `forbid-skip` job step "Reject
should_skip overlay entries" rejects every non-empty `should_skip:`
block in `compatibility/**/*.{yml,yaml}` outright, so there is no
per-PR tracking-ref check to assert.

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
| 2   | Reject bare "not implemented" / "skipped" / "deferred" wording                                              | `*_test.go` + `*.txtar`                                         | #461          |
| 3   | Reject "not implemented" wording in production code                                                         | `internal/**/*.go` (non-test)                                   | #197          |
| 4   | Reject `assert.Contains(x, "")` soft assertion                                                              | `*_test.go`                                                     | #587          |
| 5   | Reject `assert.ElementsMatch(x, []T{})` soft assertion                                                      | `*_test.go`                                                     | #587          |
| 6   | Reject silent panic recovery (`defer recover()` and the multi-line `defer func(){ _ = recover() }()` block) | `*_test.go`                                                     | #587 / #648   |

A former row 7 (reject net-new `should_skip:` overlay entries lacking
a tracking ref, PR #596) was superseded by the structural-cleanup PR
that removed the overlay consumer code and replaced the tracking-ref
guard with a strict "reject any non-empty `should_skip:` block" rule
(see `.github/workflows/ci.yml` `forbid-skip` job step "Reject
should_skip overlay entries"). The accepted form is `should_skip: []`;
any element under the key fails CI.

Vendored upstream snapshots under `compatibility/*/upstream/**` are
excluded from every test-file / fixture grep — they sit outside
cerberus's authorship boundary.

## Pattern 1 — `t.Skip[fN]?` (PR #309)

Regex: `t\.Skip[fN]?\(`

PR #309 wired this gate. Before #309 the maintainer noticed `t.Skip`
calls accumulating in test files as a way to mark known-broken tests
as "we'll fix it later." The bug never got fixed. The skip lingered.
The discipline rule became "fix the bug or delete the test, no
skipping" — and the regex is the enforcement.

`[fN]?` covers all three call shapes (`t.Skip`, `t.Skipf`, `t.SkipNow`)
in one alternation. The literal `(` anchors against accidental matches
on identifiers.

- Matches: `func TestFoo(t *testing.T) { t.Skip("upstream bug") }`
- Does NOT match: `func TestFoo(t *testing.T) { tx.Skipper() }` (different
  receiver / method name)

## Pattern 2 — bare discipline-erosion wording (PR #461)

Regex: `not implemented` \| `\bskipped\b` \| `\bdeferred\b`

PR #309 caught only `t.Skip*` calls — but English wording like "not
implemented yet" / "deferred to RC3" / "skipped because foo" in
comments and TXTAR fixtures was just as much a discipline smell.
PR #458 first added a phrase-pattern regex (`deferred to`, `not yet
supported`, etc.); PR #461 widened to bare words because the
phrase-only shape kept missing new offenders.

The bare-word shape does produce some apparent false positives — Go
has the `defer` keyword, mock packages have `Skipper` types — which
is why the gate scope is narrow (`*_test.go` + `*.txtar`) and why the
remedy is "rewrite to a neutral verb (dropped / excluded / rejected /
bypassed)" rather than "delete the comment."

- Matches: `// not implemented yet`, `var skipped = 0`,
  `// deferred until RC3`
- Does NOT match: `// dropped because of foo`, `defer cleanup()` (the
  Go `defer` keyword, distinct from the word "deferred")

## Pattern 3 — "not implemented" in production code (PR #197)

Regex: `not implemented`

PR #197 scrubbed `not implemented` from `internal/**.go` and added the
gate so it stays scrubbed. Bare `skipped` / `deferred` are NOT in the
production gate — `defer` statements described in docstrings would
false-positive, and runtime prose like "request X is rejected" is the
correct rewrite of "skipped" rather than something to forbid.

- Matches: `return errors.New("not implemented")`
- Does NOT match: `return errUnsupported // rejects unsupported foo`

## Pattern 4 — `assert.Contains(x, "")` soft assertion (PR #587)

Regex: `assert\.Contains\([^,]+,\s*""\s*\)`

PR #587 added this. The trigger was a sweep that uncovered
`assert.Contains(body, "")` calls — they pass any input, look like
real assertions in reviews, but verify nothing.

The regex uses `[^,]+` to clamp the haystack-argument match inside a
single function call (so it doesn't reach into adjacent calls), and
`\s*""\s*` to allow optional whitespace around the empty needle.

**Known scope limitation:** the `[^,]+,` clause requires exactly one
comma between haystack and needle, so the regex catches the 2-arg
gocheck-style call but not the testify 3-arg call
(`assert.Contains(t, haystack, "")`). Widening to the testify form is
a deliberate future change — listed as a follow-up rather than rolled
into this normalisation pass so the behaviour of the gate stays
byte-identical to its previous shape.

- Matches: `assert.Contains(body, "")`
- Does NOT match: `assert.Contains(body, "error: foo")`

## Pattern 5 — `assert.ElementsMatch(x, []T{})` soft assertion (PR #587)

Regex: `assert\.ElementsMatch\([^,]+,\s*\[\][^)]*\{\s*\}\s*\)`

Sibling of pattern 4. Same regex shape (`[^,]+,` clamp + empty-needle
match) targeting `ElementsMatch` against an empty slice literal. Same
known scope limitation as pattern 4 — catches 2-arg gocheck-style
calls but not the testify 3-arg form.

- Matches: `assert.ElementsMatch(got, []string{})`
- Does NOT match: `assert.ElementsMatch(got, []string{"a", "b"})`

## Pattern 6 — silent panic recovery (PR #587 / #648)

Regex (perl-slurp form):
`defer\s+recover\s*\(\s*\)` \| `defer\s+func\s*\(\s*\)\s*\{[^{}]*_\s*=\s*recover\s*\(\s*\)`

PR #587 added the single-line `defer recover()` and the same-line
`defer func() { _ = recover() }()`. PR #648 widened to the
multi-line variant via a `perl -0777` slurp because reviewers found
the original regex missed `defer func() {\n  _ = recover()\n}()`.

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

## Former pattern 7 — `should_skip:` overlay tracking-ref guard (PR #596, superseded)

Originally PR #596 added a per-PR guard that diffed the overlay
against `origin/main` and required every net-new `should_skip:` entry
to carry a non-empty `jira:` value, a `link:` field, or a `#NNN`
reference inside `reason:`. The structural-cleanup PR removed the
overlay consumer code entirely, so the per-PR tracking-ref guard is
no longer load-bearing: the `forbid-skip` CI job step "Reject
should_skip overlay entries" now rejects ANY non-empty
`should_skip:` block in `compatibility/**/*.{yml,yaml}` outright. The
delegating helper script and its assertion in
`scripts/test-forbid-skip.sh` were removed at the same time.

## Redundancy review

A read-through of the remaining patterns shows no strict
redundancies — each catches a shape the others would miss:

- Patterns 2 and 3 both look at `not implemented`, but pattern 2 only
  inspects `*_test.go` + `*.txtar` while pattern 3 only inspects
  `internal/**/*.go` (excluding the test files). Together they cover
  the codebase without double-counting.
- Patterns 4 and 5 both target soft-assertion shapes, but they match
  different `assert.*` calls (`Contains` vs `ElementsMatch`). A single
  combined alternation would work but the two-regex shape keeps the
  error message specific.
- Pattern 6 has two alternatives in a single regex (bare `defer
  recover()` vs the multi-line block). They cannot be merged with
  patterns 4 / 5 because pattern 6 needs the `perl -0777` slurp to
  span lines.

The gate's total active pattern count: **6** (patterns 1–6 above).
The former pattern 7 was superseded by the strict overlay-entry
rejection rule in CI.
