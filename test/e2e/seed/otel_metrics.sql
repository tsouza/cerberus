-- Seed minimal OTel-CH metrics for E2E. Mirrors the columns the OTel
-- ClickHouse Exporter writes for the Gauge / Sum tables, narrowed to the
-- subset cerberus's PromQL slice reads today.
--
-- Run via `just e2e-seed`.

CREATE TABLE IF NOT EXISTS otel.otel_metrics_gauge (
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ResourceSchemaUrl  String CODEC(ZSTD(1)),
    ScopeName          String CODEC(ZSTD(1)),
    ScopeVersion       String CODEC(ZSTD(1)),
    ScopeAttributes    Map(LowCardinality(String), String) CODEC(ZSTD(1)),
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
ORDER BY (MetricName, toUnixTimestamp64Nano(TimeUnix))
TTL toDateTime(TimeUnix) + INTERVAL 1 DAY;

CREATE TABLE IF NOT EXISTS otel.otel_metrics_sum (
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ResourceSchemaUrl  String CODEC(ZSTD(1)),
    ScopeName          String CODEC(ZSTD(1)),
    ScopeVersion       String CODEC(ZSTD(1)),
    ScopeAttributes    Map(LowCardinality(String), String) CODEC(ZSTD(1)),
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
)
ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(TimeUnix)
ORDER BY (MetricName, toUnixTimestamp64Nano(TimeUnix))
TTL toDateTime(TimeUnix) + INTERVAL 1 DAY;

-- Histogram table — empty for now, but its existence matters: the Prom
-- metadata endpoints (/api/v1/labels, /api/v1/label/<n>/values, /metadata)
-- UNION ALL across gauge + sum + histogram. Without the table, those
-- queries fail with `Table otel.otel_metrics_histogram doesn't exist`.
CREATE TABLE IF NOT EXISTS otel.otel_metrics_histogram (
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ResourceSchemaUrl  String CODEC(ZSTD(1)),
    ScopeName          String CODEC(ZSTD(1)),
    ScopeVersion       String CODEC(ZSTD(1)),
    ScopeAttributes    Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    MetricName         String CODEC(ZSTD(1)),
    MetricDescription  String CODEC(ZSTD(1)),
    MetricUnit         String CODEC(ZSTD(1)),
    Attributes         Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    StartTimeUnix      DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TimeUnix           DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    Count              UInt64 CODEC(Delta(8), ZSTD(1)),
    Sum                Float64 CODEC(ZSTD(1)),
    BucketCounts       Array(UInt64) CODEC(ZSTD(1)),
    ExplicitBounds     Array(Float64) CODEC(ZSTD(1)),
    Value              Float64 CODEC(ZSTD(1)),
    Flags              UInt32 CODEC(ZSTD(1)),
    AggregationTemporality Int32 CODEC(ZSTD(1))
)
ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(TimeUnix)
ORDER BY (MetricName, toUnixTimestamp64Nano(TimeUnix))
TTL toDateTime(TimeUnix) + INTERVAL 1 DAY;

-- Two `up` series at the current time so the PromQL slice has something to
-- return. PR8's playwright smoke queries these.
INSERT INTO otel.otel_metrics_gauge
  (ResourceAttributes, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value)
VALUES
  ({'service.name': 'api'}, 'up', 'Is the scrape target up', '1', {'job': 'api'}, now64(9), now64(9), 1.0),
  ({'service.name': 'db'},  'up', 'Is the scrape target up', '1', {'job': 'db'},  now64(9), now64(9), 0.0);

-- A small counter to exercise rate() once M1.1 lands RangeWindow emission.
INSERT INTO otel.otel_metrics_sum
  (ResourceAttributes, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value, Flags, AggregationTemporality, IsMonotonic)
SELECT
    map('service.name', 'api'),
    'http_server_request_duration_count',
    'HTTP request count by status',
    '1',
    map('job', 'api', 'http_status', '200'),
    now64(9) - INTERVAL number SECOND,
    now64(9) - INTERVAL number SECOND,
    toFloat64(1000 + number * 5),
    toUInt32(0),
    toInt32(2),
    true
FROM numbers(60);
