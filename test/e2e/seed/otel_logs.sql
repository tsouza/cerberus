-- Seed minimal OTel-CH logs for E2E. Mirrors the columns the OTel
-- ClickHouse Exporter writes for the otel_logs table, narrowed to
-- the subset cerberus's LogQL slice reads today.
--
-- Run via `just e2e-seed` (sources every *.sql under test/e2e/seed/).

CREATE TABLE IF NOT EXISTS otel.otel_logs (
    Timestamp          DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TimestampTime      DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TraceId            String CODEC(ZSTD(1)),
    SpanId             String CODEC(ZSTD(1)),
    TraceFlags         UInt32 CODEC(ZSTD(1)),
    SeverityText       LowCardinality(String) CODEC(ZSTD(1)),
    SeverityNumber     Int32 CODEC(ZSTD(1)),
    ServiceName        LowCardinality(String) CODEC(ZSTD(1)),
    Body               String CODEC(ZSTD(1)),
    ResourceSchemaUrl  LowCardinality(String) CODEC(ZSTD(1)),
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeSchemaUrl     LowCardinality(String) CODEC(ZSTD(1)),
    ScopeName          String CODEC(ZSTD(1)),
    ScopeVersion       LowCardinality(String) CODEC(ZSTD(1)),
    ScopeAttributes    Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    LogAttributes      Map(LowCardinality(String), String) CODEC(ZSTD(1))
)
ENGINE = MergeTree()
PARTITION BY toDate(TimestampTime)
ORDER BY (ServiceName, SeverityText, toUnixTimestamp(TimestampTime))
TTL toDateTime(TimestampTime) + INTERVAL 1 DAY;

-- Seed three services × three lines, all in the last minute, so a
-- LogQL `{service_name="api"}` stream selector returns rows and
-- `rate({service_name="api"}[5m])` returns a non-zero metric.
INSERT INTO otel.otel_logs
  (Timestamp, TimestampTime, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, ResourceAttributes, LogAttributes)
SELECT
    now64(9) - INTERVAL toUInt64((number % 60)) SECOND AS ts,
    ts,
    lpad(toString(number % 4), 32, '0'),
    lpad(toString(number % 4), 16, '0'),
    multiIf(number % 5 = 0, 'ERROR', number % 3 = 0, 'WARN', 'INFO'),
    multiIf(number % 5 = 0, 17, number % 3 = 0, 13, 9),
    arrayElement(['api', 'frontend', 'db'], number % 3 + 1),
    concat(
        arrayElement(['handled request', 'connection refused', 'slow query', 'cache hit', 'auth failed'], number % 5 + 1),
        ' id=', toString(number)
    ),
    map('service.name', arrayElement(['api', 'frontend', 'db'], number % 3 + 1)),
    map('thread', concat('worker-', toString(number % 4)))
FROM numbers(60);
