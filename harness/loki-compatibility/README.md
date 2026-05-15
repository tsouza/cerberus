# LogQL compatibility harness

Mirrors [`harness/prometheus-compliance/`](../prometheus-compliance/) for the
LogQL side. Stands up reference Grafana Loki and cerberus side-by-side,
seeds both with the same deterministic log stream, and (in later PRs)
diffs query results between the two.

This directory currently contains **PR 1 of 6**: the scaffold.

## What's here (PR 1)

```text
harness/loki-compatibility/
  README.md                            this file
  docker-compose.yml                   clickhouse + loki (reference) + cerberus
  loki-config.yaml                     single-binary Loki, filesystem backend
  cmd/seed/                            deterministic CH + Loki dual-write seeder
  scripts/run-loki-compatibility.sh    compose up --wait, run seeder, tear down
```

## What's deferred

Per [`docs/loki-compliance-plan.md`](../../docs/loki-compliance-plan.md):

- **PR 2** vendors `grafana/loki:pkg/logql/bench/` under `upstream/loki-bench/`
  and adds `cerberus-test-queries.yml` (curated YAML corpus overlay) and
  `dataset_metadata.json`.
- **PR 3** ports the upstream `TestRemoteStorageEquality` diff driver +
  wires `scripts/run-loki-compatibility.sh` to invoke it (initially just
  builds + runs the diff loop against the seeded fixture).
- **PR 4** adds the `.github/workflows/loki-compatibility.yml` informational
  CI lane (push-to-main + nightly + manual dispatch).
- **PR 5** ports the diff driver to cerberus-owned code under
  `cmd/loki-compliance-tester/` for report-format parity with the Prom
  harness.
- **PR 6** expands the corpus into `queries/regression/` + slices of
  `queries/exhaustive/`.

## Usage

From the repo root:

```sh
just loki-compatibility            # compose up, seed, smoke /labels, tear down
just loki-compatibility-keep       # same, but leave the stack running
just loki-compatibility-down       # tear down a stack left running by -keep
```

Or directly:

```sh
./harness/loki-compatibility/scripts/run-loki-compatibility.sh
```

## Endpoints (when the stack is up)

| Service    | Host:port                | Role                                    |
| ---------- | ------------------------ | --------------------------------------- |
| ClickHouse | `localhost:28000` (TCP)  | OTel-schema, `otel_logs`                |
|            | `localhost:28223` (HTTP) | same                                    |
| Loki       | `localhost:23100`        | reference target                        |
| cerberus   | `localhost:29092`        | test target (LogQL on `/loki/api/v1/*`) |

Port allocation is offset from the prometheus-compliance harness so the
two stacks can coexist on the same dev host.

## Seeder

`cmd/seed/` writes the same deterministic fixture to both targets:

- **ClickHouse** — `INSERT INTO otel_logs` with the column layout from
  the upstream OTel-CH Exporter DDL (`internal/schema/ddl`).
- **Loki** — HTTP POST to `/loki/api/v1/push` with `{streams:[...]}`.

Fixture shape: 4 services × 600 entries × 1 entry/sec = 10 minutes of
deterministic log data anchored at `2026-05-11T00:00:00Z`. Both writes
use the same anchor + same line bodies, so diff output in PR 3+ will
surface genuine LogQL semantic gaps, not data asymmetry.

Smoke contract: after the seeder runs, `GET /loki/api/v1/labels` MUST
return non-empty on BOTH `:23100` (reference Loki) and `:29092`
(cerberus), and `SELECT count() FROM otel_logs` on the CH side MUST
return > 0. The seeder polls each target and fails fast if any
condition isn't met within 30s.

The /labels probe passes a wide `[start, end]` window covering the
fixture anchor plus a buffer on each side — cerberus reads from CH
via `Timestamp BETWEEN ...`, so a window-less /labels call returns
empty. The wide window also absorbs the (system clock vs anchor)
skew that arises when CI runs days after the anchor date. PR 3's
diff driver will thread tighter per-query windows for the actual
result comparisons; the smoke just needs both endpoints alive.

## License

Cerberus itself is MIT. PR 2 will vendor an AGPLv3 corpus from
`grafana/loki:pkg/logql/bench/` under `upstream/loki-bench/` with the
upstream LICENSE preserved — same posture as
[`harness/prometheus-compliance/upstream/promql/`](../prometheus-compliance/upstream/promql/).
PR 1 vendors nothing, so the AGPL note only applies once PR 2 lands.
