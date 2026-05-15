# LogQL + TraceQL TXTAR Fixture Audit (Tautological Tests)

Audit run **2026-05-15** on `test/spec/logql/` (68 fixtures) and
`test/spec/traceql/` (107 fixtures). Mirrors the prior PromQL audit so
RC1 GA doesn't ship trivially-green tests.

## Method

Per-fixture check for the brief's red flags: (1) `expected_rows` empty,
(2) seed always-fails filter, (3) missing `chplan` snapshot, (4) `sql`
is constant, (5) query rejected by parser. Plus one cerberus-specific
shape — (6) **round-trip-tautological**: the seed materialises every
row's label identically (`MATERIALIZED map(...)`), so the chDB
round-trip can't tell "filter works" from "filter dropped". The
lower→SQL text golden still catches dropped predicates; only the
round-trip layer is asserting nothing.

## Findings

Red flags 1, 2, 4, 5 — **none found**. Every fixture has populated
`expected_rows` (when round-tripping), non-trivial `sql`, and a
parser-accepted query.

Red flag 3 — **systematic gap in TraceQL**:

- **0 of 107 TraceQL fixtures carry a `chplan` snapshot.**
  `internal/traceql/lower_test.go` only passes `sql` + `args` to
  `spec.Match`, while LogQL/PromQL runners also pass `chplan`. Any IR
  structural regression emitting the same SQL slips past every TraceQL
  fixture. Out of scope here (one-line runner change + 107-fixture
  regeneration); follow-up PR P1.

Red flag 6 — **14 LogQL fixtures** materialise labels identically so the
label/stream filter is exercised only against rows that all match:

- `stream_selector.txtar`, `stream_with_regex.txtar`,
  `stream_with_two_labels.txtar`, `multiple_label_matchers.txtar`,
  `whitespace_in_braces.txtar` — stream-selector-only, 1–3 seed rows
  all materialised to match.
- `label_filter_eq.txtar`, `label_filter_or.txtar`,
  `label_filter_regex.txtar`, `label_filter_chained.txtar`,
  `level_label_filter.txtar`, `pattern_with_label_filter.txtar`,
  `unpack_with_label_filter.txtar` — label-filter fixtures whose seed
  pre-satisfies every label predicate.
- `pattern.txtar`, `unpack.txtar`, `decolorize.txtar`,
  `label_format.txtar`, `line_format.txtar`, `line_format_compose.txtar`,
  `pattern_then_line_format.txtar`, `unpack_then_line_format.txtar` —
  intentionally lowering-no-ops (`internal/logql/lower.go` returns nil
  for these stages; post-fetch transforms live in `internal/api/loki/`
  `post_process.go` and are covered by the API-layer test files there).
  Flagged for completeness; not a bug.

Red flag 6 — **4 TraceQL fixtures** (post-aggregation predicate at the
seed boundary):

- `count.txtar` (`count() > 0`, seed=3), `count_eq_zero.txtar`
  (`count() = 0`, seed=0), `count_ge_threshold.txtar` (`count() >= 5`,
  seed=5), `count_lt_threshold.txtar` (`count() < 10`, seed=3).

In each, dropping the outer `WHERE Value op ?` would still return the
same row. SQL text golden does catch a predicate-dropped bug.

## Proposed fixes (follow-up PRs)

- **P1 — TraceQL chplan parity**: add `"chplan": spec.PrintChplan(plan)`
  to `internal/traceql/lower_test.go`, regenerate all 107 fixtures with
  `GOLDEN_UPDATE=1`. Largest single gap.
- **P2 — LogQL round-trip discrimination**: switch each of the 14
  flagged seeds to two distinct label sets (one matching, one not) so a
  dropped filter changes the row count. Pattern from
  `static_int.txtar` / `status_unset.txtar`:
  `MATERIALIZED map('job', if(_idx = 0, 'api', 'other'))` with `_idx
  UInt64` seeded `(0), (1)`.
- **P3 — TraceQL `count()` boundary**: change the 4 `count_*` seeds so
  the predicate actually filters (e.g., for `count() >= 5` seed 3 rows
  expecting `[]`, or seed 6 expecting `[[6]]`).

## Scope decision

18 fixtures flagged (>5) → report-only PR per task brief. Follow-up
PRs P1–P3 handle the mechanical seed/runner changes.
