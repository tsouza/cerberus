#!/bin/sh
# Compliance harness seeder. Runs once per `docker compose up`.
#
# 1. Pipe seed.sql into ClickHouse so cerberus has data to serve.
# 2. Generate equivalent samples and remote-write them into the reference
#    Prometheus so both stores see identical input. (Implementation lands
#    when M1 is far enough along that we want differential correctness;
#    for now we just seed CH and let Prom run empty — compliance pass
#    rate is informational until M6 anyway.)
#
# Designed for the Alpine clickhouse-server image we use as the seeder
# (it ships `clickhouse-client`).

set -eu

echo "==> waiting for clickhouse to be ready"
until clickhouse-client --host clickhouse --port 9000 --user cerberus --password cerberus --query "SELECT 1" >/dev/null 2>&1; do
  sleep 1
done

echo "==> loading seed.sql"
clickhouse-client \
    --host clickhouse \
    --port 9000 \
    --user cerberus \
    --password cerberus \
    --multiquery < /seed.sql

echo "==> seed done. rows by metric:"
clickhouse-client \
    --host clickhouse \
    --port 9000 \
    --user cerberus \
    --password cerberus \
    --query "
        SELECT MetricName, count() AS rows
        FROM (
            SELECT MetricName FROM otel.otel_metrics_gauge
            UNION ALL
            SELECT MetricName FROM otel.otel_metrics_sum
        )
        GROUP BY MetricName
        ORDER BY MetricName
        FORMAT PrettyCompact
    "

# Remote-write equivalent to Prometheus lands when we want differential
# correctness gating (M1.x — once enough lowering exists to make the
# comparison meaningful).
echo "==> (reference Prometheus remote-write deferred until M1.x)"
