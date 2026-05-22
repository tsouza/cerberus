# `forbid-skip` — canonical pattern reference

The `forbid-skip` CI gate (and its `lefthook.yml` pre-push mirror) is the
machine-enforced arm of cerberus's GA test-discipline rule: **no `t.Skip`,
no "not implemented", no "skipped" or "deferred" wording, no soft
assertions, no silent panic recovery, no untracked `should_skip` overlay
entries.**

The gate grew iteratively — six PRs widened it as new offenders escaped
prior regex shapes. This document is the canonical, frozen reference for
the full pattern set so future widenings start from a known baseline
instead of re-deriving from CI failure tickers.

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
summary table below. Row 7 (`should_skip:` overlay tracking-ref guard)
is CI-only because it needs a diff against `origin/main` to identify
net-new entries — locally that base-ref isn't always meaningful, and
the CI gate is the authoritative pre-merge backstop.

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
| 4   | Reject `assert.Contains(x, "")` soft assertion (2-arg + testify 3-arg)                                      | `*_test.go`                                                     | #587 / #277   |
| 5   | Reject `assert.ElementsMatch(x, []T{})` soft assertion (2-arg + testify 3-arg)                              | `*_test.go`                                                     | #587 / #277   |
| 6   | Reject silent panic recovery (`defer recover()` and the multi-line `defer func(){ _ = recover() }()` block) | `*_test.go`                                                     | #587 / #648   |
| 7   | Reject net-new `should_skip:` overlay entries lacking a tracking ref                                        | overlay YAML in `OVERLAY_FILES`                                 | #596          |

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

## Pattern 4 — `assert.Contains(x, "")` soft assertion (PR #587, widened #277)

Regex: `assert\.Contains\(([^,]+,\s*){0,1}[^,]+,\s*""\s*\)`

PR #587 added this. The trigger was a sweep that uncovered
`assert.Contains(body, "")` calls — they pass any input, look like
real assertions in reviews, but verify nothing.

The regex uses `[^,]+` to clamp the haystack-argument match inside a
single function call (so it doesn't reach into adjacent calls), and
`\s*""\s*` to allow optional whitespace around the empty needle. The
leading `([^,]+,\s*){0,1}` is an optional prefix that consumes a
testify-style first argument (`t *testing.T`) when present, so the
single regex catches BOTH the 2-arg gocheck-style call
(`assert.Contains(haystack, "")`) and the 3-arg testify call
(`assert.Contains(t, haystack, "")`). The original PR #587 regex was
2-arg-only; PR #277 widened it to both shapes after the limitation
was flagged on PR #719.

- Matches (2-arg gocheck): `assert.Contains(body, "")`
- Matches (3-arg testify): `assert.Contains(t, body, "")`
- Does NOT match: `assert.Contains(body, "error: foo")`
- Does NOT match: `assert.Contains(t, body, "error: foo")`

## Pattern 5 — `assert.ElementsMatch(x, []T{})` soft assertion (PR #587, widened #277)

Regex: `assert\.ElementsMatch\(([^,]+,\s*){0,1}[^,]+,\s*\[\][^)]*\{\s*\}\s*\)`

Sibling of pattern 4. Same regex shape (`[^,]+,` clamp + empty-needle
match) targeting `ElementsMatch` against an empty slice literal. The
optional `([^,]+,\s*){0,1}` prefix matches the testify-style
`t *testing.T` first arg when present, so the regex catches BOTH the
2-arg gocheck-style and the 3-arg testify forms — widened by PR #277
on top of PR #587's original 2-arg-only shape.

- Matches (2-arg gocheck): `assert.ElementsMatch(got, []string{})`
- Matches (3-arg testify): `assert.ElementsMatch(t, got, []string{})`
- Does NOT match: `assert.ElementsMatch(got, []string{"a", "b"})`
- Does NOT match: `assert.ElementsMatch(t, got, []string{"a", "b"})`

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

## Pattern 7 — `should_skip:` overlay tracking-ref guard (PR #596)

Regex: not a single line of regex; delegated to
`scripts/check-skip-additions.sh`, which diffs the overlay against
`origin/main` and requires every net-new `should_skip:` entry to carry
one of: a non-empty `jira:` value, a `link:` field, or a `#NNN` GitHub
PR / issue reference inside `reason:`.

PR #596 added this after PRs #429 and #537 added `should_skip:` rows
to silence failing Loki compat cases without fixing the underlying
bug. Two of those skip-PRs stayed merged for weeks before someone
wired the real fix.

The guard script has a `--self-test` mode that the CI step runs first.
`scripts/test-forbid-skip.sh` delegates to that self-test as its
case 7 assertion.

- Matches (net-new entry, no tracking ref):

  ```yaml
  - source: "fast/example.yaml#bad-entry"
    reason: "no tracking ref"
    since: "2026-05-20"
  ```

- Does NOT match (net-new entry with inline ref):

  ```yaml
  - source: "fast/example.yaml#good-entry"
    reason: "tracked via #450"
    since: "2026-05-20"
  ```

## Redundancy review (2026-05-22)

A read-through of the seven patterns shows no strict redundancies —
each catches a shape the others would miss:

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

The gate's total pattern count after this documentation pass: **7**
(unchanged — this PR documents the existing set rather than altering
behaviour).
