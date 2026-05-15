# Tempo / TraceQL compatibility harness

> Status: **PR 2 (compose + stub driver + nightly workflow)** of the
> rollout described in [`docs/tempo-compliance-plan.md`](../../docs/tempo-compliance-plan.md).
> This directory holds the vendored snapshot from PR 1 (`upstream/`),
> the Docker Compose stack standing up reference Tempo + cerberus +
> ClickHouse, a STUB driver that exits 0, and a nightly /
> `workflow_dispatch` GitHub Actions lane (informational, not a
> required check). The real seeder + diff driver follow in PRs 3-4.

## Why this harness exists

Cerberus already has `harness/prometheus-compliance/` (PromQL parity vs reference
Prometheus) and a sibling shadow-mode harness for in-process diffs. The
Tempo / TraceQL surface has **no upstream "tempo-compliance" repo** — the
closest analogue is `cmd/tempo-vulture`, Grafana's deterministic seed +
read-back canary. The plan forks vulture's seeder pattern into a
cerberus-owned diff driver, similar to how `harness/prometheus-compliance/`
reuses `prometheus/compliance/promql/`.

See [`docs/tempo-compliance-plan.md`](../../docs/tempo-compliance-plan.md)
for the full landscape analysis, the per-PR breakdown, and the diff
strategy.

## Layout (current — PR 2)

```text
harness/tempo-compatibility/
  README.md             this file
  docker-compose.yml    PR 2 — tempo + cerberus + clickhouse + stub driver
  tempo-config.yaml     PR 2 — reference Tempo (local block storage)
  scripts/
    run-tempo-compatibility.sh  PR 2 — `docker compose up --wait` + driver + teardown
  driver/                       PR 2 — STUB; PRs 3-4 wire the real seeder + differ
    Dockerfile          PR 2 — multi-stage build of the driver binary
    main.go             PR 2 — flag-only stub, prints banner + exits 0
  upstream/             PR 1 — vendored snapshot (read-only reference)
    LICENSE             AGPL-3.0, copied verbatim from grafana/tempo
    VERSION             exact upstream coordinates of the snapshot
    cmd/tempo-vulture/  long-running canary; reused as seeder pattern
    pkg/httpclient/     Tempo HTTP client; reused by the future driver
```

## Layout (planned — PRs 3-7)

PRs 3-7 grow the driver from stub into the real differ:

```text
harness/tempo-compatibility/
  driver/
    main.go             PR 3+ — orchestrator (parses flags, dispatches)
    seeder.go           PR 3  — push OTLP batches to tempo + cerberus
    corpus.go           PR 4  — TXTAR loader
    differ.go           PR 4  — JSON-shape diff
    report.go           PR 4  — markdown report writer
    corpus/
      smoke.txtar       PR 4  — 10-query smoke set
      coverage.txtar    PR 5  — ~40 queries, one per TraceQL feature
  expected-failures.json  PR 4+
```

## What's in `upstream/`

A pure, unmodified snapshot of two paths from `github.com/grafana/tempo`,
brought in via the `github.com/tsouza/tempo` fork that already gates
cerberus's tempo dependency (see `docs/upstream-forks.md`). The
cerberus-accessors branch only patches `pkg/traceql/`; the two paths
vendored here (`cmd/tempo-vulture/` and `pkg/httpclient/`) are
byte-identical between the fork tag and the upstream commit it tracks.

Exact coordinates live in [`upstream/VERSION`](upstream/VERSION).

### Why vendor, not import via `go.mod`?

`github.com/grafana/tempo` is already in `go.mod` via the replace
directive that points at `github.com/tsouza/tempo`, so we **could**
just import `pkg/httpclient` directly. We vendor instead because:

1. **PR 1 is reference material, not a build dependency.** The future
   driver (PR 3) will decide which subset to import, copy outright,
   or rewrite. Vendoring lets reviewers read the seeder pattern in
   the diff without grepping `~/go/pkg/mod/`.
2. **`cmd/tempo-vulture` is a `package main`**, not importable. Vendoring
   it keeps the source visible for the driver author to adapt.
3. **Snapshot stability.** A future bump of the tsouza/tempo replace
   tag won't silently move the seeder pattern under our feet; the
   snapshot here is pinned and explicit.

### Why the vendor isn't compiled

The vendor's imports (e.g. `github.com/go-test/deep`, `go.uber.org/zap`,
`github.com/jsternberg/zap-logfmt`, `github.com/grafana/tempo/integration/util`)
are not in cerberus's `go.mod` and would pull in heavy transitive deps
just to compile vulture's `main` package. Until the driver (PR 3)
actually imports a subset of this code, we exclude the vendor from
the module graph via a `go.mod` `ignore` directive:

```text
ignore ./harness/tempo-compatibility/upstream
```

The directive is a Go 1.25+ feature; cerberus already pins Go 1.26
via `go.mod`. With it in place:

- `go build ./...` and `go test ./...` skip the vendor entirely.
- `go vet ./...` skips the vendor entirely.
- The vendored sources remain on disk for reference + future PRs.

## Bump procedure

The vendor is **not** automatically refreshed when `go.mod`'s
`tsouza/tempo` tag bumps — the cerberus-accessors branch only patches
`pkg/traceql/`, so a fork-tag bump rarely touches vendored paths. Do a
manual re-snapshot only when:

1. Upstream Grafana adds a meaningful capability to `cmd/tempo-vulture`
   or `pkg/httpclient` that the cerberus driver wants to mirror, **or**
2. The cerberus driver (PR 3+) starts depending on a specific behaviour
   of the vendored code and we want a known-good baseline.

To re-snapshot:

```sh
# 1. Read the current replace target in go.mod.
grep '^replace github.com/grafana/tempo' go.mod
#    => replace github.com/grafana/tempo => github.com/tsouza/tempo vX.Y.Z

# 2. Clone the fork at that tag.
git clone --depth=1 -b vX.Y.Z https://github.com/tsouza/tempo /tmp/tempo-upstream

# 3. Wipe the existing snapshot and re-copy.
rm -rf harness/tempo-compatibility/upstream/{cmd,pkg,LICENSE}
mkdir -p harness/tempo-compatibility/upstream/{cmd,pkg}
cp -r /tmp/tempo-upstream/cmd/tempo-vulture  harness/tempo-compatibility/upstream/cmd/
cp -r /tmp/tempo-upstream/pkg/httpclient     harness/tempo-compatibility/upstream/pkg/
cp    /tmp/tempo-upstream/LICENSE            harness/tempo-compatibility/upstream/LICENSE

# 4. Update upstream/VERSION with the new fork tag + commit SHA and
#    the matching upstream/main base commit.
$EDITOR harness/tempo-compatibility/upstream/VERSION

# 5. PR the diff. Reviewer checks the bump procedure was followed
#    verbatim; no sanitisation of vendored sources is permitted.
```

## Licensing

`grafana/tempo` was relicensed from Apache-2.0 to **AGPL-3.0** in
[grafana/tempo#660](https://github.com/grafana/tempo/pull/660). The
vendored snapshot inherits AGPL-3.0, and [`upstream/LICENSE`](upstream/LICENSE)
is the verbatim copy.

Cerberus itself is independently licensed (see the repo root `LICENSE`);
the AGPL terms apply only to the vendored subtree under `upstream/`.
The driver code that lands in PRs 3+ will live OUTSIDE `upstream/`
(under `harness/tempo-compatibility/driver/`) and is cerberus-licensed.

## Related docs

- [`docs/tempo-compliance-plan.md`](../../docs/tempo-compliance-plan.md) — the rollout plan
- [`docs/upstream-forks.md`](../../docs/upstream-forks.md) — how the `tsouza/tempo` fork is wired
- [`harness/prometheus-compliance/`](../prometheus-compliance/) — sibling harness, the template this one mirrors
