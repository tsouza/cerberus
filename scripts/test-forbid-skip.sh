#!/usr/bin/env bash
#
# test-forbid-skip.sh — assertion harness for the `forbid-skip` regex
# set documented in docs/forbid-skip.md.
#
# Each pattern in that doc has a canonical match-example and a
# canonical no-match counter-example. This script writes both into a
# scratch directory and runs the exact regex shape used by
# .github/workflows/ci.yml + lefthook.yml against them, asserting that
# match-examples MATCH and counter-examples do NOT.
#
# Invocation:
#   scripts/test-forbid-skip.sh           # run all cases, exit 0 on green
#   scripts/test-forbid-skip.sh --verbose # print every case decision
#
# Wired into CI as a step inside the `forbid-skip` job. Also runs from
# lefthook's pre-push hook via the `forbid-skip-self-test` command.
#
# When adding a new pattern: update docs/forbid-skip.md AND add a
# `case_N_*` block below. The two MUST stay in lockstep.

set -euo pipefail

VERBOSE=0
if [[ "${1:-}" == "--verbose" ]]; then
  VERBOSE=1
fi

passes=0
failures=0
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

note() {
  if [[ $VERBOSE -eq 1 ]]; then
    printf '  %s\n' "$1"
  fi
}

# expect_match <label> <regex> <fixture-file>
expect_match() {
  local label="$1" regex="$2" file="$3"
  if grep -nE -- "$regex" "$file" >/dev/null; then
    note "PASS  match  $label"
    passes=$((passes + 1))
  else
    printf 'FAIL  match  %s — regex %q did not match %s\n' "$label" "$regex" "$file" >&2
    failures=$((failures + 1))
  fi
}

# expect_no_match <label> <regex> <fixture-file>
expect_no_match() {
  local label="$1" regex="$2" file="$3"
  if grep -nE -- "$regex" "$file" >/dev/null; then
    printf 'FAIL  reject %s — regex %q false-matched %s\n' "$label" "$regex" "$file" >&2
    failures=$((failures + 1))
  else
    note "PASS  reject $label"
    passes=$((passes + 1))
  fi
}

# expect_match_perl / expect_no_match_perl — same idea, but using the
# perl -0777 slurp shape from the CI step (pattern 6).
expect_match_perl() {
  local label="$1" regex="$2" file="$3"
  if perl -0777 -ne 'exit !/'"$regex"'/' <"$file"; then
    note "PASS  match  $label (perl)"
    passes=$((passes + 1))
  else
    printf 'FAIL  match  %s (perl) — regex did not match %s\n' "$label" "$file" >&2
    failures=$((failures + 1))
  fi
}
expect_no_match_perl() {
  local label="$1" regex="$2" file="$3"
  if perl -0777 -ne 'exit !/'"$regex"'/' <"$file"; then
    printf 'FAIL  reject %s (perl) — regex false-matched %s\n' "$label" "$file" >&2
    failures=$((failures + 1))
  else
    note "PASS  reject $label (perl)"
    passes=$((passes + 1))
  fi
}

# --------------------------------------------------------------------
# case 1 — t.Skip / t.Skipf / t.SkipNow  (PR #309)
# --------------------------------------------------------------------
RE1='t\.Skip[fN]?\('
printf 'func TestFoo(t *testing.T) { t.Skip("upstream bug") }\n' >"$tmpdir/case1_match.txt"
printf 'func TestFoo(t *testing.T) { tx.Skipper() }\n'           >"$tmpdir/case1_nomatch.txt"
expect_match    "case1 t.Skip"        "$RE1" "$tmpdir/case1_match.txt"
expect_no_match "case1 tx.Skipper()"  "$RE1" "$tmpdir/case1_nomatch.txt"

# --------------------------------------------------------------------
# case 2 — bare discipline-erosion wording  (PR #461)
# --------------------------------------------------------------------
RE2='not implemented|\bskipped\b|\bdeferred\b'
{
  printf '// not implemented yet\n'
  printf 'var skipped = 0\n'
  printf '// deferred until RC3\n'
} >"$tmpdir/case2_match.txt"
{
  printf '// dropped because of foo\n'
  printf 'defer cleanup()\n'
  printf 'rejection := errReject\n'
} >"$tmpdir/case2_nomatch.txt"
expect_match    "case2 bare wording"            "$RE2" "$tmpdir/case2_match.txt"
expect_no_match "case2 neutral verbs + defer kw" "$RE2" "$tmpdir/case2_nomatch.txt"

# --------------------------------------------------------------------
# case 3 — "not implemented" in production code  (PR #197)
# --------------------------------------------------------------------
RE3='not implemented'
printf 'return errors.New("not implemented")\n' >"$tmpdir/case3_match.txt"
printf 'return errUnsupported // rejects unsupported foo\n' >"$tmpdir/case3_nomatch.txt"
expect_match    "case3 not-implemented prod"  "$RE3" "$tmpdir/case3_match.txt"
expect_no_match "case3 factual verb"          "$RE3" "$tmpdir/case3_nomatch.txt"

# --------------------------------------------------------------------
# case 4 — assert.Contains(x, "")  (PR #587, widened #277)
# --------------------------------------------------------------------
# The optional `([^,]+,\s*){0,1}` prefix matches the testify
# `t *testing.T` first arg when present, so the regex catches BOTH
# the 2-arg gocheck-style call (`assert.Contains(haystack, "")`) and
# the 3-arg testify call (`assert.Contains(t, haystack, "")`).
RE4='assert\.Contains\(([^,]+,\s*){0,1}[^,]+,\s*""\s*\)'
printf 'assert.Contains(body, "")\n'              >"$tmpdir/case4_match.txt"
printf 'assert.Contains(t, body, "")\n'           >"$tmpdir/case4_match_testify.txt"
printf 'assert.Contains(body, "error: foo")\n'    >"$tmpdir/case4_nomatch.txt"
printf 'assert.Contains(t, body, "error: foo")\n' >"$tmpdir/case4_nomatch_testify.txt"
expect_match    "case4 empty-needle Contains (2-arg)"   "$RE4" "$tmpdir/case4_match.txt"
expect_match    "case4 empty-needle Contains (3-arg)"   "$RE4" "$tmpdir/case4_match_testify.txt"
expect_no_match "case4 real-needle Contains (2-arg)"    "$RE4" "$tmpdir/case4_nomatch.txt"
expect_no_match "case4 real-needle Contains (3-arg)"    "$RE4" "$tmpdir/case4_nomatch_testify.txt"

# --------------------------------------------------------------------
# case 5 — assert.ElementsMatch(x, []T{})  (PR #587, widened #277)
# --------------------------------------------------------------------
# Same optional-`t`-prefix widening as case4 — covers both 2-arg
# gocheck-style and 3-arg testify shapes.
RE5='assert\.ElementsMatch\(([^,]+,\s*){0,1}[^,]+,\s*\[\][^)]*\{\s*\}\s*\)'
printf 'assert.ElementsMatch(got, []string{})\n'           >"$tmpdir/case5_match.txt"
printf 'assert.ElementsMatch(t, got, []string{})\n'        >"$tmpdir/case5_match_testify.txt"
printf 'assert.ElementsMatch(got, []string{"a", "b"})\n'   >"$tmpdir/case5_nomatch.txt"
printf 'assert.ElementsMatch(t, got, []string{"a", "b"})\n' >"$tmpdir/case5_nomatch_testify.txt"
expect_match    "case5 empty-slice ElementsMatch (2-arg)" "$RE5" "$tmpdir/case5_match.txt"
expect_match    "case5 empty-slice ElementsMatch (3-arg)" "$RE5" "$tmpdir/case5_match_testify.txt"
expect_no_match "case5 populated ElementsMatch (2-arg)"   "$RE5" "$tmpdir/case5_nomatch.txt"
expect_no_match "case5 populated ElementsMatch (3-arg)"   "$RE5" "$tmpdir/case5_nomatch_testify.txt"

# --------------------------------------------------------------------
# case 6 — silent panic recovery, both shapes  (PR #587 / #648)
# --------------------------------------------------------------------
RE6='defer\s+recover\s*\(\s*\)|defer\s+func\s*\(\s*\)\s*\{[^{}]*_\s*=\s*recover\s*\(\s*\)'
# 6a — bare `defer recover()`
printf 'defer recover()\n' >"$tmpdir/case6a_match.txt"
expect_match_perl "case6a bare-defer-recover" "$RE6" "$tmpdir/case6a_match.txt"
# 6b — multi-line `defer func() { _ = recover() }()`
{
  printf 'defer func() {\n'
  printf '  _ = recover()\n'
  printf '}()\n'
} >"$tmpdir/case6b_match.txt"
expect_match_perl "case6b multi-line silent recover" "$RE6" "$tmpdir/case6b_match.txt"
# 6c — asserted-panic form MUST NOT match
{
  printf 'defer func() {\n'
  printf '  r := recover()\n'
  printf '  if r == nil { t.Fatal("expected panic") }\n'
  printf '}()\n'
} >"$tmpdir/case6c_nomatch.txt"
expect_no_match_perl "case6c asserted-panic form" "$RE6" "$tmpdir/case6c_nomatch.txt"

# --------------------------------------------------------------------
# (former case 7 — should_skip overlay tracking-ref guard, PR #596)
#
# Superseded by the strict rule landed in
# `.github/workflows/ci.yml` `forbid-skip` job step "Reject should_skip
# overlay entries", which rejects ANY non-empty `should_skip:` block
# in `compatibility/**/*.{yml,yaml}` (the accepted form is
# `should_skip: []`). The per-PR tracking-ref delegation to
# `scripts/check-skip-additions.sh` is no longer needed and the
# script itself was removed; there is nothing left to self-test here.
# --------------------------------------------------------------------

# --------------------------------------------------------------------
# summary
# --------------------------------------------------------------------
total=$((passes + failures))
if [[ $failures -gt 0 ]]; then
  printf '\nforbid-skip regex tests: %d/%d FAILED\n' "$failures" "$total" >&2
  exit 1
fi
printf 'forbid-skip regex tests: %d/%d OK\n' "$passes" "$total"
