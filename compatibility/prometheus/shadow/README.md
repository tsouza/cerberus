# Shadow-mode differential testing harness

> The CLI + diff machinery is in place and the in-process PromQL oracle
> (`internal/promshim/local/`) is wired into the CLI via `oracle.go` in
> the same directory as `cmd/shadow/main.go`. The workflow at
> `.github/workflows/shadow-mode.yml` runs nightly + on pushes to main
> touching the differential-testing paths.

## What "shadow mode" means

For every query in a corpus, the harness invokes **two** evaluators and diffs
the result vectors:

| Side       | What it is                                                        | When it's authoritative                      |
| ---------- | ----------------------------------------------------------------- | -------------------------------------------- |
| **native** | cerberus's normal pipeline (HTTP → parse → lower → optimize → CH) | Production-shaped path. Default truth.       |
| **oracle** | In-process PromQL evaluator over an in-memory sample set          | Reference truth (Prometheus's own evaluator) |

The diff catches regressions where the CH-backed path drifts from PromQL
semantics without having to spin up a full reference Prometheus + remote-write
seeder (which is what the sibling `compatibility/prometheus/` Docker Compose stack
already does). Shadow mode is the lighter-weight, faster-feedback companion
that runs without containers.

## Strategies

The CLI takes `--strategy`:

| Strategy        | Behaviour                                                        | Use case                                                        |
| --------------- | ---------------------------------------------------------------- | --------------------------------------------------------------- |
| `prefer-native` | Run both; return native; record diff; non-fatal on disagreement  | **Default.** CI baseline; lets you measure the gap over time.   |
| `force-native`  | Run both; return native; **fail** on diff                        | Pre-release gate. Use when the oracle is trusted to be correct. |
| `oracle-only`   | Run only the oracle; native is skipped                           | Debugging the oracle; isolating semantic-vs-emitter bugs.       |

## Diff algorithm

`differ.Diff` compares two `VectorResult`s by:

1. **Cardinality**: number of series in each side.
2. **Label-set match**: each native series is paired with its label-equal oracle series.
3. **Point-wise value equality** with a configurable epsilon (default `1e-9`,
   relative for non-zero values, absolute for zero).
4. **Timestamp alignment**: timestamps must match exactly (no resampling).

Any mismatch yields a structured `Diff` record with the offending series and a
short reason string. The CLI prints one line per query and a JSON summary at
the end.

## Exit codes

| Code | Meaning                                                                      |
| ---- | ---------------------------------------------------------------------------- |
| `0`  | All queries agree (or strategy is `prefer-native` with diffs present)        |
| `1`  | One or more diffs under `force-native` strategy                              |
| `2`  | Setup failure (corpus unreadable, cerberus unreachable, etc.)                |

## How it slots into `compatibility/prometheus/`

```text
compatibility/prometheus/
  docker-compose.yml         <-- existing: reference Prom + cerberus + CH + seeder
  scripts/run-compatibility.sh
  shadow/                    <-- this directory
    cmd/shadow/main.go         CLI entry point
    cmd/shadow/oracle.go       in-process PromQL oracle (promshim/local wrapper)
    differ.go                  pure diff function
    corpus.go                  TXTAR corpus loader
    result_adapter.go          local.Result → VectorResult bridge
    corpus/smoke.txt           7-query smoke corpus
```

The Docker Compose stack remains the heavyweight reference; shadow mode is the
in-process companion. They can share the corpus format in a follow-up,
but ship independently today.

## Usage

The harness expects a running cerberus reachable at `$CERBERUS_URL` (defaults
to `http://localhost:9090`):

```sh
just shadow-mode CORPUS=compatibility/prometheus/shadow/corpus/smoke.txt STRATEGY=prefer-native
```

Or invoke the binary directly:

```sh
go build -o bin/shadow ./compatibility/prometheus/shadow/cmd/shadow
CERBERUS_URL=http://localhost:9090 ./bin/shadow \
    --corpus compatibility/prometheus/shadow/corpus/smoke.txt \
    --strategy prefer-native \
    --report shadow-report.json
```

If `CERBERUS_URL` is unset and the strategy requires the native side, the
harness exits with code `2` and a clear error.

## Corpus format

TXTAR file with two sections per query:

```text
-- query --
rate(http_requests_total[5m])
-- expected_strategy --
prefer-native
```

`expected_strategy` is optional and overrides the CLI flag per-query (used to
mark known-divergent queries that should stay `prefer-native` even when CI
flips the global flag to `force-native`).

## Oracle wiring

The CLI builds a `localOracle` (see `cmd/shadow/oracle.go`) that wraps
the `internal/promshim/local` engine over a deterministic in-memory
SampleStore. The store is seeded with `http_requests_total` counters,
`up` and `node_load1` gauges, and `http_request_duration_seconds_bucket`
classic-histogram buckets — enough surface for the smoke corpus to
return non-trivial vectors. When `--at` is unset the CLI evaluates at
the seeded dataset's epoch + 5 minutes so rate() and friends have
enough samples.

The `OracleProvider` interface remains a seam so alternate oracles
(e.g. a from-scratch PromQL evaluator) can replace the wired one
without touching the CLI loop:

```go
type OracleProvider interface {
    Evaluate(ctx context.Context, expr string) (VectorResult, error)
}
```

## Status

Both sides are wired: the native side speaks HTTP to cerberus, the
oracle side evaluates in-process via `internal/promshim/local`. The
workflow runs nightly + on push-to-main on differential-testing paths;
the default strategy is `force-native` so any diff between cerberus
and the reference engine fails the workflow.
