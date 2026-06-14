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
summary table below. Row 7 is enforced by the CI `forbid-skip` job step
"Reject should_skip overlay entries", which rejects every non-empty
`should_skip:` block in `compatibility/**/*.{yml,yaml}` outright.

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
| 7   | Reject any non-empty `should_skip:` block                                                                   | `compatibility/**/*.{yml,yaml}`                                 | #596          |

Row 7 rejects any non-empty `should_skip:` block outright (see
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

## Pattern 2 — bare discipline-erosion wording (PR #461)

Regex: `not implemented` \| `\bskipped\b` \| `\bdeferred\b`

Pattern 1 catches only `t.Skip*` calls — but English wording like "not
implemented yet" / "deferred to RC3" / "skipped because foo" in
comments and TXTAR fixtures is just as much a discipline smell. This
pattern matches the bare words rather than fixed phrases, because a
phrase-only shape misses new offenders.

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

`internal/**.go` carries no `not implemented` wording, and this gate
keeps it that way. Bare `skipped` / `deferred` are NOT in the
production gate — `defer` statements described in docstrings would
false-positive, and runtime prose like "request X is rejected" is the
correct rewrite of "skipped" rather than something to forbid.

- Matches: `return errors.New("not implemented")`
- Does NOT match: `return errUnsupported // rejects unsupported foo`

## Pattern 4 — `assert.Contains(x, "")` soft assertion (PR #587, widened #277)

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

## Pattern 5 — `assert.ElementsMatch(x, []T{})` soft assertion (PR #587, widened #277)

Regex: `assert\.ElementsMatch\(([^,]+,\s*){0,1}[^,]+,\s*\[\][^)]*\{\s*\}\s*\)`

Sibling of pattern 4. Same regex shape (`[^,]+,` clamp + empty-needle
match) targeting `ElementsMatch` against an empty slice literal. The
optional `([^,]+,\s*){0,1}` prefix matches the testify-style
`t *testing.T` first arg when present, so the regex catches BOTH the
2-arg gocheck-style and the 3-arg testify forms.

- Matches (2-arg gocheck): `assert.ElementsMatch(got, []string{})`
- Matches (3-arg testify): `assert.ElementsMatch(t, got, []string{})`
- Does NOT match: `assert.ElementsMatch(got, []string{"a", "b"})`
- Does NOT match: `assert.ElementsMatch(t, got, []string{"a", "b"})`

## Pattern 6 — silent panic recovery (PR #587 / #648)

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

## Pattern 7 — `should_skip:` compatibility overlay

The `forbid-skip` CI job step "Reject should_skip overlay entries"
rejects ANY non-empty `should_skip:` block in
`compatibility/**/*.{yml,yaml}` outright. A compatibility corpus entry
is either scored against the reference or it is not in the corpus —
there is no per-case skip overlay, so the gate forbids the construct
itself rather than auditing each entry's tracking ref.

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

The gate's total active pattern count: **7** (patterns 1–7 above).
Patterns 1–6 run over Go test files / fixtures / production code;
pattern 7 is the strict overlay-entry rejection over the compatibility
YAML.
