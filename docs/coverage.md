# Function & construct coverage

The single pre-adoption question for a drop-in gateway is: **does it support
the queries my dashboards and alerts already run?** This page answers it
construct-by-construct for all three heads — every PromQL function /
aggregation / operator, every LogQL stage / aggregation, every TraceQL
intrinsic / metrics-op — with an honest support status.

## How to read this

Cerberus parses each head with its **reference upstream parser**
(`prometheus/promql/parser`, `grafana/loki/v3/pkg/logql/syntax`,
`grafana/tempo/pkg/traceql`), so anything those parsers accept, cerberus
parses. The status below reflects whether cerberus's full
`parse → fold → lower → optimize → emit` pipeline turns a symbol into
ClickHouse SQL, measured against what the reference backend does with the
same query.

| Status                               | Meaning                                                                                                                                                                                                                                                                                                                |
| ------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Supported**                        | Cerberus lowers it and the reference backend accepts it too — exact parity. This is the overwhelming majority of the surface.                                                                                                                                                                                          |
| **Supported (experimental)**         | A symbol the upstream parser gates behind an experimental-functions flag. Cerberus enables that flag in its production parser (PromQL: `--enable-feature=promql-experimental-functions`), so these work out of the box. Treat them as you would on upstream — they are experimental *upstream*, not flaky in cerberus. |
| **Supported (cerberus extension)**   | Cerberus accepts a query the reference's default configuration rejects, and answers it correctly. Rare; called out explicitly.                                                                                                                                                                                         |
| **Rejected (parity with reference)** | Cerberus rejects it *and so does the reference* under the same configuration — a deliberate, parity-preserving gate, not a gap.                                                                                                                                                                                        |
| **Not yet supported**                | Cerberus rejects a symbol the reference accepts — a real coverage gap. There are currently **none** of these (see [Coverage at a glance](#coverage-at-a-glance)).                                                                                                                                                      |

This is the human-facing translation of cerberus's machine-readable
conformance **ledger** in [`test/surface-parity/`](../test/surface-parity/)
([Layer 6d](test-strategy.md#layer-6d--function-surface-parity-ledger) of the
test map). The ledger classifies each symbol four ways — `parity-accept`,
`parity-reject`, `wrong-reject`, `wrong-accept` — which is the right vocabulary
for the test ratchet but the wrong one for a reader: an experimental function
cerberus implements that a *flag-OFF* reference would reject reads as
"wrong-accept" in the ledger yet is genuinely **Supported (experimental)** for
a user. The table above performs that translation.

## Coverage at a glance

The numbers below are derived directly from
[`test/surface-parity/inventory.json`](../test/surface-parity/inventory.json),
the pinned ledger of every grammar symbol the three upstream parsers expose.

| Head      | Symbols probed | Supported (incl. experimental) | Intentionally rejected (parity) | Not yet supported |
| --------- | -------------- | ------------------------------ | ------------------------------- | ----------------- |
| PromQL    | 121            | 119                            | 2                               | 0                 |
| LogQL     | 62             | 62                             | 0                               | 0                 |
| TraceQL   | 45             | 45                             | 0                               | 0                 |
| **Total** | **228**        | **226**                        | **2**                           | **0**             |

> **This is an accept/reject ledger, not a correctness score.** "226/228
> supported" means cerberus *lowers to SQL that the reference also accepts*
> for 226 of 228 grammar symbols. It does **not** mean those 226 symbols
> return numerically correct rows — that is what the
> [differential harnesses](compatibility.md) measure (an accepted symbol
> can still emit wrong rows). Cite this number as **surface / rejection
> parity**, never as numerical correctness coverage.

There are **zero** wrong-rejections across all three heads: cerberus lowers
every grammar symbol the reference backends accept. The two PromQL parity
rejections are the bare `start()` / `end()` query-context calls, which the
reference parser itself only admits inside an `@` modifier (`up @ start()`,
`up @ end()` — both **Supported**, listed under Modifiers) and rejects as
standalone expressions exactly as cerberus does.

### A note on PromQL `range()` / `step()`

These two query-context functions are experimental upstream and **Supported**
in cerberus: it constant-folds them per query exactly as the reference engine
does (`range()` → eval-range seconds, `step()` → the query_range step). They
appear as a `cerberus extension` in the raw ledger only because the
surface-parity probe drives them as bare top-level calls, a form the
flag-OFF HTTP reference rejects — an artifact of the probe shape, not a
correctness divergence. In real PromQL usage they match upstream.

## Per-symbol tables

The tables below are generated from the inventory; regenerate them after a
burndown that changes the surface with:

```sh
CERBERUS_UPDATE_INVENTORY=1 go test ./test/surface-parity/   # re-pin the ledger
python3 scripts/gen-coverage.py                              # re-render this doc
```

The `Probe` column is the exact query the ledger evaluates for that symbol.

<!-- BEGIN AUTOGEN: coverage-tables (scripts/gen-coverage.py) -->

### PromQL (121 symbols)

#### Aggregations

| Symbol         | Probe                   | Status    |
| -------------- | ----------------------- | --------- |
| `avg`          | `avg(up)`               | Supported |
| `bottomk`      | `bottomk(3, up)`        | Supported |
| `count`        | `count(up)`             | Supported |
| `count_values` | `count_values("v", up)` | Supported |
| `group`        | `group(up)`             | Supported |
| `limit_ratio`  | `limit_ratio(0.5, up)`  | Supported |
| `limitk`       | `limitk(3, up)`         | Supported |
| `max`          | `max(up)`               | Supported |
| `min`          | `min(up)`               | Supported |
| `quantile`     | `quantile(0.9, up)`     | Supported |
| `stddev`       | `stddev(up)`            | Supported |
| `stdvar`       | `stdvar(up)`            | Supported |
| `sum`          | `sum(up)`               | Supported |
| `topk`         | `topk(3, up)`           | Supported |

#### Functions

| Symbol                         | Probe                                                                            | Status                           |
| ------------------------------ | -------------------------------------------------------------------------------- | -------------------------------- |
| `abs`                          | `abs(up)`                                                                        | Supported                        |
| `absent`                       | `absent(up)`                                                                     | Supported                        |
| `absent_over_time`             | `absent_over_time(up[5m])`                                                       | Supported                        |
| `acos`                         | `acos(up)`                                                                       | Supported                        |
| `acosh`                        | `acosh(up)`                                                                      | Supported                        |
| `asin`                         | `asin(up)`                                                                       | Supported                        |
| `asinh`                        | `asinh(up)`                                                                      | Supported                        |
| `atan`                         | `atan(up)`                                                                       | Supported                        |
| `atanh`                        | `atanh(up)`                                                                      | Supported                        |
| `avg_over_time`                | `avg_over_time(up[5m])`                                                          | Supported                        |
| `ceil`                         | `ceil(up)`                                                                       | Supported                        |
| `changes`                      | `changes(http_server_request_duration_count[5m])`                                | Supported                        |
| `clamp`                        | `clamp(up, 0.5, 0.5)`                                                            | Supported                        |
| `clamp_max`                    | `clamp_max(up, 0.5)`                                                             | Supported                        |
| `clamp_min`                    | `clamp_min(up, 0.5)`                                                             | Supported                        |
| `cos`                          | `cos(up)`                                                                        | Supported                        |
| `cosh`                         | `cosh(up)`                                                                       | Supported                        |
| `count_over_time`              | `count_over_time(up[5m])`                                                        | Supported                        |
| `day_of_month`                 | `day_of_month(up)`                                                               | Supported                        |
| `day_of_week`                  | `day_of_week(up)`                                                                | Supported                        |
| `day_of_year`                  | `day_of_year(up)`                                                                | Supported                        |
| `days_in_month`                | `days_in_month(up)`                                                              | Supported                        |
| `deg`                          | `deg(up)`                                                                        | Supported                        |
| `delta`                        | `delta(http_server_request_duration_count[5m])`                                  | Supported                        |
| `deriv`                        | `deriv(http_server_request_duration_count[5m])`                                  | Supported                        |
| `double_exponential_smoothing` | `double_exponential_smoothing(http_server_request_duration_count[5m], 0.5, 0.5)` | Supported (experimental)         |
| `end`                          | `end()`                                                                          | Rejected (parity with reference) |
| `exp`                          | `exp(up)`                                                                        | Supported                        |
| `first_over_time`              | `first_over_time(up[5m])`                                                        | Supported (experimental)         |
| `floor`                        | `floor(up)`                                                                      | Supported                        |
| `histogram_avg`                | `histogram_avg(showcase_latency_exp_hist)`                                       | Supported                        |
| `histogram_count`              | `histogram_count(showcase_latency_exp_hist)`                                     | Supported                        |
| `histogram_fraction`           | `histogram_fraction(0, 1, showcase_latency_exp_hist)`                            | Supported                        |
| `histogram_quantile`           | `histogram_quantile(0.9, showcase_latency_exp_hist)`                             | Supported                        |
| `histogram_quantiles`          | `histogram_quantiles(showcase_latency_exp_hist, "q", 0.5, 0.9)`                  | Supported (experimental)         |
| `histogram_stddev`             | `histogram_stddev(showcase_latency_exp_hist)`                                    | Supported                        |
| `histogram_stdvar`             | `histogram_stdvar(showcase_latency_exp_hist)`                                    | Supported                        |
| `histogram_sum`                | `histogram_sum(showcase_latency_exp_hist)`                                       | Supported                        |
| `hour`                         | `hour(up)`                                                                       | Supported                        |
| `idelta`                       | `idelta(http_server_request_duration_count[5m])`                                 | Supported                        |
| `increase`                     | `increase(http_server_request_duration_count[5m])`                               | Supported                        |
| `info`                         | `info(http_server_request_duration_count)`                                       | Supported (experimental)         |
| `irate`                        | `irate(http_server_request_duration_count[5m])`                                  | Supported                        |
| `label_join`                   | `label_join(up, "dst", ",", "job")`                                              | Supported                        |
| `label_replace`                | `label_replace(up, "dst", "$1", "job", "(.*)")`                                  | Supported                        |
| `last_over_time`               | `last_over_time(up[5m])`                                                         | Supported                        |
| `ln`                           | `ln(up)`                                                                         | Supported                        |
| `log10`                        | `log10(up)`                                                                      | Supported                        |
| `log2`                         | `log2(up)`                                                                       | Supported                        |
| `mad_over_time`                | `mad_over_time(up[5m])`                                                          | Supported (experimental)         |
| `max_over_time`                | `max_over_time(up[5m])`                                                          | Supported                        |
| `min_over_time`                | `min_over_time(up[5m])`                                                          | Supported                        |
| `minute`                       | `minute(up)`                                                                     | Supported                        |
| `month`                        | `month(up)`                                                                      | Supported                        |
| `pi`                           | `pi()`                                                                           | Supported                        |
| `predict_linear`               | `predict_linear(http_server_request_duration_count[5m], 0.5)`                    | Supported                        |
| `present_over_time`            | `present_over_time(up[5m])`                                                      | Supported                        |
| `quantile_over_time`           | `quantile_over_time(0.5, up[5m])`                                                | Supported                        |
| `rad`                          | `rad(up)`                                                                        | Supported                        |
| `range`                        | `range()`                                                                        | Supported (experimental)         |
| `rate`                         | `rate(http_server_request_duration_count[5m])`                                   | Supported                        |
| `resets`                       | `resets(http_server_request_duration_count[5m])`                                 | Supported                        |
| `round`                        | `round(up, 0.5)`                                                                 | Supported                        |
| `scalar`                       | `scalar(up)`                                                                     | Supported                        |
| `sgn`                          | `sgn(up)`                                                                        | Supported                        |
| `sin`                          | `sin(up)`                                                                        | Supported                        |
| `sinh`                         | `sinh(up)`                                                                       | Supported                        |
| `sort`                         | `sort(up)`                                                                       | Supported                        |
| `sort_by_label`                | `sort_by_label(up, "s")`                                                         | Supported (experimental)         |
| `sort_by_label_desc`           | `sort_by_label_desc(up, "s")`                                                    | Supported (experimental)         |
| `sort_desc`                    | `sort_desc(up)`                                                                  | Supported                        |
| `sqrt`                         | `sqrt(up)`                                                                       | Supported                        |
| `start`                        | `start()`                                                                        | Rejected (parity with reference) |
| `stddev_over_time`             | `stddev_over_time(up[5m])`                                                       | Supported                        |
| `stdvar_over_time`             | `stdvar_over_time(up[5m])`                                                       | Supported                        |
| `step`                         | `step()`                                                                         | Supported (experimental)         |
| `sum_over_time`                | `sum_over_time(up[5m])`                                                          | Supported                        |
| `tan`                          | `tan(up)`                                                                        | Supported                        |
| `tanh`                         | `tanh(up)`                                                                       | Supported                        |
| `time`                         | `time()`                                                                         | Supported                        |
| `timestamp`                    | `timestamp(up)`                                                                  | Supported                        |
| `ts_of_first_over_time`        | `ts_of_first_over_time(up[5m])`                                                  | Supported (experimental)         |
| `ts_of_last_over_time`         | `ts_of_last_over_time(up[5m])`                                                   | Supported (experimental)         |
| `ts_of_max_over_time`          | `ts_of_max_over_time(up[5m])`                                                    | Supported (experimental)         |
| `ts_of_min_over_time`          | `ts_of_min_over_time(up[5m])`                                                    | Supported (experimental)         |
| `vector`                       | `vector(0.5)`                                                                    | Supported                        |
| `year`                         | `year(up)`                                                                       | Supported                        |

#### Binary operators

| Symbol   | Probe          | Status    |
| -------- | -------------- | --------- |
| `add`    | `up + up`      | Supported |
| `and`    | `up and up`    | Supported |
| `atan2`  | `up atan2 up`  | Supported |
| `div`    | `up / up`      | Supported |
| `eql`    | `up == up`     | Supported |
| `gte`    | `up >= up`     | Supported |
| `gtr`    | `up > up`      | Supported |
| `lss`    | `up < up`      | Supported |
| `lte`    | `up <= up`     | Supported |
| `mod`    | `up % up`      | Supported |
| `mul`    | `up * up`      | Supported |
| `neq`    | `up != up`     | Supported |
| `or`     | `up or up`     | Supported |
| `pow`    | `up ^ up`      | Supported |
| `sub`    | `up - up`      | Supported |
| `unless` | `up unless up` | Supported |

#### Modifiers

| Symbol     | Probe          | Status    |
| ---------- | -------------- | --------- |
| `at`       | `up @ 0`       | Supported |
| `at_end`   | `up @ end()`   | Supported |
| `at_start` | `up @ start()` | Supported |
| `offset`   | `up offset 5m` | Supported |

### LogQL (62 symbols)

#### Vector aggregations

| Symbol        | Probe                                                           | Status    |
| ------------- | --------------------------------------------------------------- | --------- |
| `approx_topk` | `approx_topk(3, count_over_time({service_name="gateway"}[5m]))` | Supported |
| `avg`         | `avg(count_over_time({service_name="gateway"}[5m]))`            | Supported |
| `bottomk`     | `bottomk(3, count_over_time({service_name="gateway"}[5m]))`     | Supported |
| `count`       | `count(count_over_time({service_name="gateway"}[5m]))`          | Supported |
| `max`         | `max(count_over_time({service_name="gateway"}[5m]))`            | Supported |
| `min`         | `min(count_over_time({service_name="gateway"}[5m]))`            | Supported |
| `sort`        | `sort(count_over_time({service_name="gateway"}[5m]))`           | Supported |
| `sort_desc`   | `sort_desc(count_over_time({service_name="gateway"}[5m]))`      | Supported |
| `stddev`      | `stddev(count_over_time({service_name="gateway"}[5m]))`         | Supported |
| `stdvar`      | `stdvar(count_over_time({service_name="gateway"}[5m]))`         | Supported |
| `sum`         | `sum(count_over_time({service_name="gateway"}[5m]))`            | Supported |
| `topk`        | `topk(3, count_over_time({service_name="gateway"}[5m]))`        | Supported |

#### Range aggregations

| Symbol               | Probe                                                                               | Status    |
| -------------------- | ----------------------------------------------------------------------------------- | --------- |
| `absent_over_time`   | `absent_over_time({service_name="gateway"}[5m])`                                    | Supported |
| `avg_over_time`      | `avg_over_time({service_name="gateway"} \| logfmt \| unwrap status [5m])`           | Supported |
| `bytes_over_time`    | `bytes_over_time({service_name="gateway"}[5m])`                                     | Supported |
| `bytes_rate`         | `bytes_rate({service_name="gateway"}[5m])`                                          | Supported |
| `count_over_time`    | `count_over_time({service_name="gateway"}[5m])`                                     | Supported |
| `first_over_time`    | `first_over_time({service_name="gateway"} \| logfmt \| unwrap status [5m])`         | Supported |
| `last_over_time`     | `last_over_time({service_name="gateway"} \| logfmt \| unwrap status [5m])`          | Supported |
| `max_over_time`      | `max_over_time({service_name="gateway"} \| logfmt \| unwrap status [5m])`           | Supported |
| `min_over_time`      | `min_over_time({service_name="gateway"} \| logfmt \| unwrap status [5m])`           | Supported |
| `quantile_over_time` | `quantile_over_time(0.9, {service_name="gateway"} \| logfmt \| unwrap status [5m])` | Supported |
| `rate`               | `rate({service_name="gateway"}[5m])`                                                | Supported |
| `rate_counter`       | `rate_counter({service_name="gateway"} \| logfmt \| unwrap status [5m])`            | Supported |
| `stddev_over_time`   | `stddev_over_time({service_name="gateway"} \| logfmt \| unwrap status [5m])`        | Supported |
| `stdvar_over_time`   | `stdvar_over_time({service_name="gateway"} \| logfmt \| unwrap status [5m])`        | Supported |
| `sum_over_time`      | `sum_over_time({service_name="gateway"} \| logfmt \| unwrap status [5m])`           | Supported |

#### Parser stages

| Symbol    | Probe                                                 | Status    |
| --------- | ----------------------------------------------------- | --------- |
| `json`    | `{service_name="shop"} \| json`                       | Supported |
| `logfmt`  | `{service_name="gateway"} \| logfmt`                  | Supported |
| `pattern` | `{service_name="proxy"} \| pattern "<method> <path>"` | Supported |
| `regexp`  | `{service_name="gateway"} \| regexp "(?P<lvl>\w+)"`   | Supported |
| `unpack`  | `{service_name="packer"} \| unpack`                   | Supported |

#### Label / format stages

| Symbol          | Probe                                                                                               | Status    |
| --------------- | --------------------------------------------------------------------------------------------------- | --------- |
| `decolorize`    | `{service_name="painter"} \| decolorize`                                                            | Supported |
| `drop`          | `{service_name="gateway"} \| logfmt \| drop level`                                                  | Supported |
| `keep`          | `{service_name="gateway"} \| logfmt \| keep level`                                                  | Supported |
| `label_format`  | `{service_name="gateway"} \| logfmt \| label_format lvl="{{.level}}"`                               | Supported |
| `label_replace` | `label_replace(count_over_time({service_name="gateway"}[5m]), "dst", "$1", "service_name", "(.*)")` | Supported |
| `line_format`   | `{service_name="gateway"} \| line_format "{{.status}}"`                                             | Supported |

#### Label filters

| Symbol  | Probe                                                                      | Status    |
| ------- | -------------------------------------------------------------------------- | --------- |
| `ip`    | `{service_name="gateway"} \| logfmt \| remote_addr = ip("192.168.0.0/16")` | Supported |
| `match` | `{service_name="gateway"} \| logfmt \| level = "error"`                    | Supported |

#### Line filters

| Symbol | Probe                                  | Status    |
| ------ | -------------------------------------- | --------- |
| `eq`   | `{service_name="gateway"} \|= "error"` | Supported |
| `neq`  | `{service_name="gateway"} != "error"`  | Supported |
| `nre`  | `{service_name="gateway"} !~ "err.*"`  | Supported |
| `re`   | `{service_name="gateway"} \|~ "err.*"` | Supported |

#### Conversion functions

| Symbol             | Probe                                                                                     | Status    |
| ------------------ | ----------------------------------------------------------------------------------------- | --------- |
| `bytes`            | `sum_over_time({service_name="gateway"} \| logfmt \| unwrap bytes(size) [5m])`            | Supported |
| `duration`         | `sum_over_time({service_name="gateway"} \| logfmt \| unwrap duration(took) [5m])`         | Supported |
| `duration_seconds` | `sum_over_time({service_name="gateway"} \| logfmt \| unwrap duration_seconds(took) [5m])` | Supported |

#### Binary operators

| Symbol   | Probe                                                                                                | Status    |
| -------- | ---------------------------------------------------------------------------------------------------- | --------- |
| `!=`     | `count_over_time({service_name="gateway"}[5m]) != count_over_time({service_name="gateway"}[5m])`     | Supported |
| `%`      | `count_over_time({service_name="gateway"}[5m]) % count_over_time({service_name="gateway"}[5m])`      | Supported |
| `*`      | `count_over_time({service_name="gateway"}[5m]) * count_over_time({service_name="gateway"}[5m])`      | Supported |
| `+`      | `count_over_time({service_name="gateway"}[5m]) + count_over_time({service_name="gateway"}[5m])`      | Supported |
| `-`      | `count_over_time({service_name="gateway"}[5m]) - count_over_time({service_name="gateway"}[5m])`      | Supported |
| `/`      | `count_over_time({service_name="gateway"}[5m]) / count_over_time({service_name="gateway"}[5m])`      | Supported |
| `<`      | `count_over_time({service_name="gateway"}[5m]) < count_over_time({service_name="gateway"}[5m])`      | Supported |
| `<=`     | `count_over_time({service_name="gateway"}[5m]) <= count_over_time({service_name="gateway"}[5m])`     | Supported |
| `==`     | `count_over_time({service_name="gateway"}[5m]) == count_over_time({service_name="gateway"}[5m])`     | Supported |
| `>`      | `count_over_time({service_name="gateway"}[5m]) > count_over_time({service_name="gateway"}[5m])`      | Supported |
| `>=`     | `count_over_time({service_name="gateway"}[5m]) >= count_over_time({service_name="gateway"}[5m])`     | Supported |
| `^`      | `count_over_time({service_name="gateway"}[5m]) ^ count_over_time({service_name="gateway"}[5m])`      | Supported |
| `and`    | `count_over_time({service_name="gateway"}[5m]) and count_over_time({service_name="gateway"}[5m])`    | Supported |
| `or`     | `count_over_time({service_name="gateway"}[5m]) or count_over_time({service_name="gateway"}[5m])`     | Supported |
| `unless` | `count_over_time({service_name="gateway"}[5m]) unless count_over_time({service_name="gateway"}[5m])` | Supported |

### TraceQL (45 symbols)

#### Aggregates

| Symbol  | Probe                                                          | Status    |
| ------- | -------------------------------------------------------------- | --------- |
| `avg`   | `{ resource.service.name = "gateway" } \| avg(duration) > 1ms` | Supported |
| `count` | `{ resource.service.name = "gateway" } \| count() > 0`         | Supported |
| `max`   | `{ resource.service.name = "gateway" } \| max(duration) > 1ms` | Supported |
| `min`   | `{ resource.service.name = "gateway" } \| min(duration) > 1ms` | Supported |
| `sum`   | `{ resource.service.name = "gateway" } \| sum(duration) > 1ms` | Supported |

#### Intrinsics

| Symbol                    | Probe                                                   | Status    |
| ------------------------- | ------------------------------------------------------- | --------- |
| `duration`                | `{ duration > 1ms }`                                    | Supported |
| `event:name`              | `{ event:name = "exception" }`                          | Supported |
| `event:timeSinceStart`    | `{ event:timeSinceStart > 1ms }`                        | Supported |
| `instrumentation:name`    | `{ instrumentation:name = "showcase-instrumentation" }` | Supported |
| `instrumentation:version` | `{ instrumentation:version = "1.2.3" }`                 | Supported |
| `kind`                    | `{ kind = server }`                                     | Supported |
| `link:spanID`             | `{ link:spanID != "" }`                                 | Supported |
| `link:traceID`            | `{ link:traceID != "" }`                                | Supported |
| `name`                    | `{ name = "charge" }`                                   | Supported |
| `nestedSetLeft`           | `{ nestedSetLeft > 0 }`                                 | Supported |
| `nestedSetParent`         | `{ nestedSetParent > 0 }`                               | Supported |
| `nestedSetRight`          | `{ nestedSetRight > 0 }`                                | Supported |
| `rootName`                | `{ rootName = "request" }`                              | Supported |
| `rootServiceName`         | `{ rootServiceName = "gateway" }`                       | Supported |
| `span:childCount`         | `{ span:childCount > 0 }`                               | Supported |
| `span:duration`           | `{ span:duration > 1ms }`                               | Supported |
| `span:id`                 | `{ span:id != "" }`                                     | Supported |
| `span:kind`               | `{ span:kind = server }`                                | Supported |
| `span:name`               | `{ span:name = "charge" }`                              | Supported |
| `span:parentID`           | `{ span:parentID != "" }`                               | Supported |
| `span:status`             | `{ span:status = error }`                               | Supported |
| `span:statusMessage`      | `{ span:statusMessage = "card declined" }`              | Supported |
| `status`                  | `{ status = error }`                                    | Supported |
| `statusMessage`           | `{ statusMessage = "card declined" }`                   | Supported |
| `trace:duration`          | `{ trace:duration > 1ms }`                              | Supported |
| `trace:id`                | `{ trace:id != "" }`                                    | Supported |
| `trace:rootName`          | `{ trace:rootName = "request" }`                        | Supported |
| `trace:rootService`       | `{ trace:rootService = "gateway" }`                     | Supported |
| `traceDuration`           | `{ traceDuration > 1ms }`                               | Supported |

#### Metrics operators

| Symbol                | Probe                                                                        | Status    |
| --------------------- | ---------------------------------------------------------------------------- | --------- |
| `avg_over_time`       | `{ resource.service.name = "gateway" } \| avg_over_time(duration)`           | Supported |
| `bottomk`             | `{ resource.service.name = "gateway" } \| rate() by (name) \| bottomk(3)`    | Supported |
| `compare`             | `{ resource.service.name = "gateway" } \| compare({ status = error })`       | Supported |
| `count_over_time`     | `{ resource.service.name = "gateway" } \| count_over_time()`                 | Supported |
| `histogram_over_time` | `{ resource.service.name = "gateway" } \| histogram_over_time(duration)`     | Supported |
| `max_over_time`       | `{ resource.service.name = "gateway" } \| max_over_time(duration)`           | Supported |
| `min_over_time`       | `{ resource.service.name = "gateway" } \| min_over_time(duration)`           | Supported |
| `quantile_over_time`  | `{ resource.service.name = "gateway" } \| quantile_over_time(duration, 0.9)` | Supported |
| `rate`                | `{ resource.service.name = "gateway" } \| rate()`                            | Supported |
| `sum_over_time`       | `{ resource.service.name = "gateway" } \| sum_over_time(duration)`           | Supported |
| `topk`                | `{ resource.service.name = "gateway" } \| rate() by (name) \| topk(3)`       | Supported |

<!-- END AUTOGEN: coverage-tables -->

## See also

- [`test-strategy.md`](test-strategy.md#layer-6d--function-surface-parity-ledger)
  — Layer 6d, the conformance ledger this page translates, and the CI ratchet
  that keeps it honest.
- [`compatibility.md`](compatibility.md) — the differential harnesses that
  prove an *accepted* query returns the right rows, not just that it parses.
- [`engine.md`](engine.md) — how a parsed query becomes ClickHouse SQL.
