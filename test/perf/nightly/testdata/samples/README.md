# Nightly sample (trimmed)

A single-day, bandwidth-sized derivative of `test/perf/smoke/testdata/samples/`'s full 14-day
real production sample (#2411), used by the #2370 nightly measurement harness
(`test/perf/nightly/`). Not a replacement for the full set — that stays exactly as committed for
manual/occasional deeper calibration use.

## Why a trim

No workflow in this repo sets `lfs: true` on `actions/checkout`, so a CI checkout only ever pulls
LFS pointer files, never the actual bytes — a workflow that needs real content must pull it
explicitly. GitHub's free LFS bandwidth quota is 1 GiB/month; the full sample set (~72 MB) pulled
on every one of ~30 nightly runs/month would exceed that quota by itself, before counting any
human/agent clone traffic. This trim (~5 MB total) keeps nightly bandwidth sustainable
(~150 MB/month) while still exercising the exact same real production shape.

## Selection

**2026-08-18** — the day with the closest-to-median row count across `svc_http_request_duration_seconds`
and `svc_http_requests_total` (both landed within ~30k rows of the 14-day median; every other
day's distance from the median was larger), and a typical (non-outlier) `kube_pod_status_reason`
count. 2026-08-06 (the full set's first captured day) was excluded from consideration as a
gauge-count outlier — roughly half the typical daily volume, consistent with a partial first day
of capture.

## Files

Same schema and columns as the full set's `README.md` — each file combines both of 2026-08-18's
sampling windows (baseline + peak-cardinality), the same two-window-per-day structure the full set
uses throughout:

- `svc_http_request_duration_seconds.parquet` — 1,317,183 rows.
- `svc_http_requests_total.parquet` — 1,338,836 rows.
- `kube_pod_status_reason.parquet` — 1,676,160 rows.

Real captured span: 2026-08-18 09:00:01 – 13:29:59 UTC.
