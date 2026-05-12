-- Deterministic OTel fixture for the PromQL compatibility harness.
--
-- Seeds otel_metrics_gauge + otel_metrics_sum with predictable series
-- covering the constructs the upstream prometheus/compliance corpus
-- exercises:
--
--   demo_cpu_usage_seconds_total     counter, multi-label, monotonic
--   demo_memory_usage_bytes          gauge,   multi-label
--   demo_http_requests_total         counter, with a reset
--   demo_disk_usage_bytes            gauge,   sparse series
--   demo_disk_total_bytes            gauge,   companion to disk_usage_bytes
--   up                               gauge,   per-instance liveness
--
-- The same logical fixture is remote-written into reference Prometheus by
-- seed.sh, so both stores see identical input data.

-- Schema (matches OTel CH exporter):
CREATE TABLE IF NOT EXISTS otel.otel_metrics_gauge (
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    MetricName         String CODEC(ZSTD(1)),
    MetricDescription  String CODEC(ZSTD(1)),
    MetricUnit         String CODEC(ZSTD(1)),
    Attributes         Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    StartTimeUnix      DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TimeUnix           DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    Value              Float64 CODEC(ZSTD(1))
)
ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(TimeUnix)
ORDER BY (MetricName, toUnixTimestamp64Nano(TimeUnix));

CREATE TABLE IF NOT EXISTS otel.otel_metrics_sum (
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    MetricName         String CODEC(ZSTD(1)),
    MetricDescription  String CODEC(ZSTD(1)),
    MetricUnit         String CODEC(ZSTD(1)),
    Attributes         Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    StartTimeUnix      DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TimeUnix           DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    Value              Float64 CODEC(ZSTD(1)),
    Flags              UInt32 CODEC(ZSTD(1)),
    AggregationTemporality Int32 CODEC(ZSTD(1)),
    IsMonotonic        Bool CODEC(ZSTD(1))
);

-- 1h of fixture at 15s steps = 240 datapoints per series.
-- Anchor at 2026-05-11T00:00:00Z so the fixture is reproducible.
SET param_anchor='2026-05-11 00:00:00';

-- demo_cpu_usage_seconds_total: 2 hosts × 3 modes = 6 series, counters
INSERT INTO otel.otel_metrics_sum
SELECT
    map('service.name', 'demo'),
    'demo_cpu_usage_seconds_total',
    'CPU seconds spent in mode',
    'seconds',
    map('host', host, 'mode', mode),
    toDateTime64({anchor:String}, 9),
    toDateTime64({anchor:String}, 9) + INTERVAL step SECOND,
    toFloat64(step + (host_idx * 100) + (mode_idx * 10)),
    0,
    2,
    true
FROM (
    SELECT
        number AS step,
        arrayElement(['host-a','host-b'], (number % 2) + 1) AS host,
        arrayElement(['user','system','idle'], (number % 3) + 1) AS mode,
        (number % 2) AS host_idx,
        (number % 3) AS mode_idx
    FROM numbers(240)
);

-- demo_memory_usage_bytes: 2 hosts, gauge
INSERT INTO otel.otel_metrics_gauge
SELECT
    map('service.name', 'demo'),
    'demo_memory_usage_bytes',
    'Memory in use',
    'bytes',
    map('host', host),
    toDateTime64({anchor:String}, 9),
    toDateTime64({anchor:String}, 9) + INTERVAL step SECOND,
    toFloat64(2 * 1024 * 1024 * 1024 + (step * 1024) + (host_idx * 512))
FROM (
    SELECT
        number AS step,
        arrayElement(['host-a','host-b'], (number % 2) + 1) AS host,
        (number % 2) AS host_idx
    FROM numbers(240)
);

-- demo_http_requests_total: 2 routes × 2 status codes, with a counter reset
-- partway through to exercise rate() reset handling.
INSERT INTO otel.otel_metrics_sum
SELECT
    map('service.name', 'demo'),
    'demo_http_requests_total',
    'HTTP requests by route + status',
    '1',
    map('route', route, 'status', status),
    toDateTime64({anchor:String}, 9),
    toDateTime64({anchor:String}, 9) + INTERVAL step SECOND,
    if(step < 120,
        toFloat64(step * 10 + (route_idx * 100) + (status_idx * 50)),
        toFloat64((step - 120) * 10 + (route_idx * 100) + (status_idx * 50))),
    0,
    2,
    true
FROM (
    SELECT
        number AS step,
        arrayElement(['/api','/web'], (number % 2) + 1) AS route,
        arrayElement(['200','500'], (number % 2) + 1) AS status,
        (number % 2) AS route_idx,
        (number % 2) AS status_idx
    FROM numbers(240)
);

-- demo_disk_usage_bytes / demo_disk_total_bytes: 1 host × 2 mounts, gauge
INSERT INTO otel.otel_metrics_gauge
SELECT
    map('service.name', 'demo'),
    'demo_disk_usage_bytes',
    'Disk space in use',
    'bytes',
    map('host', 'host-a', 'mount', mount),
    toDateTime64({anchor:String}, 9),
    toDateTime64({anchor:String}, 9) + INTERVAL step SECOND,
    toFloat64(10 * 1024 * 1024 * 1024 + step * mount_idx * 1024)
FROM (
    SELECT
        number AS step,
        arrayElement(['/','/data'], (number % 2) + 1) AS mount,
        ((number % 2) + 1) AS mount_idx
    FROM numbers(240)
);

INSERT INTO otel.otel_metrics_gauge
SELECT
    map('service.name', 'demo'),
    'demo_disk_total_bytes',
    'Total disk capacity',
    'bytes',
    map('host', 'host-a', 'mount', mount),
    toDateTime64({anchor:String}, 9),
    toDateTime64({anchor:String}, 9) + INTERVAL step SECOND,
    toFloat64(100 * 1024 * 1024 * 1024)
FROM (
    SELECT
        number AS step,
        arrayElement(['/','/data'], (number % 2) + 1) AS mount
    FROM numbers(240)
);

-- up: 2 instances, both up.
INSERT INTO otel.otel_metrics_gauge
SELECT
    map('service.name', 'demo'),
    'up',
    'Is the scrape target up',
    '1',
    map('instance', instance, 'job', 'demo'),
    toDateTime64({anchor:String}, 9),
    toDateTime64({anchor:String}, 9) + INTERVAL step SECOND,
    1.0
FROM (
    SELECT
        number AS step,
        arrayElement(['host-a:9100','host-b:9100'], (number % 2) + 1) AS instance
    FROM numbers(240)
);
