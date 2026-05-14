# Test strategy

Cerberus relies on a layered test suite that fans out from low-cost
unit checks to high-cost end-to-end smoke tests. This doc enumerates
the layers, the gating policy for each, and how the TXTAR spec suite
opts into a semantic assertion layer on top of its text goldens.

## Layers

| Layer                        | Driver                                                 | Build tag      | Gate                  |
| ---------------------------- | ------------------------------------------------------ | -------------- | --------------------- |
| Unit tests                   | `just test` (Go + race)                                | none           | Required (`check`)    |
| TXTAR spec text-equality     | `just test` walks `test/spec/<head>/*.txtar`           | none           | Required (`check`)    |
| TXTAR spec round-trip (chDB) | `just spec-chdb` executes emitted SQL against chDB     | `chdb`         | Informational         |
| `schema/ddl` integration     | `just schema-ddl-test` (testcontainers ClickHouse)     | `integration`  | Informational         |
| `prometheus/compliance`      | `just compatibility` (Docker Compose harness)          | none           | Required at M6        |
| End-to-end smoke             | `just e2e-up && e2e-seed && e2e-run` (k3d + Grafana)   | none           | Informational         |

## TXTAR spec round-trip

The TXTAR text-equality layer (`test/spec/<head>/<name>.txtar`)
catches every change in the emitted SQL. It does NOT catch
*semantic* regressions where the SQL still parses but its result set
flips. The chDB-backed round-trip layer closes that gap.

Authors opt a fixture into round-trip execution by adding two
optional sections:

```text
-- seed --
CREATE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = Memory;
INSERT INTO otel_metrics_gauge VALUES
    ('temperature', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), -3.5);
-- expected_rows --
[
  ["temperature", {"host": "a"}, "2026-01-01T00:00:00Z", 3.5]
]
```

The `chdb` build-tagged runner (in `test/spec/runner_chdb.go`) opens
an ephemeral in-process chDB session per fixture, applies `seed:`,
executes the fixture's `sql:` + `args:` against it, and asserts the
resulting rows match `expected_rows:`. Without the `chdb` build tag
both sections are inert — `just test` skips them.

### Determinism

The runner does NOT sort the result set. Authors must guarantee
deterministic row order via the seed's `INSERT` ordering combined
with the emitted SQL's `ORDER BY` (or, for single-row fixtures,
trivially). When seeding multiple rows for queries that lack an
ORDER BY, add one to the `seed:` table's `ORDER BY` clause and rely
on the engine's read order.

### Map columns

chdb-go v1.11.0's parquet wire driver panics on the parquet MAP
logical type when `rows.Scan` reaches a `Map(String, String)`
column. The R8.0 chDB probe
(`internal/chclient/chdb_probe_test.go`) confirmed that wrapping the
projection server-side in `toJSONString(...)` and JSON-decoding the
resulting string on the Go side is a clean shim.

The round-trip runner applies this transform automatically: any
top-level SELECT projection whose alias is one of `Attributes`,
`ResourceAttributes`, `ScopeAttributes`, or `SpanAttributes` is
rewritten to a `toJSONString(<expr>) AS <alias>` form (with the
alias backtick-quoted in the actual emitted SQL). Fixture authors
write `expected_rows:` with the Map column as a JSON object (for
example, `{"host": "a"}`); `reflect.DeepEqual` handles JSON key
ordering for free.

### chdb-go quirks

The runner ships a couple of small shims around chdb-go v1.11.0
behavior:

- `rows.Err()` returns `fmt.Errorf("empty row")` at end-of-iteration
  instead of `io.EOF`. The runner's `tolerantRowsErr` ignores that
  exact string and surfaces every other error.

- `sql.Open("chdb", "")` creates a temp-dir-backed session that is
  torn down with the connection. There is no `:memory:` literal in
  the driver.

- `rows.ColumnTypes()` panics on Map columns. The runner sizes its
  scan-target slice by parsing the outer SELECT's projection list
  textually — never call `ColumnTypes` from chdb code.

- Only the `PARQUET` driver wire format is supported; reach for
  parquet-go types, not Arrow.

### CI

The `chdb` workflow (`.github/workflows/chdb.yml`) runs nightly +
manual dispatch only. It first runs `TestChDBProbe` from R8.0, then
`just spec-chdb` to exercise the round-trip suite. Promote to a
required PR gate once the suite is consistently green and the pilot
fixture coverage has grown beyond the initial 5.
