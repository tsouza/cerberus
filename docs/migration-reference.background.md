# Migration reference — background

This document collects the design rationale behind
[`migration-reference.md`](migration-reference.md): why a mapping is what it
is, which upstream behaviour forced it, and which alternatives were rejected.
It answers "why is it built this way" rather than "what does it do" —
`migration-reference.md` is self-sufficient for using the `migrate` command
group correctly.

## The root flags the subcommands replaced

The legacy `migrate --schema` root flag is now the `schema` subcommand, and
the legacy `migrate --rules` root shorthand folded into `explain --rules`.

## Why `inventory`'s Loki ranking is operator-named

Loki exposes no whole-tenant top-N cardinality call the way Prometheus's TSDB
status endpoint does. The operator therefore names what to rank through
`--loki-selector` rather than the tool guessing at a set it has no API to
enumerate.

## Why Tempo has no cardinality section of its own

Tempo's span/block storage has no head-block or ranked-cardinality-stats API
analogous to either other head. `--tempo-source` records a fixed,
specifically-reasoned out-of-scope entry rather than a fabricated number.

## Why Loki `alert:` rules are never harvested into the rule graph

A LogQL alerting expr is a log-stream selector, not a metric-name reference,
so it can never consume a recorded series. Feeding it through the PromQL
extractor would only manufacture spurious unparseable-consumer skips.

## Why there is no `--tempo-rules` flag

Tempo has no rule concept in the sense `rulegraph` needs: its
metrics-generator is a fixed-shape, config-driven span-metric emitter with no
user-authored rule file for such a flag to point at.

## Why the replay constants are pinned rather than exposed as flags

The log-stream `limit` and direction and the trace-search `limit` and
spans-per-set decide how much of a result is truncated — that is, how much of
it the gate can judge. An operator knob would therefore silently change what
parity *means* between two runs, so the values are pinned constants recorded
in the `--report` artifact instead.

## Why a TraceQL `compare()` is out of scope

`compare()` selects its attribute inventory by a topN ranking neither
backend's wire contract specifies, so no definition of equality holds for it.
It is counted and reported `out_of_scope` with the reason, never silently
dropped and never guessed at.

## Why an unparseable consumer expression blocks in `rulegraph`

"Orphan implies safe to drop" is unsound once a consumer has itself been
dropped from the graph: the recorded series would look unread purely because
the expression that reads it could not be parsed.
