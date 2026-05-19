# Tempo / TraceQL compliance harness — adoption plan

Cerberus's `harness/compatibility/` runs `prometheus/compliance` against a reference Prometheus to verify PromQL/Prom-API parity. This doc plans the TraceQL equivalent. Unlike Prom and Loki, **there's no upstream "tempo-compliance" repo** — the closest analogue is `tempo-vulture`. The plan forks vulture's seeder pattern into a cerberus-owned diff driver.

## Landscape

### Official Grafana-maintained artefacts (no dedicated suite)

| Artefact                                  | Path                                                                                                                                | Shape                                                                                                                                                                                                                                                                                                                                                                                         | Useful as compliance corpus?                                                                                                                                                                          |
| ----------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **`pkg/traceql/test_examples.yaml`**      | [grafana/tempo:pkg/traceql/test_examples.yaml](https://github.com/grafana/tempo/blob/main/pkg/traceql/test_examples.yaml)           | YAML lists of `valid`, `parse_fails`, `validate_fails`, `unsupported` query *strings*. No expected results.                                                                                                                                                                                                                                                                                   | Parser-level only. Already covered by cerberus's `internal/traceql/parse_test.go`. Not behavioural.                                                                                                   |
| **`integration/api/*.go`**                | [grafana/tempo:integration/api](https://github.com/grafana/tempo/tree/main/integration/api)                                         | Black-box Go tests booting real Tempo, hitting `/api/v2/search/tags`, `/api/v2/search/tag/{n}/values`, `/api/search`, `/api/v2/traces/{id}`, etc. Assert `tempopb.*Response` shape via `require.Equal`.                                                                                                                                                                                       | **Strongest fit.** Procedurally generated Jaeger Thrift batches (`MakeThriftBatchWithSpanCountAttributeAndName`) — not portable corpus, but harness pattern + assertion shapes are directly reusable. |
| **`integration/api/query_range_test.go`** | [grafana/tempo:integration/api/query_range_test.go](https://github.com/grafana/tempo/blob/main/integration/api/query_range_test.go) | Exercises `/api/metrics/query_range`; validates via **internal consistency** (`avg = sum/count`, exemplar belongs to series, etc.). No external diff.                                                                                                                                                                                                                                         | Lifts the cross-check invariant idea; inputs random.                                                                                                                                                  |
| **`cmd/tempo-vulture/`**                  | [grafana/tempo:cmd/tempo-vulture](https://github.com/grafana/tempo/tree/main/cmd/tempo-vulture)                                     | Long-running canary. Pushes deterministic OTLP traces seeded from epoch, queries them back via [`httpclient.Client`](https://github.com/grafana/tempo/blob/main/pkg/httpclient/httpclient.go) (`QueryTrace`, `QueryTraceV2`, `SearchTraceQLWithRange`, `MetricsQueryRange`, `SearchWithRange`). Diffs trace round-trip via `reflect.DeepEqual` + `go-test/deep`. Exports Prometheus counters. | **Closest analogue to `prom-compliance-tester`.** Single-backend (write/read same Tempo), but trivially repointed at two backends.                                                                    |
| **`grafana/oats`**                        | [grafana/oats](https://github.com/grafana/oats)                                                                                     | YAML-declarative round-trip tests against the LGTM stack. TraceQL assertions: `traceql:`, `equals:`/`regexp:`/`attributes:`/`count:`.                                                                                                                                                                                                                                                         | Conformance against a *single* Tempo, not differential. Useful as smoke-test corpus source.                                                                                                           |

### Third-party / community

- **Jaeger `tracegen`** ([jaegertracing/jaeger:cmd/tracegen](https://github.com/jaegertracing/jaeger/tree/main/cmd/tracegen)) — pure load generator, no assertions. Seed source only.
- **`tracetest.io`** — trace-based testing of *applications* via collector. Wrong axis: tests instrumentation, not Tempo's API.
- **OpenTelemetry collector testbed** — covers OTLP ingest, not Tempo query.
- **No OTLP-Query conformance suite exists** — OTLP only standardises ingest. Query is vendor-specific.

### Closest analogues ranked

|                                              | Pros                                                                                                                                                                   | Cons                                                                                  |
| -------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------- |
| **A. `cmd/tempo-vulture`**                   | Deterministic seed + read-back + structured comparison + Prometheus counters. Repointing the read path at cerberus gives a differential driver in a few hundred lines. | Single-backend by design.                                                             |
| **B. `grafana/oats` yaml runner**            | Clean DSL.                                                                                                                                                             | Assertion model is "matched span has attribute X = Y", not byte-equal — less precise. |
| **C. `integration/api/api_test.go` harness** | In-process, no Compose, reuses our tempo-fork proto types.                                                                                                             | Weak for full HTTP/JSON wire diffing.                                                 |

**Recommendation: A.** Fork vulture's seeder pattern into a cerberus-owned Compose stack + diff driver. This mirrors how `harness/compatibility/` reuses `prometheus/compliance/promql/`.

## Proposed layout

```text
compatibility/tempo/
  docker-compose.yml              # tempo + cerberus + clickhouse + driver
  tempo-config.yaml               # reference Tempo (local block storage)
  scripts/run-tempo-compatibility.sh
  driver/                         # cerberus-owned diff driver, NOT a fork
    main.go
    seeder.go                     # OTLP push to BOTH tempo + cerberus
    corpus.go                     # TXTAR loader (reuse shadow/corpus.go format)
    differ.go                     # JSON-shape diff
    report.go
    corpus/
      smoke.txtar                 # 10-query smoke set
      coverage.txtar              # ~40 queries, one per TraceQL feature
  expected-failures.json
  upstream/                       # vendored vulture snapshot
    cmd/tempo-vulture/
    pkg/httpclient/
```

The `driver/` dir is **new code** in cerberus, not a fork — vulture is too tightly coupled to single-backend round-trip. Reuse `pkg/httpclient` (already in deps via the tempo fork) + trace-generation helpers from the integration suite.

## Docker Compose addition

```yaml
tempo:
  image: grafana/tempo:2.9.0          # pin to the tag matching go.mod tsouza/tempo
  command: ["-config.file=/etc/tempo.yaml"]
  ports: ["23200:3200", "24317:4317"]
  volumes: ["./tempo-config.yaml:/etc/tempo.yaml:ro"]

cerberus-tempo:
  image: cerberus:compatibility
  environment:
    CERBERUS_TEMPO_HTTP_ADDR: ":29092"
    CERBERUS_CH_ADDR: "clickhouse:9000"
  ports: ["29092:29092"]

tempo-compat-driver:
  build: { context: ./driver }
  depends_on: { tempo: { condition: service_healthy }, cerberus-tempo: ..., clickhouse: ... }
  command: ["-corpus", "/corpus/smoke.txtar", "-tempo", "http://tempo:3200", "-cerberus", "http://cerberus-tempo:29092", "-report", "/reports/diff.json"]
```

## Corpus source (priority order)

1. **Hand-translate** the 20+ cases in `harness/compatibility/shadow/traceql_shadow_test.go::traceqlInstantCases()` — they already cover every TraceQL category with hand-computed expected results. **Free corpus.**
2. Lift parse-only queries from `pkg/traceql/test_examples.yaml` (`valid` bucket) to widen attribute/intrinsic coverage.
3. Add `/api/metrics/query_range` cases from `integration/api/query_range_test.go` (rate / sum_over_time / quantile_over_time).
4. Add endpoint-only shape-conformance: `/api/echo`, `/api/search/tags?scope=resource`, `/api/v2/search/tags`, `/api/search/tag/service.name/values`, `/api/v2/search/tag/service.name/values`, `/api/traces/<id>`, `/api/v2/traces/<id>`.

## Diff strategy

Two layers, both mandatory:

1. **Wire-level**: byte-canonicalised JSON diff. Sort `Series[]` by label-set, sort label-maps, normalise float precision to 1e-9 relative epsilon (reuse `harness/compatibility/shadow/differ.go::Compare`). Trace IDs hashed-deterministic so different orderings of equal sets don't false-positive.
2. **Semantic** (for `/api/metrics/query_range` + `/api/search` only): also run internal-consistency checks from `query_range_test.go` (`avg ≡ sum/count`, exemplar in series). Catches cases where both backends are wrong but in different ways.

Intentional mismatches → `expected-failures.json` with comment, same as Prom harness.

## Endpoints exercised (priority order)

| Endpoint                              | RC alignment | Why                                                  |
| ------------------------------------- | ------------ | ---------------------------------------------------- |
| `GET /api/echo`                       | RC1          | Trivial liveness; Grafana datasource probes this.    |
| `GET /api/traces/{id}` (v1 + v2)      | RC1          | Foundational read path.                              |
| `GET /api/search?q=<TraceQL>`         | RC2          | TraceQL search — central conformance surface.        |
| `GET /api/v2/search/tags?scope=...`   | RC2          | Drives Grafana's tag autocomplete.                   |
| `GET /api/v2/search/tag/{n}/values`   | RC2          | Typed value response.                                |
| `GET /api/metrics/query_range`        | RC3          | TraceQL metrics; semantic-consistency layer applies. |
| `GET /api/metrics/query`              | RC3          | Instant variant.                                     |
| `GET /api/search/tags` (v1)           | RC1          | Backwards-compat shape.                              |
| `GET /api/search/tag/{n}/values` (v1) | RC1          | Backwards-compat shape.                              |

## Per-PR breakdown

1. **PR 1 (vendor)**: snapshot `cmd/tempo-vulture/` + `pkg/httpclient/` under `compatibility/tempo/upstream/`. No CI yet.
2. **PR 2 (compose)**: `docker-compose.yml`, `tempo-config.yaml`, `scripts/run-tempo-compatibility.sh`. Driver is a stub returning 0. Wires `.github/workflows/tempo-compatibility.yml` with `workflow_dispatch` + nightly only.
3. **PR 3 (seeder)**: implement `driver/seeder.go`: write same deterministic OTLP batch to both Tempo's `:4317` and cerberus's OTLP ingest. 30s replication wait. Smoke: `/api/traces/<id>` on both returns the same span count.
4. **PR 4 (read corpus + smoke diff)**: implement `driver/corpus.go` + `driver/differ.go`. Lift the 20 cases from `shadow/traceql_shadow_test.go`. CI runs nightly; markdown report as artifact.
5. **PR 5 (metrics endpoints)**: extend corpus with `/api/metrics/query_range` + `/api/metrics/query`. Wire semantic-consistency layer.
6. **PR 6 (tags + values)**: extend corpus with the four tag endpoints; v1/v2 shape conformance.
7. **PR 7 (gate)**: once nightly green for 7 consecutive runs, promote to required PR check.

## Open questions

1. **OTLP ingest into cerberus**. The plan assumes cerberus accepts OTLP-gRPC so we can seed identically into both backends. If cerberus is read-only (queries CH directly, ingestion is the OTel-CH exporter's job), need a third actor (OTel collector with two exporters) to fan out. **Dominant architectural unknown.**
2. **Tempo version pin**. `tsouza/tempo:cerberus-accessors` tracks upstream main with patches. Reference Tempo container must match the same tag, else parser changes cause spurious diffs. CI step: assert `tempo:image-tag == go.mod-tsouza-tempo-tag`.
3. **Trace-ID determinism**. OTLP batches have client-generated IDs; if cerberus's CH-side trace assembly hashes spans differently, `/api/traces/<id>` corpus needs a lookup-by-attributes step. Vulture's `info.ConstructTraceFromEpoch()` is prior art.
4. **`/api/v2/search` Accept-header matrix**. Tempo's v2 trace endpoint negotiates JSON / protobuf / `application/vnd.grafana.llm`. Decide: exercise all three or JSON only.
5. **Block storage vs WAL**. Tempo's integration tests run each case twice (WAL, then flushed block). Read paths differ. We'd need a `flush` step in the driver — non-trivial against an in-container Tempo. Defer.
6. **Live-store endpoints**. Tempo recently added "live store" reads. Doubles the matrix. Defer.
7. **OATs as follow-up gate**. Worth a small PR after PR 7 to wire `grafana/oats` as secondary smoke. Different shape (round-trip via collector + LGTM image), catches OTel-ingestion-path drift the differential driver won't see.

## Sources

- [grafana/tempo cmd/tempo-vulture/main.go](https://github.com/grafana/tempo/blob/main/cmd/tempo-vulture/main.go)
- [grafana/tempo integration/api](https://github.com/grafana/tempo/tree/main/integration/api)
- [grafana/tempo integration/api/query_range_test.go](https://github.com/grafana/tempo/blob/main/integration/api/query_range_test.go)
- [grafana/tempo pkg/traceql/test_examples.yaml](https://github.com/grafana/tempo/blob/main/pkg/traceql/test_examples.yaml)
- [grafana/tempo HTTP API reference](https://grafana.com/docs/tempo/latest/api_docs/)
- [grafana/oats README](https://github.com/grafana/oats)
- [tempo-vulture artifacthub](https://artifacthub.io/packages/helm/grafana/tempo-vulture)
- [prometheus/compliance](https://github.com/prometheus/compliance)
