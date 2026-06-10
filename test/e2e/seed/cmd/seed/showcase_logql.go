package main

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Showcase-LogQL seed shapes (PR feat/showcase-logql). The
// showcase-logql dashboard's feature panels need log SHAPES the base
// 3-service seed (insertLogsSQL) doesn't carry:
//
//   - gateway  — logfmt bodies with numeric (status), duration (took),
//     byte-size (size) and IP (remote_addr) fields, for `| logfmt`,
//     the typed label filters (number / duration / bytes), `| unwrap`
//     (+ duration()/bytes() conversions) and `label_format`/`keep`/
//     `drop` stages.
//   - shop     — JSON bodies with a nested object, for `| json` and
//     the parameterised `| json status="response.status"` path form.
//   - proxy    — access-log style fixed-structure lines for the
//     `| pattern` parser, with in-line IPs for future ip() support.
//   - painter  — ANSI-colour-coded lines for `| decolorize`.
//   - packer   — Loki pack-format bodies ({"_entry": ...}) for
//     `| unpack`.
//
// All five streams follow the base seed's discipline: 40 rows at 15 s
// cadence spanning [now-300s, now+285s], re-anchored on now64(9) by
// every rolling re-seed tick. SeverityText tracks the in-body level
// field so `detected_level` filters see consistent values.
const (
	insertShowcaseLogfmtSQL = `INSERT INTO otel_logs
  (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, ResourceAttributes, LogAttributes)
SELECT
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    lpad(toString(number % 8), 32, '0'),
    lpad(toString(number % 8), 16, '0'),
    multiIf(number % 5 = 0, 'ERROR', number % 3 = 0, 'WARN', 'INFO'),
    multiIf(number % 5 = 0, 17, number % 3 = 0, 13, 9),
    'gateway',
    concat(
        'level=', multiIf(number % 5 = 0, 'error', number % 3 = 0, 'warn', 'info'),
        ' method=', arrayElement(['GET', 'POST', 'PUT'], number % 3 + 1),
        ' path=/api/', arrayElement(['users', 'orders', 'items', 'carts'], number % 4 + 1),
        ' status=', multiIf(number % 5 = 0, '500', number % 3 = 0, '404', '200'),
        ' took=', toString((number % 40) * 7 + 5), 'ms',
        ' size=', toString(256 * (number % 8 + 1)), 'B',
        ' remote_addr=', multiIf(
            number % 2 = 0,
            concat('192.168.1.', toString(number % 250 + 1)),
            concat('10.0.0.', toString(number % 250 + 1))
        ),
        ' id=', toString(number)
    ),
    map('service_name', 'gateway'),
    map('thread', concat('worker-', toString(number % 4)))
FROM numbers(40)`

	insertShowcaseJSONSQL = `INSERT INTO otel_logs
  (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, ResourceAttributes, LogAttributes)
SELECT
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    lpad(toString(number % 8 + 8), 32, '0'),
    lpad(toString(number % 8 + 8), 16, '0'),
    multiIf(number % 5 = 0, 'ERROR', number % 3 = 0, 'WARN', 'INFO'),
    multiIf(number % 5 = 0, 17, number % 3 = 0, 13, 9),
    'shop',
    concat(
        '{"level":"', multiIf(number % 5 = 0, 'error', number % 3 = 0, 'warn', 'info'),
        '","message":"order placed","order_id":', toString(number),
        ',"response":{"status":', multiIf(number % 5 = 0, '500', number % 3 = 0, '404', '200'),
        ',"latency_ms":', toString((number % 30) * 3 + 2),
        '},"customer":"cust-', toString(number % 6), '"}'
    ),
    map('service_name', 'shop'),
    map('thread', concat('worker-', toString(number % 4)))
FROM numbers(40)`

	insertShowcasePatternSQL = `INSERT INTO otel_logs
  (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, ResourceAttributes, LogAttributes)
SELECT
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    lpad(toString(number % 8 + 16), 32, '0'),
    lpad(toString(number % 8 + 16), 16, '0'),
    multiIf(number % 5 = 0, 'ERROR', 'INFO'),
    multiIf(number % 5 = 0, 17, 9),
    'proxy',
    concat(
        arrayElement(['GET', 'POST', 'DELETE'], number % 3 + 1),
        ' /shop/items/', toString(number % 7),
        ' ', multiIf(number % 5 = 0, '502', '200'),
        ' ', toString((number % 25) * 11 + 3), 'ms',
        ' ', multiIf(
            number % 2 = 0,
            concat('192.168.2.', toString(number % 250 + 1)),
            concat('10.0.1.', toString(number % 250 + 1))
        )
    ),
    map('service_name', 'proxy'),
    map('thread', concat('worker-', toString(number % 4)))
FROM numbers(40)`

	insertShowcaseANSISQL = `INSERT INTO otel_logs
  (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, ResourceAttributes, LogAttributes)
SELECT
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    lpad(toString(number % 8 + 24), 32, '0'),
    lpad(toString(number % 8 + 24), 16, '0'),
    multiIf(number % 5 = 0, 'ERROR', 'INFO'),
    multiIf(number % 5 = 0, 17, 9),
    'painter',
    concat(
        char(27), multiIf(number % 5 = 0, '[31m', '[32m'),
        multiIf(number % 5 = 0, 'ERROR', 'INFO'),
        char(27), '[0m',
        ' brush stroke rendered id=', toString(number)
    ),
    map('service_name', 'painter'),
    map('thread', concat('worker-', toString(number % 4)))
FROM numbers(40)`

	insertShowcaseUnpackSQL = `INSERT INTO otel_logs
  (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, ResourceAttributes, LogAttributes)
SELECT
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    lpad(toString(number % 8 + 32), 32, '0'),
    lpad(toString(number % 8 + 32), 16, '0'),
    multiIf(number % 5 = 0, 'ERROR', 'INFO'),
    multiIf(number % 5 = 0, 17, 9),
    'packer',
    concat(
        '{"_entry":"packed event id=', toString(number),
        ' status=', multiIf(number % 5 = 0, '500', '200'),
        '","origin":"packer","batch":"b-', toString(number % 5), '"}'
    ),
    map('service_name', 'packer'),
    map('thread', concat('worker-', toString(number % 4)))
FROM numbers(40)`
)

// insertShowcaseLogQLLogs inserts the five showcase-logql streams.
// Called from insertLogs so the rolling re-seeder re-anchors these
// windows on every tick alongside the base log seed.
func insertShowcaseLogQLLogs(ctx context.Context, conn driver.Conn) error {
	for _, ins := range []struct {
		label string
		sql   string
	}{
		{"showcase logfmt (gateway)", insertShowcaseLogfmtSQL},
		{"showcase json (shop)", insertShowcaseJSONSQL},
		{"showcase pattern (proxy)", insertShowcasePatternSQL},
		{"showcase ansi (painter)", insertShowcaseANSISQL},
		{"showcase unpack (packer)", insertShowcaseUnpackSQL},
	} {
		if err := conn.Exec(ctx, ins.sql); err != nil {
			return fmt.Errorf("%s: %w", ins.label, err)
		}
	}
	return nil
}
