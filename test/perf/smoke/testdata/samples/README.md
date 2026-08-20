# Metric samples

OTel metric samples (PromQL head: gauge, sum, histogram) used by the perf-smoke sentinel lane
(`test/perf/smoke/`, tracking issue #2370) to give the sentinels realistic shape alongside the
hand-rolled synthetic generator already in `test/perf/smoke/seed.go`. Traces and logs are out of
scope for this sample set.

## Files

Each file is a [zstd-compressed Parquet](https://parquet.apache.org/) table, stored via Git LFS
(`.gitattributes`) — `repo-hygiene-binary` recognizes an LFS pointer and does not flag it.

- `svc_http_request_duration_seconds.parquet` — classic-histogram HTTP request duration sample,
  ~18.2M rows. Primary sample: matches the classic-histogram `arrayJoin` fan-out cost investigated
  in #2408.
- `svc_http_requests_total.parquet` — matching request-count Sum sample for the same series,
  ~18.6M rows.
- `kube_pod_status_reason.parquet` — Kubernetes pod-status Gauge sample, ~22.1M rows, the
  highest-cardinality metric in this set (up to ~4,800 series in a single window), drawn entirely
  from cluster-infrastructure telemetry (kube-state-metrics-style data), not any application-level
  metric.

Each file combines two sampling passes across a 14-day span: a fixed daily 90-minute baseline
window, plus a second 90-minute window centered on the daily cardinality peak (~2x the baseline
series count) found while checking whether the baseline window was representative. Within any
given window, sample density matches the real scrape interval — no temporal downsampling — since
per-series sample density is the dimension a prior calibration pass found to dominate this
workload's CPU/memory cost. ~58.9M rows combined, ~72MB as zstd-compressed Parquet.

Columns mirror the OTel Collector ClickHouse exporter schema
(`ServiceName`, `MetricName`, `MetricDescription`, `MetricUnit`, `Attributes`,
`ResourceAttributes`, `StartTimeUnix`, `TimeUnix`, plus the value columns for each metric type).

## Not yet wired in

These files are not yet consumed by any test — `test/perf/smoke/seed.go`'s sentinel builders
still generate synthetic data programmatically. Loading these samples to replace or calibrate
that generator is tracked separately.
