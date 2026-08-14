# Spec: #1986 classic histogram count parity

Status: implemented and regenerated; verification pending focused chDB checks.

## Problem

Classic-histogram fixtures may omit physical `Count` because Cerberus derives the
total from `BucketCounts`. The seed DDL supplies the absent column as zero, but
the parity reader used that zero for Prometheus's `_count` and `le="+Inf"`
series. The reference therefore received different data from Cerberus.

## Scope

- Derive a classic histogram's count from its bucket counts only when the fixture
  did not declare `Count`.
- Preserve a declared count verbatim, including an inconsistent one, so the
  parity check can expose it.
- Enrol only classic-histogram fixtures proven to pass the live reference check.

## Audit And Rationale

The original issue's five input-model gaps have already been materially reduced:
gauge and sum rows, projection columns, resource labels, metric-name
normalisation, classic histograms, native histogram values, and `@ start()` /
`@ end()` evaluation are represented by the current parity layer. The remaining
work is no longer one shared reader omission: it is native-histogram edge
semantics, DELTA-temporality values that vanilla Prometheus cannot represent,
non-sample API shapes, and individual real value/count/label disagreements.

This batch is the smallest remaining common root cause in the classic-histogram
residue. A minimal seed omits `Count`; the test DDL backfill supplies zero only
for INSERT arity, while Cerberus derives the total from `BucketCounts`. Passing
that synthetic zero to Prometheus makes the oracle evaluate different input.
Deriving only an omitted count corrects the oracle input without concealing a
fixture that explicitly declares an inconsistent count.

## Task List

1. Complete: derive the omitted classic count in the parity input translator and
   cover both derived and declared-count paths.
2. Complete: enrol four passing classic-histogram quantile fixtures and raise the
   global parity floor from 578 to 582.
3. Complete: regenerate the required shards; the generated diff contains only
   the four intended PromQL parity declarations.
This batch does not close #1986. The parent issue remains the tracker for every
residual class below until its definition of done is met.

4. Follow-up, real disagreements rather than enrolment omissions:
   `histogram_quantile_avg_by_le`,
   `histogram_quantile_sum_by_le`, and `histogram_quantile_with_filter` differ
   by final-bit float values; `histogram_quantile_classic_duplicate_bounds`,
   `histogram_quantile_classic_latest_sample`, and
   `histogram_quantile_classic_multi_series` have material wrong values.
5. Follow-up, separately audit the native-histogram semantic residue,
   structurally non-sample metadata/exemplar/duplicate-labelset fixtures, and
   the two DELTA-temporality counter fixtures. Each must either enrol or gain a
   named mechanical comparison contract before #1986 can close.
