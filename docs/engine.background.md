# Engine — background

This document collects the design rationale, rejected alternatives, and
failure modes behind [`engine.md`](engine.md). It answers "why is the
pipeline built this way" rather than "what does it do" — nothing here is
required to operate the engine or to add a new query head; `engine.md` is
self-sufficient for that.

## Why the optimizer ships no `FilterProjectTranspose` and no `MVSubstitution`

There is no `FilterProjectTranspose` (no lowering emits `Filter(Project)`,
so the rule would never match) and no `MVSubstitution` (the default schema
ships no live rollups, so a substitution rule would be a guaranteed no-op).

## Why the scan time bound is an IR-level property, not an emitter detail

If that innermost read carries no time predicate, ClickHouse cannot prune
granules and materialises the full per-series retention — tens of millions
of rows on a prod instant query. A bound that lived only in the emitter
would be easy to forget as new `groupArray` emitters land, so it is an
IR-level property instead.

## Why deferred label shaping merges partial states instead of combining finished grids

The combinator pair — rather than arithmetic over two finished grids — is
what makes the rewrite value-preserving. Label shaping is many-to-one, so
several raw series can carry one output identity, and their samples must be
POOLED before the window function runs; merging partial states pools
samples, whereas combining finished grids computes a different number and
yields NULL wherever one contributor holds too few samples in the window.

## Why eval-grid carriers are an interface, not a type switch

This is a correctness contract, not a style choice. A consumer written as a
type switch has to list every grid-bearing node, and the failure mode when
it misses one is **silent**: the walk finds no carrier, the consumer reads a
zero grid, and a zero grid is indistinguishable from a genuine instant query
— so a range query gets filed under the wrong evaluation mode instead of
raising an error.

## Why the subquery anchor bound is a rejection, not a streaming fusion

Fusing the reducer families into streaming passes would serve grids upstream
refuses, and a drop-in gateway's answer set is upstream's answer set — so
the bound is a rejection, which also keeps one head's grid from exhausting
the process the other two share.
