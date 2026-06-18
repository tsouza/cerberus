# Columnar vs row-scan result decode — benchmark + decision

**TL;DR: QUALIFIED GO — columnar pays, on a dedicated `ch-go` conn used
for the matrix path only ("when needed").** The row-scan cost the premise
targets is real and large (~47x slower, ~38,000x more allocs than columnar
on a 1M-row matrix). It is unreachable through the **public**
`clickhouse-go/v2 v2.46.0` API — but reachable cleanly via `ch-go`'s
columnar `proto.ColMap` (its `Keys`/`Values` are exported) on a SECOND
dial, while **reusing** the existing settings / query_id / breaker / retry
logic, which is conn-agnostic. This is a proportionate build, not the full
re-plumb an earlier draft of this doc claimed; it passes the GO bar. The
benchmark below is the evidence; the implementation is a follow-up.

## What the hot path does (and why columnar was attractive)

`rowsCursor.Next` (cursor.go) decodes a query_range matrix row-by-row via
`driver.Rows.Scan(&MetricName, &labels, &Timestamp, &Value)`. Inside
clickhouse-go, `(*rows).Scan` delegates to `scan(block, row, dest...)`
(scan.go:62), which loops the destinations and calls
`column.Interface.ScanRow(d, row)` per cell.

For the `Attributes Map(String,String)` column, `ScanRow` calls
`column.Map.row(i)` (map.go:304): a fresh `reflect.MakeMap` plus, per
entry, `col.keys.Row()` / `col.values.Row()` (each boxes a string into
`any` — an allocation), `reflect.ValueOf`, and a reflect `SetMapIndex`.

cerberus already **interns** label maps (cursor.go `internLabels`), so all
but the first decode of each series' map is immediately discarded as
garbage. The premise: decode the Map column **once per series** as typed
slices instead of `reflect.MakeMap`-ing it on every row.

The premise is correct. The per-row Map decode is ~31 allocs/row and
dominates the decode cost.

## The benchmark

`columnar_bench_test.go` builds a representative matrix block — `(MetricName
String, Attributes Map(String,String), TimeUnix DateTime, Value Float64)`
— as a real `*proto.Block` via the public `AddColumn`/`Append` API, then
decodes it three ways, all producing identical `[]Sample` (parity proven
by `TestColumnarParity`):

- **RowPath** — exactly what `rowsCursor.Next` does: per-row positional
  `column.ScanRow` (the production code path) + intern.
- **ColumnarPath** — the best achievable with the **pinned public API**:
  pull each column via `column.Interface.Row(i)`, decode the Map only via
  `Row(i)`, reuse interned instances.
- **ColumnarIdeal** — the *prize*: what a true columnar decode would buy
  if the Map sub-columns (`keys`/`values`/`offsets`) were reachable as
  typed slices. Modelled directly (they are unexported in
  clickhouse-go; ch-go's `proto.ColMap` DOES expose `Keys`/`Values`).

`go test -bench BenchmarkDecode -benchmem -benchtime=5x`
(Intel i7-10510U, go1.26.2):

| Path | shape | ns/op | B/op | allocs/op |
|---|---|---:|---:|---:|
| **RowPath** (prod) | 1s × 100k | 116,234,368 | 92,801,145 | 3,100,003 |
| **ColumnarPath** (public API) | 1s × 100k | 123,883,040 | 95,201,158 | 3,100,003 |
| **ColumnarIdeal** (what-if) | 1s × 100k | **4,445,910** | **7,201,686** | **11** |
| **RowPath** (prod) | 100s × 10k | 1,230,045,133 | 928,017,108 | 31,000,012 |
| **ColumnarPath** (public API) | 100s × 10k | 1,275,942,050 | 952,017,086 | 31,000,012 |
| **ColumnarIdeal** (what-if) | 100s × 10k | **23,407,235** | **72,072,254** | **812** |
| **RowPath** (prod) | 1000s × 1k | 1,224,529,046 | 928,168,043 | 31,000,023 |
| **ColumnarPath** (public API) | 1000s × 1k | 1,255,002,281 | 952,168,100 | 31,000,023 |
| **ColumnarIdeal** (what-if) | 1000s × 1k | **22,562,262** | **72,720,014** | **8,023** |

## Reading the numbers

1. **The prize is enormous.** Ideal columnar is **~47x faster**, **~13x
   less memory**, and **~38,000x fewer allocs** (31M -> 812 on 1M rows).
   The 31M allocs are almost entirely the per-row `reflect.MakeMap` + boxed
   `SetMapIndex` for the Attributes Map. The premise is validated.

2. **The prize is unreachable with the pinned deps.** ColumnarPath — the
   best you can do through the `clickhouse-go/v2 v2.46.0` *public* API — is
   **identical to (slightly worse than) RowPath**. Why: the only public
   accessor on `column.Map` is `Row(i) any`, and `Row(i)` *is* `col.row(i)`
   — the same `reflect.MakeMap`. The Map sub-columns are unexported. There
   is **no block/columnar read API** on `driver.Conn` at all: `Query`
   returns row-oriented `driver.Rows`; the columnar `*proto.Block` it holds
   (`clickhouse_rows.go:13`) is private, and the only column-level surface
   on the interface is INSERT-side (`Batch.Column`).

## Reaching the prize: a SECOND dial, not a second plumbing

To read `proto.ColMap.Keys`/`Values` you must use **ch-go** directly —
clickhouse-go uses ch-go's wire `proto` package but its own connection, so
the two do not share a socket. The instinct is "that means re-implementing
all of `internal/chclient`." It does **not**, and that is the correction
that flips this from NO-GO to GO.

The chclient plumbing is **conn-agnostic**: only five call sites touch
`c.conn` (`Query`, `Exec`, `Ping`, `Close`, the acquire-skip). Everything
that makes a query safe — the circuit breaker (`c.br.allow`/`record`),
broken-conn retry (`withTransportRetry`), per-query settings + query_id
(`queryContext`/`querySettings`), the sample budget, the execute span, the
progress recorder — **wraps around** the query call, operating on `ctx`
and the returned error/rows. None of it is coupled to the `driver.Conn`
type. And the dial config is the same vocabulary on both clients:

| chclient needs | clickhouse-go | ch-go |
|---|---|---|
| TLS / mTLS | `Options.TLS *tls.Config` | `ch.Options.TLS *tls.Config` |
| user / pass / db | `Options.Auth` | `ch.Options.User/Password/Database` |
| `max_memory_usage`, `max_execution_time` | `WithSettings` | `Query.Settings []Setting` |
| query_id | `WithQueryID` | `Query.QueryID` |
| progress | `WithProgress` | `Query.OnProgress` |
| streamed blocks | private `*proto.Block` | `Query.OnResult(ctx, block)` — **per block**, bounded memory, columnar |

So "on top of ch-go **when needed**" is a proportionate build:

- A dedicated ch-go conn (or a small `chpool`) built from the SAME
  `Config` (TLS/auth) chclient already parses — used ONLY for the matrix
  shape. The row path and every other head keep `driver.Conn` untouched.
- A `QueryColumnarMatrix` that wraps `client.Do` with the EXISTING breaker
  / retry / settings / query_id / recorder / budget — reused, not rebuilt.
- `OnResult` binds `proto.ColStr` (MetricName), `proto.ColMap[string,string]`
  (Attributes), `proto.ColDateTime` (TimeUnix), `proto.ColFloat64` (Value).
  Iterate the Map via its exported `Keys`/`Values` + offsets, build the
  label map ONCE per series (intern as today), stream `Sample`s.
- Behind a `CERBERUS_*` flag (default off / experimental until parity is
  exhaustively proven against the row path on prod ClickHouse).

This passes the GO bar: it **reuses** the settings/query_id/breaker plumbing
(the explicit criterion), keeps the row path for loki/tempo/labels, and is
reversible via the flag. `OnResult`'s per-block delivery preserves the
cursor's bounded-memory streaming contract.

## Decision

**GO, scoped to the matrix path, as a follow-up.** This PR lands the
benchmark + parity proof + this analysis so the win is quantified and the
chosen path is recorded. The implementation (`QueryColumnarMatrix` over a
dedicated ch-go conn, flag-gated, differential-tested for byte-identical
Samples) is a separate, larger change and ships next.

The honest cost note: it is more than a patch — a second dial, a columnar
cursor, and exhaustive parity testing (empty / single / multi-series /
NULLs / large) before the flag can default on. But the ~47x / 38,000x-allocs
prize on the hottest path in the gateway earns it, and the existing interim
wins (label-map interning, `SeriesID`) only reclaim the *retained-memory*
half — the per-row *decode allocation* is exactly what this captures.
