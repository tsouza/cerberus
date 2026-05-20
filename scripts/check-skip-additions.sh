#!/usr/bin/env bash
#
# check-skip-additions.sh — guard against new `should_skip:` overlay
# entries that lack an unblock-PR / tracking reference.
#
# Background: PRs #429 and #537 added `should_skip:` rows to silence
# failing Loki compat cases instead of fixing the underlying lowering /
# harness gap. Two of those skip-PRs stayed merged for weeks before
# someone wired the proper fix. This guard rejects the same shape going
# forward.
#
# Rule: any net-new `should_skip:` YAML entry added to a tracked overlay
# file must carry one of:
#   - a non-empty `jira:` value
#   - a `link:` field
#   - a `#NNN` GitHub PR / issue reference embedded in `reason:`
#
# Failure mode: clear stderr message naming the offending entry's
# `source:` (or fallback line range) and exit 1.
#
# Self-test: invoke with `--self-test` to run an in-memory regression —
# 4 synthetic cases that exercise reject + accept paths.

set -euo pipefail

# Files this guard inspects. Today only the Loki overlay carries
# should_skip entries; the Prom + Tempo expected-failures.json are JSON
# and use a different shape (covered by their own reviewer-discipline
# rule that each entry needs `reason` + `tracking`). Extending the
# guard to a new YAML overlay is a one-line add here.
OVERLAY_FILES=(
  "compatibility/loki/cerberus-test-queries.yml"
)

# `BASE_REF` is the comparison ref. In CI we diff against
# `origin/main`; locally a contributor can override.
BASE_REF="${BASE_REF:-origin/main}"

# ---------------------------------------------------------------------
# inspect_block <file> <start_line> <end_line>
#
# Reads the YAML block bounded by [start_line, end_line] from <file>
# and verifies one of the three acceptable tracking refs is present.
# Echoes a GH-Actions error annotation on rejection.
# Returns 0 on accept, 1 on reject.
# ---------------------------------------------------------------------
inspect_block() {
  local file="$1"
  local start="$2"
  local end="$3"
  local block
  block="$(sed -n "${start},${end}p" "$file")"

  # Accept if `jira:` carries a non-empty, non-quoted-empty value.
  # Matches:  jira: "foo"  /  jira: 'foo'  /  jira: foo
  # Rejects:  jira: ""     /  jira: ''     /  jira:  (whitespace only)
  if printf '%s\n' "$block" | grep -qE '^[[:space:]]*jira:[[:space:]]*("[^"]+"|'\''[^'\'']+'\''|[^[:space:]"'\'']+)[[:space:]]*$'; then
    return 0
  fi

  # Accept if `link:` carries any non-empty value.
  if printf '%s\n' "$block" | grep -qE '^[[:space:]]*link:[[:space:]]*\S'; then
    return 0
  fi

  # Accept if any field embeds a `#NNN` GH issue/PR ref.
  if printf '%s\n' "$block" | grep -qE '#[0-9]{2,5}'; then
    return 0
  fi

  # Reject — emit a helpful pointer.
  local source_label
  source_label="$(printf '%s\n' "$block" \
    | grep -m1 -E '^[[:space:]]*-?[[:space:]]*source:' \
    | sed -E 's/^[[:space:]]*-?[[:space:]]*source:[[:space:]]*//' || true)"
  if [[ -z "$source_label" ]]; then
    source_label="<lines ${start}-${end}>"
  fi
  printf '::error file=%s,line=%s::new should_skip entry missing tracking ref (need non-empty jira: OR link: OR #NNN in reason). Offending entry: %s\n' \
    "$file" "$start" "$source_label" >&2
  return 1
}

# ---------------------------------------------------------------------
# added_lines_for_file <file>
#
# Emits the set of line numbers in the post-image of <file> that the
# diff-vs-BASE_REF marks as `+` (added). Uses `git diff --unified=0`
# so the output is purely added/removed runs.
# ---------------------------------------------------------------------
added_lines_for_file() {
  local file="$1"
  local base_sha
  if base_sha="$(git merge-base "$BASE_REF" HEAD 2>/dev/null)"; then
    :
  else
    base_sha="$BASE_REF"
  fi

  git diff --unified=0 "${base_sha}..HEAD" -- "$file" 2>/dev/null \
    | awk '
        /^@@ / {
          # Parse `@@ -A,B +C,D @@` — C is post-image start, D count
          # (D defaults to 1 when omitted: `+C @@`).
          match($0, /\+([0-9]+)(,([0-9]+))?/, m)
          start = m[1] + 0
          count = (m[3] == "") ? 1 : m[3] + 0
          for (i = 0; i < count; i++) print start + i
        }
      '
}

# ---------------------------------------------------------------------
# should_skip_ranges <file>
#
# Emits `start end` line-number pairs (one per line) for each
# `should_skip:` block in <file>. End is exclusive — the line BEFORE
# the next top-level YAML key or EOF.
# ---------------------------------------------------------------------
should_skip_ranges() {
  local file="$1"
  awk '
    /^should_skip:/ {
      if (in_block) { print start "\t" (NR - 1) }
      in_block = 1
      start = NR + 1
      next
    }
    /^[a-zA-Z_][a-zA-Z0-9_]*:/ {
      if (in_block) {
        print start "\t" (NR - 1)
        in_block = 0
      }
    }
    END {
      if (in_block) print start "\t" NR
    }
  ' "$file"
}

# ---------------------------------------------------------------------
# scan_file <file>
#
# Walks every `- source:` line in <file>, checks whether it falls
# inside a `should_skip:` block AND inside an added-line range, and
# if so, validates the entry's tracking refs.
# ---------------------------------------------------------------------
scan_file() {
  local file="$1"
  local violations=0

  if [[ ! -f "$file" ]]; then
    return 0
  fi

  local added_lines
  added_lines="$(added_lines_for_file "$file")"
  if [[ -z "$added_lines" ]]; then
    return 0
  fi

  # Build an associative lookup of added line numbers.
  declare -A is_added=()
  while IFS= read -r ln; do
    [[ -n "$ln" ]] && is_added["$ln"]=1
  done <<<"$added_lines"

  # Iterate over should_skip blocks.
  local total_lines
  total_lines="$(wc -l <"$file")"

  while IFS=$'\t' read -r block_start block_end; do
    [[ -z "$block_start" ]] && continue

    # Find every `- source:` line in [block_start, block_end].
    local source_lines
    source_lines="$(awk -v lo="$block_start" -v hi="$block_end" '
      NR >= lo && NR <= hi && /^[[:space:]]*-[[:space:]]+source:/ { print NR }
    ' "$file")"

    while IFS= read -r src_line; do
      [[ -z "$src_line" ]] && continue
      # Skip entries that weren't added on this branch.
      [[ -z "${is_added[$src_line]:-}" ]] && continue

      # Determine the entry's end line — the line before the next
      # `- source:` (or block_end / EOF).
      local entry_end="$block_end"
      local probe=$((src_line + 1))
      while [[ $probe -le $block_end ]]; do
        local probe_content
        probe_content="$(sed -n "${probe}p" "$file")"
        if [[ "$probe_content" =~ ^[[:space:]]*-[[:space:]]+source: ]]; then
          entry_end=$((probe - 1))
          break
        fi
        probe=$((probe + 1))
      done

      if ! inspect_block "$file" "$src_line" "$entry_end"; then
        violations=$((violations + 1))
      fi
    done <<<"$source_lines"
  done < <(should_skip_ranges "$file")

  if [[ $violations -gt 0 ]]; then
    return "$violations"
  fi
  return 0
}

# ---------------------------------------------------------------------
# self_test
#
# Synthesises four skip entries in /tmp and runs the inspector against
# each, verifying that reject + accept decisions match expectation.
# ---------------------------------------------------------------------
self_test() {
  local tmpdir
  tmpdir="$(mktemp -d)"
  # shellcheck disable=SC2064  # expand tmpdir now, not at signal time
  trap "rm -rf '$tmpdir'" EXIT

  # Case A — missing all tracking refs → MUST reject.
  cat >"$tmpdir/case_a.yml" <<'EOF'
should_skip:
  - source: "fast/example.yaml#bad-entry"
    reason: "no tracking ref"
    since: "2026-05-20"
EOF
  if inspect_block "$tmpdir/case_a.yml" 1 4 2>/dev/null; then
    printf 'self-test FAILED: case A (missing tracking ref) should have been rejected\n' >&2
    return 1
  fi

  # Case B — jira populated → MUST accept.
  cat >"$tmpdir/case_b.yml" <<'EOF'
should_skip:
  - source: "fast/example.yaml#good-entry"
    reason: "tracked"
    since: "2026-05-20"
    jira:  "upstream-engine-limitation"
EOF
  if ! inspect_block "$tmpdir/case_b.yml" 1 5; then
    printf 'self-test FAILED: case B (jira populated) should have been accepted\n' >&2
    return 1
  fi

  # Case C — inline `#NNN` ref in reason → MUST accept.
  cat >"$tmpdir/case_c.yml" <<'EOF'
should_skip:
  - source: "fast/example.yaml#inline-ref"
    reason: "tracked via #450"
    since: "2026-05-20"
EOF
  if ! inspect_block "$tmpdir/case_c.yml" 1 4; then
    printf 'self-test FAILED: case C (inline #NNN ref) should have been accepted\n' >&2
    return 1
  fi

  # Case D — jira empty string → MUST reject.
  cat >"$tmpdir/case_d.yml" <<'EOF'
should_skip:
  - source: "fast/example.yaml#empty-jira"
    reason: "empty tracker"
    since: "2026-05-20"
    jira:  ""
EOF
  if inspect_block "$tmpdir/case_d.yml" 1 5 2>/dev/null; then
    printf 'self-test FAILED: case D (empty jira) should have been rejected\n' >&2
    return 1
  fi

  printf 'self-test OK: 4/4 cases passed\n'
  return 0
}

main() {
  if [[ "${1:-}" == "--self-test" ]]; then
    self_test
    exit $?
  fi

  local total_violations=0
  for overlay in "${OVERLAY_FILES[@]}"; do
    local rc=0
    scan_file "$overlay" || rc=$?
    total_violations=$((total_violations + rc))
  done

  if [[ $total_violations -gt 0 ]]; then
    local plural
    if [[ $total_violations -eq 1 ]]; then plural="y"; else plural="ies"; fi
    printf '\n::error::%d new should_skip entr%s missing tracking ref. Each new entry must include a non-empty `jira:` field, a `link:` field, or a `#NNN` GitHub issue/PR reference inside `reason:`. Background: prevents the #429/#537 anti-pattern (skips that masked real bugs).\n' \
      "$total_violations" "$plural" >&2
    exit 1
  fi

  printf 'check-skip-additions: no new should_skip entries missing tracking refs.\n'
}

main "$@"
