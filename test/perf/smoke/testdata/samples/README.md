# Real-world-shaped metric samples

Non-synthetic OTel metric samples (PromQL head: gauge, sum, histogram) used by the perf-smoke
sentinel lane (`test/perf/smoke/`, tracking issue #2370) to give the sentinels realistic shape
instead of purely hand-rolled synthetic data. Traces and logs are out of scope for this sample
set.

## Files

Each file is a [zstd-compressed Parquet](https://parquet.apache.org/) table, base64-encoded as
plain text so it satisfies this repo's tracked-binary-artefact gate (`repo-hygiene-binary`).
Decode with `base64 -d <file> > out.parquet` before reading.

- `svc_http_request_duration_seconds.parquet.b64` — classic-histogram HTTP request duration,
  ~18.2M rows. Primary sample: this is a real-world analog for the classic-histogram `arrayJoin`
  fan-out cost investigated in #2408.
- `svc_http_requests_total.parquet.b64` — matching request-count Sum metric for the same series,
  ~18.6M rows.
- `kube_pod_status_reason.parquet.b64` — Kubernetes pod-status Gauge, ~22.1M rows, the
  highest-cardinality metric in this set (up to ~4,800 series in a single window), drawn entirely
  from cluster-infrastructure telemetry (kube-state-metrics-style data), not any application-level
  metric.

Each file combines two sampling passes across a 14-day span: a fixed daily 90-minute baseline
window, plus a second 90-minute window centered on the daily cardinality peak (~2x the baseline
series count) found while checking whether the baseline window was representative. Within any
given window, sample density matches the real scrape interval — no temporal downsampling — since
per-series sample density is the dimension a prior calibration pass found to dominate this
workload's CPU/memory cost. ~58.9M rows combined, ~72MB as zstd-compressed Parquet (~95MB
base64-encoded).

Columns mirror the OTel Collector ClickHouse exporter schema
(`ServiceName`, `MetricName`, `MetricDescription`, `MetricUnit`, `Attributes`,
`ResourceAttributes`, `StartTimeUnix`, `TimeUnix`, plus the value columns for each metric type).

## Scrubbing methodology

Every identifier-bearing string value — every value in `Attributes` and `ResourceAttributes`
(pod names, hostnames, namespaces, route paths, connection strings, everything), plus
`ServiceName` where it names a specific service rather than an infra category — was replaced
with a deterministic `HMAC-SHA256`-derived token, keyed by a random salt generated once and
discarded at the end of the sourcing session. The same real value always maps to the same token
everywhere it appears, so cardinality and `GROUP BY`/label-matching behavior are preserved
without preserving content. Numeric and structural columns (`Count`, `Sum`, `BucketCounts`,
`ExplicitBounds`, `Min`, `Max`, timestamps, `Value`, `AggregationTemporality`) are unmodified —
they carry no identifying content and are exactly the perf-relevant shape data this sample
exists to provide. `Exemplars` were dropped entirely (they reference trace/span IDs and add
re-identification risk for no perf value). Any internal naming convention present in
`MetricName`/`MetricDescription`/`ServiceName` was genericized.

Verified clean against connection-string schemes, IPv4 patterns, email patterns, and internal
naming conventions, at both the JSON and the final compiled Parquet-binary level.

## Not yet wired in

These files are not yet consumed by any test — `test/perf/smoke/seed.go`'s sentinel builders
still generate synthetic data programmatically. Loading these samples to replace or calibrate
that generator is tracked separately.
