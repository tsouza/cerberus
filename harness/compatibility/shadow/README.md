# Shadow-mode differential testing harness

> The CLI + diff machinery is in place. Oracle wiring is currently a
> noop stub; the in-process PromQL evaluator lives at
> `internal/promshim/local/` and can be plugged in here when the
> shadow-mode pipeline is promoted from informational to a required
> gate. See [`docs/roadmap.md` § RC3](../../../docs/roadmap.md).

## What "shadow mode" means

For every query in a corpus, the harness invokes **two** evaluators and diffs
the result vectors:

| Side       | What it is                                                        | When it's authoritative                      |
| ---------- | ----------------------------------------------------------------- | -------------------------------------------- |
| **native** | cerberus's normal pipeline (HTTP → parse → lower → optimize → CH) | Production-shaped path. Default truth.       |
| **oracle** | In-process PromQL evaluator over an in-memory sample set          | Reference truth (Prometheus's own evaluator) |

The diff catches regressions where the CH-backed path drifts from PromQL
semantics without having to spin up a full reference Prometheus + remote-write
seeder (which is what the sibling `harness/compatibility/` Docker Compose stack
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
| `3`  | Oracle unavailable when strategy requires it (`force-native`, `oracle-only`) |

## How it slots into `harness/compatibility/`

```text
harness/compatibility/
  docker-compose.yml         <-- existing: reference Prom + cerberus + CH + seeder
  scripts/run-compatibility.sh
  shadow/                    <-- this directory
    cmd/shadow/main.go         CLI entry point
    differ.go                  pure diff function
    corpus.go                  TXTAR corpus loader
    corpus/smoke.txt           5-query smoke corpus
```

The Docker Compose stack remains the heavyweight reference; shadow mode is the
in-process companion. They can share the corpus format in a follow-up,
but ship independently today.

## Usage

The harness expects a running cerberus reachable at `$CERBERUS_URL` (defaults
to `http://localhost:9090`):

```sh
just shadow-mode CORPUS=harness/compatibility/shadow/corpus/smoke.txt STRATEGY=prefer-native
```

Or invoke the binary directly:

```sh
go build -o bin/shadow ./harness/compatibility/shadow/cmd/shadow
CERBERUS_URL=http://localhost:9090 ./bin/shadow \
    --corpus harness/compatibility/shadow/corpus/smoke.txt \
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

## Oracle stub

The binary ships with a `noopOracle` that returns `OracleSkipped` for
every query. Under `prefer-native` this is fine — the native answer is
returned and the diff is recorded as "oracle skipped" (non-fatal).
Under `force-native` or `oracle-only`, the binary exits with code `3`
to make the missing dependency loud.

The wiring point is a single interface:

```go
type OracleProvider interface {
    Evaluate(ctx context.Context, q Query) (VectorResult, error)
}
```

Wiring `internal/promshim/local/` in here (`promshimlocal.New(...)`)
is the natural next step.

## Status

Native evaluator wiring is a real HTTP client; oracle is stubbed.
Workflow is `workflow_dispatch` only — it does not run on PRs or
nightly. Promote to a required gate once the oracle is wired.
