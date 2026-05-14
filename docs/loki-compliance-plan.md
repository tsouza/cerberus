# Loki / LogQL compliance harness — adoption plan

Cerberus's `harness/compatibility/` runs `prometheus/compliance` against a reference Prometheus to verify PromQL/Prom-API parity. This doc plans the LogQL equivalent.

## Landscape

### Official Grafana-maintained suites

The closest analogue is **`grafana/loki:pkg/logql/bench/`** — not framed as "compliance" but functionally is one:

- **YAML query corpus** at [`pkg/logql/bench/queries/`](https://github.com/grafana/loki/tree/main/pkg/logql/bench/queries) split into `fast/`, `regression/`, `exhaustive/` subdirs. Schema validated by [`pkg/logql/bench/schema.json`](https://github.com/grafana/loki/blob/main/pkg/logql/bench/schema.json). Each entry: `description`, `query` (with `${SELECTOR}` / `${LABEL_NAME}` template vars), `kind` (`log` / `metric`), `time_range`, `directions`, `requires`, `tags`, `notes`.
- **`QueryRegistry` + variable expander** ([`pkg/logql/bench/query_registry.go`](https://github.com/grafana/loki/blob/main/pkg/logql/bench/query_registry.go)) — loads YAML, resolves templates from a `dataset_metadata.json`, expands to concrete queries.
- **`TestRemoteStorageEquality`** in [`pkg/logql/bench/remote_test.go`](https://github.com/grafana/loki/blob/main/pkg/logql/bench/remote_test.go) (~203 LOC, `//go:build remote_correctness`). Takes `-addr-1` + `-addr-2`, runs both instant + range queries, normalises (`sortVector` / `sortMatrix` / `sortStreams`), diffs with float tolerance (default `1e-5`). **Mechanically equivalent to `prometheus/compliance/promql-compliance-tester`** — same shape, same flags, same role.
- Companion **`discover` tool** ([`pkg/logql/bench/discover/cmd/discover`](https://github.com/grafana/loki/tree/main/pkg/logql/bench/discover)) introspects a live Loki to produce `dataset_metadata.json`. Cerberus seeds deterministically + commits a pinned `dataset_metadata.json` to bypass.

Other Grafana artefacts:
- **`grafana/loki:integration/`** — programmatic cluster builder + table tests. Not extractable as a wire-API corpus.
- **`pkg/logql/syntax/parser_test.go`** — parser conformance, not query/response.
- **`pkg/logql/engine_test.go`** — Go-typed expected outputs, not extractable.
- **`loki-canary`** — write/read integrity probe, not differential.

### Third-party / community

- **CNCF / OpenTelemetry contrib** — no Loki conformance suite.
- **`metrico/qryn-bench`** — k6 load/perf, not differential correctness.
- **`metrico/gigapipe` / `qryn`** — no public diff harness in tree.
- **No CNCF-housed `loki-conformance`** equivalent of `prometheus/compliance`.

### Closest analogues ranked

| Approach | Pros | Cons |
|---|---|---|
| **A. Vendor `pkg/logql/bench/` + `TestRemoteStorageEquality`** | Already maintained by Grafana; correct diff semantics; mirrors `prometheus/compliance` shape | Build-tagged; couples to upstream Go test driver; YAML templates need `dataset_metadata.json` |
| **B. Build cerberus-owned driver, reuse bench YAML** | Decouples from upstream test runner; consistent with how cerberus ships the Prom harness | Owning diff logic = more code; AGPL drag of YAML lift |
| **C. Roll fresh corpus from cerberus's TXTAR fixtures** | Self-contained, MIT-clean | Forfeits Grafana's curation; reinvents what they already maintain |

**Recommendation: A → B over time.** Start by vendoring upstream verbatim (mirror how `harness/compatibility/upstream/promql/` is set up); migrate the diff driver to cerberus-owned later for report-format consistency.

## Proposed layout

```text
harness/loki-compatibility/
  README.md
  docker-compose.yml                  # clickhouse + loki (reference) + cerberus
  test-cerberus.yml                   # endpoint config (addr-1=loki, addr-2=cerberus)
  cerberus-test-queries.yml           # curated subset, header documents drops
  expected-failures.json              # documented semantic gaps (initially empty)
  loki-config.yaml                    # reference Loki single-binary config
  scripts/
    run-loki-compatibility.sh
  cmd/
    seed/                             # OTel-shape log seeder for both targets
    discover/                         # dataset_metadata.json generator
  upstream/
    loki-bench/                       # sparse subtree of grafana/loki:pkg/logql/bench
      queries/                        # YAML corpus
      schema.json
      query_registry.go
      remote_test.go
      LICENSE                         # AGPL notice (isolated to this dir)
```

## Docker Compose addition

```yaml
loki:
  image: grafana/loki:3.7.0
  command: ['-config.file=/etc/loki/loki-config.yaml']
  volumes: ['./loki-config.yaml:/etc/loki/loki-config.yaml:ro']
  ports: ['23100:3100']
  healthcheck:
    test: ['CMD-SHELL', 'wget -qO- http://localhost:3100/ready']
    interval: 5s
    retries: 30
```

Cerberus already exposes `internal/api/loki/` + has TXTAR fixtures at `test/spec/logql/`. The cerberus container just needs a free port for the LogQL API.

## Diff strategy

- **PR 1 path**: reuse upstream `TestRemoteStorageEquality` verbatim. Vendor `remote_test.go` under `harness/loki-compatibility/upstream/loki-bench/`, build with `-tags=remote_correctness`. Pass `-addr-1=http://loki:3100` (reference) + `-addr-2=http://cerberus:29092` (test). Normalisers (`sortVector` / `sortMatrix` / `sortStreams`) + `1e-5` tolerance.
- **PR 5 path** (later): cerberus-owned driver under `harness/loki-compatibility/cmd/loki-compliance-tester/` so the report shape matches the Prom report.

## Endpoints exercised

Initially: `/loki/api/v1/query` + `/query_range`. PR 4 expands to `/labels`, `/label/<name>/values`, `/series`, `/index/volume`. `/tail` (WebSocket) + `/detected_fields` deferred until cerberus implements them.

## Per-PR breakdown

1. **PR 1 — scaffold** (compose + reference Loki + cerberus + seed). No corpus yet. `docker compose up --wait` brings reference Loki up at `:23100` and cerberus at `:29092`. Seeder pushes 5-10 minutes of deterministic log streams to both endpoints. Smoke: `/labels` non-empty on both.
2. **PR 2 — vendor `pkg/logql/bench/`** subtree + minimal corpus. Pull `queries/fast/*.yaml`, `schema.json`, `query_registry.go`, `remote_test.go`. Add `cerberus-test-queries.yml` overlay documenting drops for unsupported LogQL features per `docs/roadmap.md`. Pinned `dataset_metadata.json`.
3. **PR 3 — `scripts/run-loki-compatibility.sh` + just recipe.** Build the test driver (`go test -tags=remote_correctness -c`), point at the two endpoints, write report. Add `just loki-compatibility`.
4. **PR 4 — informational lane** in `.github/workflows/loki-compatibility.yml`. Trigger: `push: main` + nightly cron + `workflow_dispatch`. Report uploaded as artifact. Initially NOT a required check (matches Prom precedent).
5. **PR 5 (later) — cerberus-owned driver** replacing the Go test entry. Port `TestRemoteStorageEquality`'s diff loop into `cmd/loki-compliance-tester/` so it emits the same JSON shape as the Prom tester. Add `just compatibility-all`.
6. **PR 6 — expand corpus.** Add `queries/regression/` + slices of `queries/exhaustive/` as cerberus's LogQL coverage grows. Track in `cerberus-test-queries.yml` header.

## Open questions

- **License**: Grafana Loki is **AGPLv3**. Vendoring `pkg/logql/bench/queries/*.yaml` + `remote_test.go` extends AGPL into cerberus's test corpus path. Mitigation: isolate AGPL files under `harness/loki-compatibility/upstream/loki-bench/` with upstream `LICENSE` preserved (same posture as `harness/compatibility/upstream/promql/`). Reviewer: confirm cerberus's main `LICENSE` doesn't cover the harness `upstream/` dir.
- **Template-variable resolution**: `${SELECTOR}` / `${LABEL_NAME}` / `${LABEL_VALUE}` need a `dataset_metadata.json` mapped to the seeded fixture. Pre-discover locally; commit the JSON.
- **Loki version pin**: pick `grafana/loki:X.Y.Z`. The parser fork `tsouza/loki:v3.0.0-cerberus-parser` is syntax source-of-truth; runtime container should match the major.
- **`should_fail` semantics**: Prom corpus has this; Loki bench doesn't. PR 6 may need a cerberus-side flag for queries expected to fail-fast.
- **Range-type granularity**: `-remote-range-type=instant|range`. Both should run in CI.
- **Existing TXTAR coverage** in `test/spec/logql/` is unit-shaped (golden tests); the new harness is integration-shaped (live diff). Complementary per `docs/test-strategy.md`'s layer map.

## Sources

- [prometheus/compliance promql README](https://github.com/prometheus/compliance/tree/main/promql)
- [grafana/loki:pkg/logql/bench](https://github.com/grafana/loki/tree/main/pkg/logql/bench)
- [pkg/logql/bench/queries/fast/basic-selectors.yaml](https://github.com/grafana/loki/blob/main/pkg/logql/bench/queries/fast/basic-selectors.yaml)
- [pkg/logql/bench/remote_test.go](https://github.com/grafana/loki/blob/main/pkg/logql/bench/remote_test.go)
- [pkg/logql/bench/query_registry.go](https://github.com/grafana/loki/blob/main/pkg/logql/bench/query_registry.go)
- [grafana/loki AGPLv3 LICENSE](https://github.com/grafana/loki/blob/main/LICENSE)
