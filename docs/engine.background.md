# Engine — background

This document collects the design rationale and rejected alternatives behind
[`engine.md`](engine.md). It answers "why is the pipeline shaped this way"
rather than "what does it do" — nothing here is required to add a head, wire
an extension point, or read a response header correctly.

## Why the shared IR is head-agnostic but not backend-neutral

The `chplan` algebra carries physical ClickHouse capability nodes
(`RangeWindowGridNative`, `RangeWindowStaleResample`) and a sealed function
vocabulary that resolves at the `chsql` boundary. Those nodes let lowering and
optimization select a concrete execution capability without leaking raw SQL
spellings into the plan. A different storage backend would need its own
emitter and would either implement or reject those physical capabilities
explicitly; keeping them in the IR is what lets one optimizer serve three
heads without the IR pretending to be portable.

## Why the rule set is shaped the way it is

`FilterAggregateTranspose` is retained as speculative correctness insurance
even though it fires 0 times on the current corpus: a lowering change that
emits `Filter(Aggregate)` would otherwise silently lose the pushdown.
`FilterRangeWindowTranspose`, `FilterFusion`, `ConstantFoldHeuristic`,
`ProjectionPushdown`, and `FlattenVectorSetOp` all fire on real queries.

`FlattenVectorSetOp` skips `unless` because it is not associative, so an
`unless` chain keeps its binary shape.

There is no `FilterProjectTranspose` because no lowering emits
`Filter(Project)` — the rule would never match — and no `MVSubstitution`
because the default schema ships no live rollups, so a substitution rule would
be a guaranteed no-op. Every rule in `Default()` either fires on the corpus or
is named insurance against a specific lowering shape.

## Why the scan time bound is an IR property

An instant windowed range aggregation (`rate` / `increase` / `*_over_time` /
…) reads per-sample rows out of MergeTree and `groupArray`s them per series at
the innermost level before the post-`groupArray` `arrayFilter` discards
out-of-window samples. If that innermost read carries no time predicate,
ClickHouse cannot prune granules and materialises the full per-series
retention — tens of millions of rows on a prod instant query. A bound that
lived only in the emitter would be easy to forget as new `groupArray`
emitters land, so it is an IR-level property instead: established once,
fail-closed by the analyzer batch, and rendered byte-identically by every
emitter.

## Why the native `timeSeries*ToGrid` family is pinned as a class

The native family bounds its innermost read through
`maybePushRangeScanTimeBound`, but that bound changes only the rows *read*
— the aggregate's own `(start, end, step, window)` parameters already
discard out-of-window samples — so dropping it produces no wrong answer and
no failing golden. Nothing value-level would catch the regression. The family
is therefore pinned as a class by
`internal/chsql/range_window_grid_native_scan_bound_test.go`, whose case list
is driven by the emitter's own `nativeTSGridFn` registry: a native aggregate
registered without a scan-bound case fails, as does one whose emitter drops
the predicate. The join case additionally pins that the bound is rendered per
operand, since each side of a vector-vector join is an independent scan.

## Why deferred label shaping uses `-State` / `-Merge`

The combinator pair — rather than arithmetic over two finished grids — is
what makes the three-level rewrite value-preserving. Label shaping is
many-to-one, so several raw series can carry one output identity, and their
samples must be pooled before the window function runs; merging partial
states pools samples, whereas combining finished grids computes a different
number and yields NULL wherever one contributor holds too few samples in the
window. `Recollapse` is consequently only populated for range functions whose
`-State`/`-Merge` pair is proven exact under merged states.

## Why eval-grid discovery is an interface, not a type switch

A consumer written as a type switch has to list every grid-bearing node, and
the failure mode when it misses one is silent: the walk finds no carrier, the
consumer reads a zero grid, and a zero grid is indistinguishable from a
genuine instant query — so a range query gets filed under the wrong
evaluation mode instead of raising an error. `chplan.GridCarrier` plus the
completeness ratchet closes the set in both directions, which is why `Step >
0` is the only range-vs-instant discriminator a consumer may branch on.

## Why the schema comes from the exporter fork

The DDL templates are consumed from the
`tsouza/opentelemetry-collector-contrib:cerberus-ddl` fork of the OTel-CH
exporter rather than written by hand, so a deployment where the exporter
writes and cerberus reads sees one schema across both sides. The fork exists
only to expose the templates; the layout itself is upstream's.

## Why `X-Cerberus-Inspected-Spans` is a separate header

`SearchMetrics.InspectedTraces` in the response body counts distinct traces
(upstream Tempo's semantics). The span-row count is a different quantity —
the resource-bound signal — so it gets a different name rather than
overloading the body field. The gRPC `StreamingQuerier.Search` RPC reports the
span count on an identically-named trailer so both transports answer the same
question the same way.

## Why stage timings carry the language

One process serves all three heads, so a stage timing without the language
cannot be attributed to one; `telemetry.ObserveStage` takes the language
alongside the stage for that reason, and the OTel span tree and the
stage-duration histograms are aligned because they share that one helper.

## Why the engine's scope is narrow

The boundaries in "What the engine is not" keep the engine's surface small
enough that adding a new query head — or a new extension point — is a local
change rather than a refactor.

The subquery bound in particular is a rejection rather than a streaming
fusion. Fusing the reducer families into streaming passes would serve grids
upstream Prometheus refuses, and a drop-in gateway's answer set is upstream's
answer set — so the engine returns the same Prom-shaped 422 upstream returns
once a subquery would load more than `query.max-samples` into memory, which
also keeps one head's grid from exhausting the process the other two share.
