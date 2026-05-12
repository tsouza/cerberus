-- Seed minimal OTel-CH traces for E2E. Mirrors the columns the OTel
-- ClickHouse Exporter writes for the otel_traces table, narrowed to
-- the subset cerberus's TraceQL slice reads today.
--
-- Run via `just e2e-seed` (sources every *.sql under test/e2e/seed/).

CREATE TABLE IF NOT EXISTS otel.otel_traces (
    Timestamp           DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TraceId             String CODEC(ZSTD(1)),
    SpanId              String CODEC(ZSTD(1)),
    ParentSpanId        String CODEC(ZSTD(1)),
    TraceState          String CODEC(ZSTD(1)),
    SpanName            LowCardinality(String) CODEC(ZSTD(1)),
    SpanKind            LowCardinality(String) CODEC(ZSTD(1)),
    ServiceName         LowCardinality(String) CODEC(ZSTD(1)),
    ResourceAttributes  Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ResourceSchemaUrl   LowCardinality(String) CODEC(ZSTD(1)),
    ScopeName           String CODEC(ZSTD(1)),
    ScopeVersion        LowCardinality(String) CODEC(ZSTD(1)),
    SpanAttributes      Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    Duration            Int64 CODEC(ZSTD(1)),
    StatusCode          LowCardinality(String) CODEC(ZSTD(1)),
    StatusMessage       String CODEC(ZSTD(1))
)
ENGINE = MergeTree()
PARTITION BY toDate(Timestamp)
ORDER BY (ServiceName, SpanName, toUnixTimestamp(Timestamp))
TTL toDateTime(Timestamp) + INTERVAL 1 DAY;

-- Three traces × two spans (parent root + child). Mix of services and
-- durations so TraceQL queries like
--   { resource.service.name = "frontend" && duration > 50ms }
-- and structural ops like
--   { resource.service.name = "frontend" } > { resource.service.name = "api" }
-- both return rows.
INSERT INTO otel.otel_traces
  (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName, ResourceAttributes, SpanAttributes, Duration, StatusCode)
VALUES
  -- Trace 1: frontend → api
  (now64(9) - INTERVAL 10 SECOND, 'a0000000000000000000000000000001', '0000000000000001', '',                 'GET /home',        'Server', 'frontend', map('service.name', 'frontend'), map('http.method', 'GET',  'http.status_code', '200'), 150000000, 'Ok'),
  (now64(9) - INTERVAL 9 SECOND,  'a0000000000000000000000000000001', '0000000000000002', '0000000000000001', 'GET /api/users',   'Client', 'api',      map('service.name', 'api'),      map('http.method', 'GET',  'http.status_code', '200'),  80000000, 'Ok'),
  -- Trace 2: frontend → api → db
  (now64(9) - INTERVAL 20 SECOND, 'a0000000000000000000000000000002', '0000000000000003', '',                 'POST /checkout',   'Server', 'frontend', map('service.name', 'frontend'), map('http.method', 'POST', 'http.status_code', '500'), 600000000, 'Error'),
  (now64(9) - INTERVAL 19 SECOND, 'a0000000000000000000000000000002', '0000000000000004', '0000000000000003', 'POST /api/order',  'Client', 'api',      map('service.name', 'api'),      map('http.method', 'POST', 'http.status_code', '500'), 450000000, 'Error'),
  (now64(9) - INTERVAL 19 SECOND, 'a0000000000000000000000000000002', '0000000000000005', '0000000000000004', 'orders.insert',    'Client', 'db',       map('service.name', 'db'),       map('db.system',   'postgres'),                            300000000, 'Error'),
  -- Trace 3: api → db (no frontend)
  (now64(9) - INTERVAL 30 SECOND, 'a0000000000000000000000000000003', '0000000000000006', '',                 'cron.refresh',     'Server', 'api',      map('service.name', 'api'),      map('cron.name',   'refresh'),                              90000000, 'Ok'),
  (now64(9) - INTERVAL 29 SECOND, 'a0000000000000000000000000000003', '0000000000000007', '0000000000000006', 'cache.refresh',    'Client', 'db',       map('service.name', 'db'),       map('db.system',   'redis'),                                40000000, 'Ok');
