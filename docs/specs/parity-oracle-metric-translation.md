# Spec: parity-oracle metric translation

Tracks issue #2025. Status: implemented.

## Problem

The Prometheus parity oracle reads rows that were seeded into chDB, but it does not yet model two
facts about Cerberus's OTel metric surface:

- `labelsFromSeededRow` puts raw `MetricName` into `__name__`. Cerberus exposes an OTel name such as
  `container.cpu.usage` as `container_cpu_usage`; consequently, the oracle cannot answer the same
  `__name__` regex or negative-regex query as Cerberus.
- `readSeededNativeHistograms` constructs one native-histogram series only. Cerberus also exposes
  float `<name>_count` and `<name>_sum` series from an exponential-histogram row's `Count` and `Sum`
  columns. Vanilla Prometheus needs those float series as input; it cannot infer Cerberus's selector
  affordance from the histogram-valued series.

This leaves four executable fixtures unenrolled: `regex_name_matcher_underscored_matches_dotted`,
`not_regex_name_matcher_excludes_dotted`, `histogram_mean_latency_exp`, and
`regex_name_matcher_scans_exp_histogram_table`. The divergence is caused by unequal input data, not
by the query result under test.

## Scope

Included:

- Extend the Prometheus parity input translator in `test/spec` with an independent OTel-to-Prometheus
  metric-name mapping and conditional exponential-histogram companion series.
- Add focused translator tests and enrol the four named fixtures, increasing the parity enrolment
  floor and regenerating only artefacts the repository's golden workflow identifies as stale.
- Examine the remaining value disagreements only to enrol fixtures proven to have either of these
  two input-model causes.

Excluded:

- Production lowering, schema, API formatting, or ClickHouse SQL changes.
- A new parity scope, exclusion set, tolerance, or a general native-histogram decomposition. Native
  histograms remain histogram-valued; only the documented `Count` and `Sum` columns become their
  corresponding float companions.
- Changes for other #1986 classes, including DELTA temporality and non-Sample projections.

## Design

Keep the translator independent of Cerberus production packages, as enforced by
`TestParityOracleImportsNoCerberusLowering`. Add a small test-only metric-name translator beside the
existing independent label translator. It will preserve ASCII letters, digits, `_`, and `:`, prefix a
leading digit with `_`, and replace every other input byte with `_`. This is an independently stated
data-model mapping, not an import or call to `internal/api/format`; tests must cover dotted names,
negative-regex-relevant aliases, leading digits, colons, and multibyte input.

Apply that mapping whenever the reader creates a Prometheus `__name__`, including ordinary rows,
classic-histogram synthetic names, native-histogram names, and native companions. Thus every series
presented to upstream Prometheus has the same user-visible name as the Cerberus input surface. After
evaluation, restore the stored spelling for comparison: use the original full label set first, then a
normalized-name fallback only when that name has one stored spelling. A collision leaves a
label-transformed result normalized so it fails visibly instead of being attributed to an arbitrary raw
metric name.

When reading `otel_metrics_exponential_histogram`, construct its native-histogram sample only for a
direct exact-name selector. Cerberus's regex-only surface exposes the documented companions rather
than the native value, so the adapter supplies no native sample on that path. In addition, append float
series named `<normalised base>_count` and `<normalised base>_sum` from the row's `Count` and `Sum`
only when the respective physical column is declared by that fixture's seed. Do not synthesise a
companion for an absent column: Cerberus cannot read that column either, so a zero/default series would
make the oracle answer a different query. Reuse the oracle's local series-assembly helper, not
production lowering code, so labels, timestamps, and point grouping stay identical to other translated
series.

The translation is a data-model adapter like resource-label flattening and classic-histogram expansion:
it gives both evaluators equivalent input. It does not reproduce parser, routing, matcher, plan, or
SQL-emission logic. A production regression in those layers can still disagree with the independently
evaluated reference result.

## Verification

- Add focused `chdb` tests in `test/spec/parity_columns_chdb_test.go` for metric-name translation and
  native companion construction: names and labels, values, timestamps, grouping, and absence when a
  source column is not seeded.
- Run the narrowed chdb-tagged `test/spec` cases covering those translator tests and the four newly
  enrolled fixtures. Verify the positive and negative name regex fixtures produce the expected
  series, the rate division receives float `_sum`/`_count` inputs, and the broad regex sees the two
  exponential companions without replacing classic-histogram output.
- Run `TestParityOracleImportsNoCerberusLowering`, `TestParitySectionsParse`, and
  `TestParityEnrolmentFloor` to preserve oracle independence and enrolment accounting.
- Run `just update-golden parity` only after the implementation and fixture enrolment are complete;
  inspect the generated parity ledger/baseline diff and commit only changes attributable to the
  increased enrolment. Do not hand-edit generated files.

## Risks

- Mirroring production code would turn the oracle into an agreement-by-construction check. The local
  mapping must remain a concise, documented data-model expression and the existing dependency scan
  must remain green.
- A companion emitted for a missing physical column would hide a failed Cerberus query behind a
  fabricated Prometheus series. Column-declaration gating prevents that.
- Name normalisation can merge storage spellings. The translator must preserve the existing
  series-key grouping behavior so any collision is visible as a reference-data problem rather than
  silently overwriting points.
- Enrolling fixtures changes generated parity accounting. The golden recipe and diff review are
  required to avoid accepting unrelated baseline movement.

## Task List For Owner Review

1. Add independently implemented, byte-level OTel metric-name translation to the parity reader and
   focused unit coverage for its documented mapping. Verify the narrowed translator tests and
   `TestParityOracleImportsNoCerberusLowering`.
2. Apply the translator at every parity-reader metric-name construction point, including ordinary,
   classic-histogram, and native-histogram series. Verify the existing classic-reader coverage plus
   focused dotted-name and collision/grouping cases.
3. Extend the native-histogram reader to append `_count` and `_sum` float companions only for declared
   `Count` and `Sum` columns, with tests for values, timestamps, labels, grouping, and absent columns.
   Verify the narrowed native-reader tests.
4. Add `parity:` contracts to the four issue fixtures and raise `parityEnrolmentFloor` by exactly the
   number of newly enrolled fixtures. Verify their narrowed parity executions and the parity contract
   tests.
5. Audit the remaining value disagreements for these exact input-model causes; enrol any confirmed
   fixtures in the same change with focused evidence, contracts, and the corresponding floor increase.
   Do not add an exception for unconfirmed cases.
6. Regenerate the required parity artefacts with `just update-golden parity`, review their diff, and
   run the verification commands named above against the final tree.
