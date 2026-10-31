# ClickHouse optimizations — background

This document collects the design rationale, rejected alternatives, upstream
history, and the measurements behind
[`clickhouse-optimizations.md`](clickhouse-optimizations.md). It answers "why
is it built this way" rather than "what does it do" — nothing here is needed to
configure or operate the optimization suite; `clickhouse-optimizations.md` is
self-sufficient for that.

## Why a feature is `autoSelect: no`

Two representative reasons a feature lands there: `columnar_result_decode` (a
perf tradeoff), and `ts_grid_changes` (a correctness gap — the native builtin
diverges from reference Prometheus on NaN-adjacent windows; the divergence and
its reproduction are recorded in
[#1721](https://github.com/tsouza/cerberus/issues/1721), and the posture lifts
when a ClickHouse release fixes the builtin, not on a cerberus-side change).

## Why `aggregation_in_order` is a registry entry

The `aggregation_in_order` entry is the migration of the dark
`optimize_aggregation_in_order` rule into the registry. The eligibility check
itself is unchanged; only its enablement now flows from the resolved set.

## Why `condition_cache` is withheld on most builds before the 26.x backports

The query condition cache is meant to be result-equivalent, which is why the
feature ships under `auto`. Two upstream defects break that on builds cerberus
supports, and both were reproduced with cerberus's own DDL and emitted queries
rather than taken from the changelog:

- **Skip-index attribution (ClickHouse#105686).** With
  `use_skip_indexes_on_data_read` on (the default from 26.1), the bloom index on
  `mapValues(ResourceAttributes)` drops whole marks ahead of `PREWHERE`, and the
  empty read was recorded as "the `PREWHERE` predicate matches nothing here".
  TraceQL lowers `{ resource.service.name = "s7" && duration > 100ms }` to
  `PREWHERE (Duration > ?) WHERE (ResourceAttributes[?] = ?)`, so it poisoned the
  entry every later `PREWHERE (Duration > ?)` read — a broad
  `{ duration > 100ms && resource.service.name != "none" }` count over 600,000
  spans came back as 100,288. The reproduction needs enough rows that a whole
  read batch falls outside the narrow predicate; at 200,000 spans it did not
  trigger. PromQL label matchers are not exposed to this defect: they lower to
  `coalesce(nullIf(Attributes[?], ''), nullIf(ResourceAttributes[?], ''), '') = ?`,
  which no skip index can serve.
- **Row-policy attribution (ClickHouse#107145).** A row policy is applied ahead
  of `PREWHERE`, so granules a restricted user cannot see were recorded as
  non-matching for the predicate itself. A restricted user reading
  `otel_metrics_gauge` directly with `PREWHERE (MetricName = 'x')` poisons
  cerberus's PromQL read of the same metric, which goes through
  `merge(currentDatabase(), '^(otel_metrics_gauge|otel_metrics_sum)$')` with the
  identical predicate: an instant `max_over_time` over 50 series returned 19.
  Two narrower shapes did not reproduce: a restricted read *through* `merge()`
  wrote nothing harmful, and a metric name with OTel-to-Prometheus spelling
  alternatives lowers to `WHERE MetricName IN (...)`, which ClickHouse moves to
  `PREWHERE` itself without attributing it.

Every boundary was located on released images, with the cerberus-shaped
reproductions run against each. The third column is the conversion-setting
defect, which is gated per query shape rather than per build (below):

| Build         | Skip-index defect | Row-policy defect | Conversion-setting defect |
| ------------- | ----------------- | ----------------- | ------------------------- |
| `25.3.14.14`  | clean             | reproduces        | reproduces                |
| `25.4.13.22`  | clean             | not probed        | not probed                |
| `25.6.13.41`  | clean             | not probed        | not probed                |
| `25.8.33.6`   | clean             | reproduces        | not probed                |
| `25.10.7.6`   | clean             | not probed        | not probed                |
| `25.11.9.34`  | clean             | reproduces        | reproduces                |
| `25.12.11.4`  | clean             | reproduces        | not probed                |
| `26.1.12.23`  | reproduces        | reproduces        | not probed                |
| `26.2.19.43`  | reproduces        | reproduces        | reproduces                |
| `26.3.12.3`   | reproduces        | reproduces        | reproduces                |
| `26.3.13.31`  | clean             | reproduces        | reproduces                |
| `26.3.16.16`  | clean             | reproduces        | not probed                |
| `26.3.17.4`   | clean             | reproduces        | reproduces                |
| `26.3.17.56`  | clean             | clean             | reproduces                |
| `26.3.33.24`  | clean             | clean             | not probed                |
| `26.3.62.3`   | clean             | clean             | reproduces                |
| `26.4.3.37`   | reproduces        | reproduces        | reproduces                |
| `26.4.4.38`   | clean             | reproduces        | reproduces                |
| `26.4.5.143`  | clean             | clean             | reproduces                |
| `26.5.1.882`  | reproduces        | reproduces        | reproduces                |
| `26.5.2.39`   | clean             | reproduces        | reproduces                |
| `26.5.5.8`    | clean             | reproduces        | reproduces                |
| `26.5.6.64`   | clean             | clean             | reproduces                |
| `26.5.7.64`   | clean             | not probed        | not probed                |
| `26.6.1.1193` | clean             | clean             | reproduces                |
| `26.6.8.7`    | clean             | clean             | not probed                |
| `26.7.13.12`  | clean             | not probed        | not probed                |
| `26.7.38.18`  | clean             | clean             | reproduces                |
| `26.8.10.6`   | clean             | clean             | not probed                |
| `26.8.36.3`   | clean             | clean             | reproduces                |
| `26.9.1.1629` | clean             | clean             | clean                     |
| `26.9.27.3`   | clean             | clean             | clean                     |

Each range's upper bound is the first released build the reproduction came
back clean on, not the upstream backport build number (`26.3.17.50`,
`26.4.5.134`, …): the backport build is a CI build of the release branch, and
the recorded bound is the published tag an operator can actually run. No
released tag sits between the two. `26.3.17.4` and `26.3.17.56` share a patch
number and differ in the fix, which is why the version model carries the build
component and not only the patch.

The real-server test pins the last affected and first fixed release of every
backported line for each defect, plus the latest 26.8 release and 26.9's first
for the conversion-setting defect — fifteen builds — and runs them three at a
time; the reproductions are correctness checks, so sharing CPUs cannot change a
verdict, and one after another they would add six minutes to the lane.

The override is decided across every node cerberus can reach rather than the
one node the resolution probes, because the poisoning is per server: a mixed
fleet with one affected replica returns wrong results for every query that
lands there, however healthy the rest is. A failed resolution must not decide
it either: under `enforcing`, a rollback into an affected build makes the
explicit selection unresolvable, and keeping the previous set would otherwise
keep both the engine's `use_query_condition_cache=1` and a lifted override —
silent wrong results on exactly the build the gate exists for.

Vendor builds are judged by line because their build numbers are not
upstream's: an Altinity build such as `26.3.17.10034.altinitystable` compares
above `26.3.17.56` numerically whether or not it carries the backport. Treating
the whole line as affected costs a vendor build that has the fix its condition
cache; crediting it with a fix it may lack would cost wrong results.

An explicit `condition_cache` under `enforcing` now refuses to boot on every
25.x build, where it used to enable the cache. That is deliberate: the
operator asked for a mechanism that returns wrong results there, and
`enforcing` exists to make "I require this" fail loudly rather than degrade.

Raising the global floor or dropping the feature would have been the blunt
alternatives. Neither was needed: the defects are confined to known builds,
and on every other build the cache stays a result-equivalent win. The row-policy
defect is also why the gate cannot key on whether cerberus's own user has a
row policy — the poisoning query can come from any user of the server, and a
restricted user often cannot see `system.row_policies` to find out.

## Why String-to-float filters skip the condition cache on 25.3 through 26.8

A conversion function captures the conversion settings of the session that
built it, but the query condition cache keyed its filter by the function alone
(ClickHouse#119168), so a session that parses a value differently shares the
verdict. A LogQL numeric label filter lowers to
`toFloat64OrZero(<label>) <op> toFloat64(<literal>)`; another client replaying
cerberus's SQL from `system.query_log` with the opposite
`precise_float_parsing` poisons cerberus's next read. Over 1.2 million logs,
half carrying `1.1e-300`, `| v != 1.1e-300` counted 601,984 (610,176 on 25.x)
instead of 1,200,000 where fast parsing is the default (26.3 and earlier), and
`| v = 1.1e-300` counted 0 instead of 600,000 where precise parsing is (26.7
and later). The defect is as old as the cache: it reproduces on every line from
25.3 through 26.8, including the latest 26.3 and 26.8 LTS releases, and on no
26.9 release probed (the survey table above). The upstream fix landed in 26.10
and was not backported to 26.3 or 26.8.

The trigger is a filter that parses a String to a float, and nothing else, so
the gate is that shape rather than the build. Withholding the feature on every
build the defect reaches would switch the cache off for every query on both
LTS lines, the 26.8 build the Helm chart bundles included, to protect a minority
of LogQL and TraceQL queries. The shape is read off the plan's `Filter`
predicates, the `WHERE` / `PREWHERE` the cache keys on; a parse that sits in a
projection or an aggregate argument is never a granule filter. The lowerings
that put one in a filter are the LogQL numeric and bytes label filters
(`toFloat64OrZero`, `toFloat64OrNull`), the TraceQL numeric attribute coercion
(`toFloat64OrNull` over each attribute arm) and PromQL's `le` read over float
bucket series (`isNotNull(toFloat64OrNull(le))`). Plain `toFloat64` is left out
because every lowering that emits it passes a number (a sample value, a count,
a duration, a numeric tuple element), and a number-to-float cast reads no
conversion setting; the plan carries no static types, so the function name is
the signal. `internal/engine`'s lowering test runs each head's emitting query
through the gate, so a lowering that moves its parse out of a filter fails
there.

The `0` is stamped whatever the selection resolved, `off` included, because
the server turns the cache on by default from 25.9; the `1` the
`condition_cache` rule would stamp is withheld on the same plan. That a
per-query `use_query_condition_cache=0` switches the cache off for that query
in both directions was checked on every pinned build rather than assumed: after
an opposite-parsing session primes the cache, cerberus's SQL with the setting
`0` returns the cache-off answer (it does not read the entry), and after
cerberus's SQL runs with the setting `0`, an opposite-parsing session reading
through the cache gets its own cache-off answer (it wrote none), where the same
SQL with the setting `1` poisons that session on every affected build.

## Why `TraceId` reads skip the projection index on a negated conjunction from 25.11 to 26.10

From 25.11 (ClickHouse#81021) the server can filter a part's rows through a
normal projection that was not selected to serve the read, which is how
`proj_trace_id` turns a `TraceId` predicate into a row bitmap. ClickHouse#120286
fixed that analysis for a negated `AND` over the projection key: the negation
was dropped, so the projection index removed exactly the rows the predicate
keeps. TraceQL's `!( … )` over a compound reaches it.
`{ trace:id = "X" && !(trace:id = "X" && span.foo = "nope") }` lowers to
`PREWHERE (TraceId = ?) WHERE … AND not((TraceId = ?) AND (SpanAttributes[?] = ?))`,
and with the projection installed `/api/search` matched none of the trace's 60
spans, every one of which the predicate keeps; the same request with
`optimize_use_projection_filtering=0` matched all of them. The seed is 20,000
traces of 60 interleaved spans, 1.2 million rows. Its parts are each smaller
than the server's default `min_table_rows_to_use_projection_index`, so the
reproduction lowers that threshold to 0 the way `trace_id_bitmap_filter` does;
a production-sized part clears the default on its own.

| Build         | Negated `AND` through `proj_trace_id` |
| ------------- | ------------------------------------- |
| `25.9.7.56`   | clean                                 |
| `25.10.7.6`   | clean                                 |
| `25.11.1.558` | drops every span                      |
| `25.11.9.34`  | drops every span                      |
| `25.12.11.4`  | drops every span                      |
| `26.1.12.23`  | drops every span                      |
| `26.2.19.43`  | drops every span                      |
| `26.3.17.56`  | drops every span                      |
| `26.3.62.3`   | drops every span                      |
| `26.8.36.3`   | drops every span                      |
| `26.9.25.15`  | drops every span                      |
| `26.9.27.3`   | drops every span                      |
| `26.10.2.103` | clean                                 |
| `26.10.3.7`   | clean                                 |

The fix was not backported, so the range spans whole lines.

### The trigger, and why the gate is that shape

The weakening replaces a conjunct the projection cannot compute by `true`, and
under a negation that makes the prefilter stricter than the predicate:
`NOT (TraceId = X AND b)` becomes `NOT (TraceId = X)`, or `NOT true` when no
conjunct is computable. Each variant below was run on `26.3.17.56` as
`{ trace:id = "X" && <e> }`, with the projection threshold at 0, projection
filtering on and the condition cache off, against the 60 spans of trace `X`:

| `<e>`                                      | Matched |
| ------------------------------------------ | ------- |
| `!(trace:id = "X" && span.foo = "nope")`   | 0       |
| `!(span.foo = "nope" && span.bar = "x")`   | 0       |
| `!(duration > 1h && trace:id = "X")`       | 0       |
| `!(trace:id = "Y" && name = "nope")`       | 60      |
| `!(trace:id = "Y" \|\| span.foo = "nope")` | 60      |
| `!(span.foo = "nope" \|\| span.bar = "x")` | 60      |
| `!(trace:id = "Y" \|\| trace:id = "Z")`    | 60      |

A negated disjunction is left intact, and a negated conjunction loses rows
whether or not `TraceId` sits inside it. `!(trace:id = "Y" && name = "nope")`
answered correctly only because the weakened `NOT (TraceId = Y)` happens to
keep every span of `X`. The gate is therefore a `TraceId`-keyed read whose
filter holds a `not(…)` over an `AND` that reads any column other than
`TraceId`: it covers the lucky case too, since its correctness depends on the
literals rather than the shape, and leaves out the negated disjunctions and an
`AND` of `TraceId` tests, which the projection computes in full. A read that is
not `TraceId`-keyed never filters through `proj_trace_id`, so it is out of
scope whatever its filter negates.

Gating the shape rather than the build keeps the projection index on every
other `TraceId` read — the trace-by-id lookup, the search root lookup, the
structure tab and every structural join — on every release from 25.11 through
26.9, both LTS lines included, and keeps an explicit `trace_id_projection`
bootable under `enforcing` there.

### The 25.11 plain lookup

On 25.11 a plain `{ trace:id = "X" }` lost spans through the same read as well
(10 of 60 on `25.11.1.558` and `25.11.9.34`), which `25.12.11.4` no longer
does. Raw SQL on `25.11.1.558` narrows the trigger: `SELECT count() … WHERE
TraceId = X` counts all 60, and any query that also reads a column the
projection does not store (`Timestamp >= …`, `max(SpanName)`) sees 18. It is
the same with `max_threads = 1`, so it is not the parallel-replica race
ClickHouse#89762 fixed, and with the table merged into one part (4 of 60). No
upstream change was identified as its fix.

The exact trigger is therefore a `TraceId`-keyed read through the projection
index that reads any column outside `(TraceId, _part_offset)`. Every
`TraceId`-keyed read cerberus emits reads such a column — no head asks for a
bare count of a trace's rows — so the per-query gate is every `TraceId`-keyed
read, scoped to the 25.11 line by its own range (`[25.11, 25.12.11.4)`). That
is the narrowest gate the shape allows; on 25.11 it amounts to the projection
index being off for cerberus's reads, which is why the operator guidance is to
upgrade.

### Why the stamp does not depend on `trace_id_bitmap_filter`

The server enables projection filtering by default, and a projection, once
added, stays on the table whatever a later selection says: a deployment that
installed `proj_trace_id` and then rolled ClickHouse forward into the range
filters through it whenever a part clears the server's
`min_table_rows_to_use_projection_index` default. So
`optimize_use_projection_filtering=0` rides the affected shape whether or not
`trace_id_bitmap_filter` resolved, and under `off` too. The setting only
governs filtering through a projection the read did not select; the
aggregating metric-catalog projections are chosen to serve a read, a separate
mechanism it does not switch.

The real-server test checks the projection index is really used on the shapes
left ungated, not only that the setting keeps its default: the plain lookup's
ProfileEvents count an `IndexBinarySearchAlgorithm` analysis, which only
`proj_trace_id`'s `TraceId`-first key admits for that predicate (the table's
own key leads with `ServiceName`), and the same lookup with projection
filtering off counts none.

## Why defect gates are per query shape where the trigger allows it

A build-wide range withholds a feature on every query of every deployment on
that build. For the three defects above that cost was out of proportion to the
trigger: it would have switched the condition cache off on every build before
26.9 — both LTS lines, the 26.8 the Helm chart bundles included — withheld the
auto-selected `trace_id_bitmap_filter` from 25.11 through 26.9, and failed boot
on an explicit `trace_id_projection` under `enforcing` on those releases, to
protect a handful of query shapes. Each of these defects has a trigger the
lowered plan shows, so each is gated on the queries of that shape with the
setting that switches the mechanism off for one query, and the operator-facing
change is none on every other shape.

The build-wide ranges stay where the trigger is not in the plan:
ClickHouse#105686 and #107145 poison the cache through what another query or
another user does (a skip index dropping marks before `PREWHERE`, a row
policy), so no property of cerberus's own query predicts which entry is
poisoned.

A shape defect is decided across the fleet the same way as the analyzer
posture, and for the same reason: a query of the defective shape can land on
any reached node, so one node on an affected build is enough to put the defect
in force, and only a complete pass with every node outside the range lifts it.
It is a fact about the server, not a selection, which is why `off` carries it
like `auto`.

## Why a gate's setting is stamped on a mixed fleet

The supported floor is 24.8, so a fleet can mix a node too old to know a gate's
setting with a node inside a defect's range — a rolling upgrade from 24.8
straight onto a 26.x LTS line is the common case. Cerberus sends every
per-query setting through clickhouse-go's settings path over the native
protocol, and clickhouse-go v2 marks each such setting important; a server
that does not know an important setting rejects the whole query. Sent that way
through `chclient`, against real servers:

| Build        | `use_query_condition_cache=0` | `optimize_use_projection_filtering=0` |
| ------------ | ----------------------------- | ------------------------------------- |
| `24.8.14.39` | code 115, `Unknown setting`   | code 115, `Unknown setting`           |
| `25.3.14.14` | accepted                      | code 115, `Unknown setting`           |
| `25.4.13.22` | accepted                      | code 115, `Unknown setting`           |
| `25.5.11.15` | accepted                      | code 115, `Unknown setting`           |
| `25.6.13.41` | accepted                      | accepted                              |
| `25.10.7.6`  | accepted                      | accepted                              |

`test/chserver`'s `TestShapeDefectGates_SettingFloors` pins the 24.8, 25.3
and 25.10 rows; the 25.4 to 25.6 rows were run once to place the projection
setting's floor at 25.6.

What makes stamping safe is that every affected range starts at or above its
gate's first accepting release (`TestShapeDefects_RangesStartAtOrAboveTheirSetting`):
a defect cannot predate the mechanism the setting switches off. A node too old
for the setting therefore never answers the gated shape wrong; it rejects it.
Every other node answers it correctly, so a mixed fleet keeps answering the
shape on every node but the old one, and no node ever returns wrong rows.
`TestShapeDefectGates_MixedFleetNeverAnswersWrong` runs that fleet — 24.8
beside an affected 26.3 build — through the production handlers.

The raw rejection would reach the caller as an opaque `ClickHouse rejected
the query` that names neither the defect nor the remedy, so chclient
recognises `UNKNOWN_SETTING` for a gate setting and reports the setting, the
defects it avoids and the release to upgrade the node to. It stays a 502, the
status of every ClickHouse rejection: the failure is on the server side, and
the node that raised it is the one to act on.

Rejected alternatives:

- **Refuse the shape on such a fleet.** Cerberus would fail every query of the
  gated shape before dispatch, with one deterministic error, whenever an
  affected node and an old one were both reached; under the condition-cache
  override, every query. That gains no correctness — the old node never
  answers wrong, and the affected node answers right with the setting — and
  cuts the shape's availability on the fleet to zero, or the whole gateway's
  under the override.
- **Decide per node.** The setting would go only to the nodes that know it.
  Cerberus cannot route that way: one driver pool picks the node per
  connection, and replicas behind `clusterAllReplicas`, a `Distributed` table
  or a load balancer are reached through whichever node the connection landed
  on.
- **Skip the gate when a node is too old.** The affected node answers the
  defect's shape wrong, with no error — the outcome the gates exist to
  prevent.

## ClickHouse 26.10 wrong-result fixes no cerberus query reaches

The 26.10 release fixed several other wrong-result defects in mechanisms
cerberus enables. Each was checked against cerberus's emitted SQL and DDL, and
where a shape came close it was run against an affected build:

- **ClickHouse#119823, a join runtime filter's condition-cache verdict.** The
  defect writes a cache entry for a `PREWHERE` that carries a join runtime
  filter, on 26.9 before `26.9.23.15`. Cerberus's TraceQL structural joins do
  plant `__applyFilter` in the left read's `PREWHERE`, but the residual filter
  there holds a `globalIn(tuple(TraceId, SpanId), …)` set membership the cache
  writer refuses, so no entry is written: every structural operator on
  `26.9.22.3` left the cache empty except `<<` and `>>`, whose one entry is
  benign, and a live-ingest rerun that inserted the roots after priming matched
  the cache-off answer. PromQL binary operators keep the runtime filter in a
  filter step above the read, a shape the cache already declines.
- **ClickHouse#122538 / #122617, top-K cache entries.** Both need `SAMPLE` or
  mutations applied on the fly; cerberus emits no `SAMPLE`, issues no
  mutation or lightweight update, and never sets `apply_mutations_on_fly` or
  `apply_patch_parts`. Top-K recording (ClickHouse#114659) is not in any
  released line before 26.10.
- **ClickHouse#124442, a sparse column expanded under lazy materialisation of
  a JOIN.** Lazy materialisation across a JOIN arrived with ClickHouse#122682,
  backported only to 26.10, which already carries the fix; no released build
  has the one without the other.
- **ClickHouse#124022, a partial projection with
  `query_plan_remove_unused_columns=0`.** The defect is in reading through a
  normal projection that serves the read. `proj_trace_id` stores only
  `(TraceId, _part_offset)` and no cerberus read selects only those columns;
  the metric-catalog projections are aggregating, a different path; cerberus
  never sets `query_plan_remove_unused_columns`, whose default is 1. The
  upstream reproduction does not reproduce on `26.3.62.3`, `26.8.10.6` or
  `26.8.36.3` either.
- **ClickHouse#124303, minmax and set index verdicts across `AND` / `OR`.**
  The shift needs an atom the index cannot convert — a non-monotonic function
  chain or an unconvertible constant — over a minmax- or set-indexed column.
  Cerberus's indexed columns (`Duration`, `TimeUnix`,
  `AggregationTemporality`) are only ever compared directly against constants.
  Thirteen TraceQL `||` shapes over `duration` and `status` returned the same
  answer with `use_skip_indexes` on and off on `26.3.62.3`, where the upstream
  reproduction does reproduce (0 rows against 667).

## Why every cancelled dispatch kills its own statement

The cancellation probes drive a `double_exponential_smoothing` over one series
whose samples all fall in the window (one `arrayFold` call) and a PromQL
selector over a label name tens of millions of characters long that all need
`replaceRegexpAll` normalization. In the exploratory runs, under a 1 s
`max_execution_time`, `26.6.8.7` ran the fold for 9.7 s and `26.7.13.12`
stopped it at 1.05 s; a 20 MB label name ran 2.3 s on `26.6.1.1193` and
2.6 s on `26.7.13.12` against a 1 s limit, and stopped at 1.01 s on
`26.8.1.2041`.

The probes also showed that a client disconnect was not bounded even on the
fixed builds. A cancelled clickhouse-go dispatch sends `ClientCancel`, which
ClickHouse notices only between pipeline blocks, not inside a long function
call, while the in-function checks the upstream fixes added honour `KILL QUERY`
and `max_execution_time` only. Before cerberus killed every cancelled dispatch,
the single-shard path — the default deployment — freed the request's admission
slot and its connection while the fold or replacement ran to completion, on
every build; only the data-shard fan-out gate already issued `KILL QUERY`. The
cost of the kill is one extra statement per cancelled dispatch, and a wait
bounded by the kill timeout before the release proceeds when the server cannot
interrupt the call or the kill itself queues for a pooled connection — the
honest account of a slot whose work is still running.

A dispatch cancelled before the driver got the server's answer — still waiting
for a pooled connection, a dial, or the send — issues no kill. Under pool
saturation the kill would queue for the same pool the dispatch was waiting on,
holding the admission slot for the full timeout to kill a statement that never
existed.

The probes size their own workload. The same seed ran in 5.7 s on one build
and 10.9 s on another, and about twice as fast on a CI runner as on a
developer host, so a fixed seed that suits one substrate starves another: on
CI a fixed fold left 1.9–3.5 s of work at the cancellation, too little to tell
the outcomes apart. Each shape is therefore calibrated per build: seeded at a
base size, run uncancelled, and re-seeded in proportion until the run takes
about 10 s, within a size bound. The server runs at half a CPU during these
probes. The functions are single-threaded, so the throttle stretches one call
without growing it, and growing the fold instead costs about 4 KiB of memory
per sample — two concurrent siblings at the size a fast runner needs would not
fit — while its own teardown after an interrupt grows with it: unthrottled at
700,000 samples, interrupted siblings took 2.5 s to end.

The verdict is relative to the work that was left at the cancellation: an
interrupted call must end within a quarter of it, an uninterrupted one no
sooner than half of it. Observed: interrupted calls ended within 0.14 of the
remainder, uninterrupted ones after 0.7 or more. A fixed threshold would have
to be retuned per substrate; a relative one scales with the calibrated work.

`KILL QUERY` on one's own query is allowed under `readonly = 1` and
`readonly = 2`, so a read-only cerberus user can issue it.

## Why `ts_grid_range` is auto-enabled, and the scan-order gate that was not shipped

A prod-data validation proved the native path result-correct (more correct than
the buggy fan-out for `rate`) at flat memory, so `auto` picks it by version.

An order-independent scan-side gate exists and is sound, but
[#2924](https://github.com/tsouza/cerberus/issues/2924) measured it at a
~2.1-2.4x wall-clock tax on this exact path even under the best-case ORDER BY
alignment, for no memory benefit, and closed without shipping it (see
`chsql.nativeTSGridFn`'s own "Verdict on the scan-order gate" doc for the full
measurement). The only remaining path is an upstream ClickHouse report, which
needs authorization this repo has not given.

## Why `ts_grid_changes` is permanently opt-in, and why its floor is 25.9

Tracked as [#1721](https://github.com/tsouza/cerberus/issues/1721), closed by
making the feature permanently opt-in (`autoSelect: no`) rather than waiting on
an upstream fix: the divergence lives inside the ClickHouse builtin, which
cerberus cannot patch, so `CERBERUS_CH_OPTIMIZATIONS=ts_grid_changes` must be
listed explicitly and `auto` never selects it.

A 25.6 floor would mis-advertise support on 25.6-25.8 servers and 502 with
ClickHouse error code 46, `Function with name timeSeriesChangesToGrid does not
exist` — an absent `timeSeries*ToGrid` member is reported as an unknown
FUNCTION, not as `UNKNOWN_AGGREGATE_FUNCTION` (verified against 25.7 and 25.8
servers).

## Why `ts_grid_deriv` / `ts_grid_predict_linear` pin the family's 25.9 floor

Their aggregates (`timeSeriesDerivToGrid` / `timeSeriesPredictLinearToGrid`)
shipped in ClickHouse **25.8** (PR #84328) — a quarter EARLIER than
changes/resets — but the registry pins them to the family's shared **25.9**
floor (the left-open-window fix, PR #86588) so one probed capability verdict
governs every member.

The native == fan-out numeric differential (a Float64 fit, so ULP-close rather
than bit-identical) is proven on a `>= 25.9` server in the prod/e2e lane, not
on the sub-25.9 chDB CI substrate, where the version gate keeps both on the
fan-out; the always-on SQL-shape goldens (`native_deriv_range_step.txtar`,
`native_predict_linear_range_step.txtar`) pin the native emit unconditionally.

## Why `ts_grid_recollapse` splits the grid into `-State` / `-Merge`

On a reference deployment's heaviest APM range query — 35,094 raw series over a
300s window — this is **-28.5% CPU time (1.40x)**: 310.3 CPU-seconds down to
221.9. Wall time on the same paired runs moved 28.0s to 19.8s, but wall and CPU
diverge with concurrency and shard count, so the resource saving is the claim
rather than the latency.

The `-State`/`-Merge` pair is load-bearing rather than incidental. Key
sanitisation is **non-injective**: those 35,094 raw series shape onto 33,557
output series, and the colliding groups are frequently time-disjoint with a
splice gap smaller than the range window, so grid anchors near a splice have
windows straddling both halves. Prometheus `rate()` is defined over the POOLED
sample set at each anchor, which is what merging partial states computes;
combining two FINISHED grids arithmetically is a different number, wrong at 481
of 93,757 points on that query and NULL where one half holds fewer than two
samples.

### Where the 25.9 floor comes from, and the merge-exactness probe

The **25.9** floor is INHERITED from `ts_grid_range` rather than independently
derived: with `ts_grid_range` off there is no native node to defer anything
past, so that feature's floor is the effective one and nothing about the
re-collapse raises it. Merge exactness is not the binding constraint. The
exactness probe compares one pooled pass against a merge of two per-group
partial states, over samples split into two time-disjoint halves whose splice
gap is smaller than the range window, so the grid anchors near the splice
straddle both halves:

```sql
-- pooled
SELECT timeSeriesRateToGrid(start, end, 60, 300)(ts, val) FROM src
-- merged
SELECT timeSeriesRateToGridMerge(start, end, 60, 300)(st) FROM (
  SELECT timeSeriesRateToGridState(start, end, 60, 300)(ts, val) AS st
  FROM src GROUP BY raw)
```

Executed against 25.8.28.1, 25.9.7.56, 26.1.12.23, 26.2.19.43, 26.3.17.56,
26.4.5.143 and 26.5.6.64 (no 26.0 image is pullable, so that version is
bracketed by its neighbours) in each of three data regimes — time-disjoint,
interleaved, and counter-reset-straddling — the merged grid equals the pooled
grid in all 21 cells. The reset-straddling regime yields a grid that DIFFERS
from the other two, which is what shows the reset correction is applied through
the merge rather than skipped. The floor is therefore the same one every other
family member pins, so one probed capability verdict still governs the whole
set.

## What `ts_grid_histogram` measured, and where its floor comes from

Measured against a real ClickHouse 26.6 at realistic scale, same rows read
(~88-89k), 121 anchors, 5m window: the array fold takes 4,123 ms / 3.411 GB
peak / 51.5 CPU-s, the native aggregate 148 ms / 0.130 GB peak / 0.4 CPU-s —
**28x faster, 26x less memory, 129x less CPU**.

The **25.9** floor is INHERITED from the family rather than independently
derived: the shape rides the same `timeSeriesRateToGrid` `ts_grid_range` pins,
so it inherits that feature's binding constraint (the left-open / right-closed
membership window, upstream PR #86588). The presence aggregate is a
`ts_grid_resets` sibling from PR #86010, released in the same 25.9, so the
floor is unchanged either way.

## Why `quantile_prom_histogram` is opt-in, and where its cardinality ceiling comes from

A real-CH differential (`internal/chsql`'s
`TestHistogramQuantile_RankWalkNative_DifferentialRealCH`) confirmed exact
agreement with the legacy walk across representative bucket layouts (a normal
crossing, a duplicate-bound layout, the equal-length/no-overflow-rung shape, an
empty histogram, a first-bucket non-positive upper bound, and an all-zero-count
histogram) and the full phi domain (below range, the two saturating edges,
interior crossings, above range, and a runtime NaN phi).

`AutoSelect` is `false`: correctness parity is proven, but a real-scale
measurement (25.10.7.6, a real OTel classic-histogram export) found a genuine
performance TRADEOFF, not just an unproven new floor — the ORIGINAL emission's
`ARRAY JOIN` multiplied row count by the bucket-ladder length before `GROUP BY`
collapsed it back down, which the legacy walk never does. At real-world
dashboard scale (3,677 series) the native path was ~2x faster at equal memory;
at high series cardinality (73,540 series, ~880k post-unnest rows) wall time
stayed roughly even but memory grew ~3.3x. A follow-up real-ClickHouse 25.10
measurement at four additional cardinality points between those two (the same
real sample, synthetically fanned out) found memory crossed above the classic
walk's between roughly 18,000 and 22,000 series (~215k-265k post-unnest rows at
this sample's 12-bucket layout) and kept growing roughly linearly with series
count past that point ([#2790](https://github.com/tsouza/cerberus/issues/2790)
PR 1).

A second real-ClickHouse 25.10 measurement, run after rewriting the emission to
the BOUNDED-window shape above (#2790 PR 2), found the native path's OWN peak
memory drops ~1.5x-2x at every one of five cardinality points re-tested (3,677
through 73,540 series) relative to the original unbounded emission, because the
`ARRAY JOIN` now unnests a small constant number of rungs per row instead of
the whole ladder. The tradeoff shrinks rather than disappears — a per-row rank
search plus a narrower `ARRAY JOIN` still costs more than the classic walk's
pure array-expression form once cardinality is high enough — so `AutoSelect`
stays `false`.

The operator-facing ceiling is 1.5x PR 1's original ~15,000-series guidance,
taking the CONSERVATIVE (1.5x) end of the measured ~1.5x-2x memory-reduction
range so an operator whose workload sits at the low end of that range still has
real headroom; up to ~30,000 series is achievable in the best-measured (2x)
case, but is not the safe default to design around. See
[#2790](https://github.com/tsouza/cerberus/issues/2790) for the full numbers
and methodology from both PRs.

## Why `map_bucketed_serialization`'s floor is 26.4 rather than 26.3

**Version floor is 26.4, not the 26.3 the upstream backport (v26.3.2.3-lts)
technically landed in**: this registry compares `(major, minor)` only, and
26.3.0/26.3.1 lack the feature, so a "26.3" floor would wrongly claim they have
it.

## Why `column_statistics` is opt-in

**PREWHERE reordering IS verified, not merely claimed**: the issue itself
flagged as unverified "whether statistics-based condition reordering hooks into
cerberus's explicitly written PREWHERE clause (vs only the WHERE→PREWHERE move
optimizer)"; upstream RFC
[ClickHouse#53240](https://github.com/ClickHouse/ClickHouse/pull/53240) ("use
statistic to order prewhere conditions better") confirms
`allow_statistics_optimize` reorders an ALREADY-multi-condition PREWHERE
clause's own conjuncts by statistics-derived selectivity — exactly cerberus's
own emission shape, not only the promotion decision. `AutoSelect` is still
`false`, though: the Cloud gap remains, the real-world MAGNITUDE of a PREWHERE
reorder or a join-side pick on cerberus's own production query shapes is not
yet measured, and a plan-shape change from better join-side selection could
interact with the solver's calibrated fanout-guard constants the way spill
settings did in [#2665](https://github.com/tsouza/cerberus/issues/2665) —
real-world calibration questions this feature alone cannot answer, so enabling
it is a deliberate operator choice pending that evidence.

## Why `join_spill` exists, and why it stamps an explicit byte bound

`join_spill` closes the last gap in the "spill settings only protect GROUP
BY/sort" pain point: join memory was previously backstopped only by throwIf
cardinality guards (`VectorJoin`'s own `ManyToManyMatchMessage`) and structural
shape restrictions, neither of which bounds memory, so a big hash build could
hit a destructive `MEMORY_LIMIT_EXCEEDED` (code 241) abort.

**Version floor is 26.4**: `max_bytes_before_external_join` carries an
EXPERIMENTAL marker at introduction and is treated as production-grade from
26.5, where its ratio- default sibling
(`max_bytes_ratio_before_external_join=0.5`) ships — this registry entry pins
the floor to 26.4, where the setting first exists to stamp, and separately
marks `Stability` as `experimental` to keep that honestly reflected here.

**Explicit stamp, not the ratio default**: a ratio setting is silently ignored
when no server/user memory limit is configured — the same failure mode
[ClickHouse#76740](https://github.com/ClickHouse/ClickHouse/issues/76740)
documents for the analogous group_by ratio — so cerberus cannot rely on it
regardless of an operator's ClickHouse profile.

## Why `distinct_spill` is ungated, explicit, and opt-in

Before ClickHouse 26.9 a `DISTINCT` had no spill path. It held every distinct
key in an in-memory hash set, and neither the group_by nor the sort threshold
reached it, though an earlier `spill.go` comment claimed they covered TraceQL
structural `DISTINCT`.

**Floor 26.9, from the upstream history.**
[ClickHouse#116569](https://github.com/ClickHouse/ClickHouse/pull/116569)
merged to master on 2026-09-17. Its 26.9 backport, #120572, merged the same
day, and the compare API reports `v26.9.1.1629-stable`, the line's first
release, as ahead of the backport's merge commit. No 26.8 backport exists:
26.8.36.3, the newest 26.8 LTS checked, rejects the setting with code 115.
`test/chserver` pins 26.8.10.6, 26.9.1.1629 and 26.9.25.15.

**No plan-shape gate.** `join_spill` gates on `chplan.HasJoin`, because a join
is a plan node. A `DISTINCT` is a modifier the emitter adds inside several
lowerings: the structural closures, the set-operation subqueries, and the
lag-adjacency dedup. No `planShapeFacts` predicate names all of them, and a
predicate that missed one would leave that shape's hash set unbounded.
Without a gate the stamp costs nothing elsewhere, because only a `DISTINCT`
transform reads it.

**Explicit byte value, not the native ratio.** 26.9 also ships
`max_bytes_ratio_before_external_distinct=0.5`, enabled by default. The ratio
is taken of the memory left under the strictest server or user limit when
the pipeline is built. `max_memory_usage` does not take part, and the ratio
has no effect when neither limit is set. Cerberus aborts a query at its own
per-query cap, so the threshold is sized from that cap.

**Opt-in until the quickstart reaches 26.9.** The stamp turns an abort near
the cap into a disk-backed completion, which is the posture `join_spill`
auto-selects on. Check (d) of `clickhouse-version-sync.mjs` requires the
quickstart's ClickHouse to meet every `AutoSelect` floor. The quickstart is
the 26.8 LTS. Flipping `AutoSelect` is tracked in
[#4014](https://github.com/tsouza/cerberus/issues/4014).

**Measurements on 26.9.25.15** (container capped at 2 CPUs and 8 GiB):

- A plain `SELECT DISTINCT` of 30M two-string keys under a 256 MiB cap
  aborts with code 241 in `ExternalDistinctTransform`. With a 128 MiB
  threshold it completes.
- A recursive CTE whose anchor is that `DISTINCT` (30M rows, `count()`
  outside) still aborts with the threshold stamped, but in
  `RecursiveCTESource`. `system.query_log` records 14
  `ExternalDistinctWritePart`, so the anchor's `DISTINCT` did spill. At
  5M–10M rows under a 512 MiB cap it aborts without the threshold and
  completes with it. At 15M rows it aborts either way.
- A recursive step that produces 5M distinct rows from a one-row anchor
  aborts under a 512 MiB cap without the threshold. With a 256 MiB threshold
  it completes, peaking at 311 MiB with 4 spilled parts.
- The `test/chserver` probe runs 1.5M root/child trace pairs through
  `{ resource.service.name = "frontend" } > { resource.service.name = "api" }`
  under a 1 GiB cap. Without the stamp it aborts with code 241 in
  `CreatingSetsTransform` (would use 1.08–1.09 GiB). With it, it completes
  at a peak of about 976–979 MB with 7 spilled parts. Run to run the peaks
  varied by under 0.5%. The probe fails without the stamp at every size from
  1.25M to 2M pairs. At 2M pairs it aborts with the stamp too. Its
  `DISTINCT` feeds a `GLOBAL IN` set of the same rows, which the threshold
  does not bound.

## Why `lazy_materialization` replaced the hand-rolled `late_mat.go` rewrite

This replaces cerberus's own hand-rolled late-materialisation rewrite (formerly
`late_mat.go`, deleted alongside this feature): that structural
`Project(Limit(Filter?(Scan)))` matcher never fired on any production query
path, because at the time all three production `Limit` constructions wrapped an
`OrderBy` directly under `Limit` (the matcher's switch only accepted
`Filter`/`Scan` there) and the Loki line path built no SQL `Limit` at all,
applying the request limit Go-side in `buildRangeData` — the gap since closed
by `maybePushLogLineLimit`, described in
[`clickhouse-optimizations.md`](clickhouse-optimizations.md).

Sizing the knob to the request's own LIMIT rather than to a fixed ceiling was
verified on a live chDB 26.5 probe that a max-limit knob BELOW the query's
actual LIMIT silently falls back to eager reads (no `LazilyReadFromMergeTree`
step in `EXPLAIN PLAN`), so a fixed constant would silently stop helping the
instant a caller's limit grew past it.

`AutoSelect` is `true`: the same chDB probe confirmed the stamp is
RESULT-EQUIVALENT (identical row count and column set with and without it —
only the read order changes) and that ClickHouse's own top-N PREWHERE promotion
(`__topKFilter` on the sort column) fires independently of this setting, so
there is no negative PREWHERE interaction. Forcing `enable_analyzer=0` on the
same probe made the `LazilyReadFromMergeTree` step disappear entirely, which is
why cerberus co-stamps `enable_analyzer=1` alongside the setting.

## Why window-slide anchor injection is gated on a Lookback/Step ratio of 10

The measured speedup **tracks that ratio directly and is not a flat
multiplier**: a 5-minute window at a 1-minute step (ratio 5, the modal Grafana
panel shape) measured only 1.12x — explicitly below the eligibility threshold,
so that shape stays on the fan-out — while a 5-minute window at a 30-second
step (ratio 10, the threshold) measured 1.70x, a 5-minute window at a 15-second
step (ratio 20) measured 2.65x, and a 30-minute window at a 15-second step
(ratio 120) measured 10-14x.

## Why `ts_grid_delta` / `ts_grid_irate` / `ts_grid_idelta` are auto-enabled

Each shipped after a chDB differential sweep against its aggregate
([#2745](https://github.com/tsouza/cerberus/issues/2745) for delta,
[#2746](https://github.com/tsouza/cerberus/issues/2746) for irate and idelta)
that isolated every rule the ClickHouse documentation leaves underspecified —
the counter-reset posture, the trailing-pair selection, the two-sample NULL
rule, the left-open window edge, the extrapolation clamp branch — and matched
reference Prometheus on each. The one gap those sweeps found is the family-wide
duplicate-timestamp NaN survivor (originally
[#2798](https://github.com/tsouza/cerberus/issues/2798)), which the already
auto-selected rate / increase / resets / deriv / predict_linear members carry
identically, so it is not a reason to treat any one member differently.
ClickHouse #115920 made that survivor order-independent from 26.8.1.2041 on
(the finite sample wins), which removes the nondeterminism but not the
disagreement with the fan-out, whose rule elects the NaN;
[#3648](https://github.com/tsouza/cerberus/issues/3648) aligns the fan-out's
rule. The two
family members that ARE opt-in carry divergences ordinary data reaches:
`ts_grid_changes` on any NaN-adjacent window with no duplicate at all, and
`ts_grid_group_array` because it would import the nondeterminism into paths
that do not have it today.

## Why `ts_grid_vector_agg` is opt-in

It is a new code path with no fielded validation yet — the same posture
`ts_grid_instant` took on its own new floor — whereas `ts_grid_recollapse`, the
other narrowing of the native grid, had its merge exactness checked across
time-disjoint, interleaved and reset-straddling regimes before shipping
auto-on. The five outer functions are the set proven element-wise over a
finished per-series grid; `stddev` / `stdvar` / `quantile` / `group` are not
element-wise and stay on the exploded Aggregate permanently.

## Why `laginframe_adjacency` is auto-enabled and `fixed_accumulator_extrapolated` is not

The annotation pass carries the same kernels as the fan-out (`curr < prev` for
resets; `curr != prev AND NOT both-NaN` for changes; the DELTA / CUMULATIVE
pair delta for irate) and was proven bit-identical by dual-emit parity, so
there was nothing to weigh. The fixed-accumulator shape was also proven
bit-identical, but [#2894](https://github.com/tsouza/cerberus/issues/2894)'s
real ClickHouse 26.6.4.55 A/B (query_range `rate()` / `increase()`, 2,200
series, 1h span, 60 anchors, 5m window) measured half the source rows and ~20%
fewer bytes at neutral wall-clock (within ~10%) against ~1.8-2x the array-fold's
peak memory — six concurrent per-group accumulators against one `groupArrayIf`.
A memory increase for no consistent wall-clock win is an OOM-risk uplift on
every default `rate()` request, so it stays opt-in.
[#3556](https://github.com/tsouza/cerberus/issues/3556) re-ran the A/B for the
multi-name, high-fan-out selector shape that times out on the 24.8-pinned
compatibility floor lane and reproduced the same trade (~1.6x memory), so no
narrower auto rule changes the verdict. That investigation also found and
fixed a real bug in the shape: the counter-delta term rendered unparenthesised
on a scan with no temporality column, silently dropping the extrapolation
factor; the parity corpus had only ever covered the temporality-bearing
fixture.

## Why `sorted_slab_over_time` is opt-in

[#2894](https://github.com/tsouza/cerberus/issues/2894)'s A/B found the shape's
own "O(samples per series), independent of anchor count" memory claim did not
hold as shipped: at 60-240 anchors it used 6-9x the array-fold's peak memory
for no wall-clock win, and at 500 series / 480 anchors / 5m it OOMed a 6 GiB
cap where the array-fold finished at 535 MiB.
[#3046](https://github.com/tsouza/cerberus/issues/3046) traced that to
ClickHouse's block execution retaining the per-anchor `arrayFilter` /
`arrayMap` intermediates across a whole block of series rows, and fixed it
with the mandatory `max_block_size=1` stamp; every re-measured case then beat
the array-fold, including the one that OOMed. The negative measurement was
taken under the unfixed shape, so it is stale rather than a verdict on the
fixed one — but promotion to auto needs a fresh A/B against the fixed shape,
including mixed-shape queries, since the block-size stamp applies to the whole
statement.

## Why the two `*_merge_summap` features are opt-in

`classic_bucket_merge_summap` first shipped with a correctness gap on
heterogeneous groups, closed by
[#2817](https://github.com/tsouza/cerberus/issues/2817) (each row now
cumulates over its own buckets before the key-wise `sumMap`). The per-row
`arraySort` / `arrayCumSum` that fix added erased the speedup the feature
existed for: [#2923](https://github.com/tsouza/cerberus/issues/2923)'s
real-ClickHouse re-measurement against the shipped construction found its
cost within ~1% of the fold's at every controlled point, converging to parity
as volume grows. The existing `maxClassicBucketMergeCostUnits` guard protects
this path within the same margin, so no second ceiling exists.

`exp_histogram_merge_summap`'s reconstruction step is width-squared in the
worst case, independent of row count.
[#2757](https://github.com/tsouza/cerberus/issues/2757)'s real ClickHouse 26.6
measurements found 13-43x less memory at the ~160-bucket OTel-SDK default
width once rows reach the hundreds-to-thousands range, roughly parity at a
single series, and MORE memory than the fold for a single series with a wide
individual layout (1,280 buckets and up). The budget guards are per shape
(single-group, multi-group instant, multi-group range), but the single-series
wide-layout regression is a property of the design, not a calibration gap.

## Why `arg_and_max_fusion` and `ts_throw_duplicate_series_if` are auto-enabled

Both are tie-invariant substitutions with no operator tradeoff: ClickHouse
keeps one hash-table entry per GROUP BY key regardless of aggregate count and
per-group memory is dominated by the Attributes `Map` key, so `argAndMax`
saves one aggregate state and one per-row comparison — roughly a third of the
two states' payload, not a halving; `timeSeriesThrowDuplicateSeriesIf` changes
only the message of a query that aborts either way. Both are `experimental` in
maturity purely because the underlying functions are new (25.11 and 26.2) with
no fielded history. `argAndMax`'s floor is 25.11 from upstream PR
[#89884](https://github.com/ClickHouse/ClickHouse/pull/89884) and the 25.11
release notes, not the "v1.1.0" badge on its docs page, which is a
docs-tooling artefact.

## Why `ts_tag_groups` is permanently opt-in

The mechanism is sound — order-independent group ids, lossless rehydration,
a verified 26.2 floor with no experimental setting — but it loses on every
site tried. [#2750](https://github.com/tsouza/cerberus/issues/2750)'s real
ClickHouse 26.2 A/B (2M rows, `FORMAT Null`) on the name-drop guard measured
the UInt64-group-id path 2-3x slower at 100 distinct label sets (666-1083ms vs
265-305ms) and ~50% slower at 200,000 (1358-1506ms vs 881-957ms), with peak
memory ~1.8x higher at low cardinality and only 10-20% lower at high.
[#2880](https://github.com/tsouza/cerberus/issues/2880) re-ran the same
methodology on the strongest multi-stage candidate — the one-to-one
vector-vector JOIN — and it lost by a wider margin (~2.2x slower, 3-4x more
memory at low cardinality; ~1.9x slower, 25-30% more memory at high): the
per-query tag collector's bookkeeping is paid per input ROW on each side
before any reduction, while the cheaper UInt64 equality applies only to the
already-reduced comparison, so more downstream reuse cannot amortise it. The
label ops the tag-group family also offers (`by` / `without` /
`label_replace` / `group_left`) are excluded for a second reason: a group id
is per-query state, every label op reaches range mode, and route B
concatenates per-shard cursors without re-aggregating, so an id cannot cross
a shard.

## Why `trace_id_external_table` is opt-in

The external-table form was EXPLAIN-verified
([#2783](https://github.com/tsouza/cerberus/issues/2783)) to prune
`idx_trace_id` identically to the literal splice at every closure size tested,
so it fixes only the SQL-text-size axis, and its production win at the
`MaxSearchLimit=1000` phase-A width is unmeasured beyond that issue's synthetic
corpus — the same posture `trace_id_projection` took on a fresh mechanism that
only a real server exercises.

## Why native regular-expression compilation has no `chopt` entry

ClickHouse #108004 (merged into 26.7.1.459) turns compilation on by default
and falls back to RE2 outside its subset, so the choice a cerberus knob could
make — on or off — is one the server already makes well. A `chopt` feature
exists to stamp a setting that is off by default, or to gate one that is
wrong on some builds. Neither holds: the evidence below found no divergence on
valid UTF-8 between compiled and interpreted evaluation for any emitted shape,
and on invalid UTF-8 the compiled engine is the one that agrees with the
reference engines. A knob that could only restate the server default, or
switch the more reference-faithful engine off, would be machinery without a
decision behind it.

What cerberus controls is the spelling of the patterns it binds, and that is
where the change landed.

### Why `label_replace` is anchored as `(?s)^(?:re)$`

Prometheus's `^(?s:re)$` was bound verbatim, and the compiler's parser
rejects a scoped flag group, so no `label_replace` regex ever compiled. The
two spellings differ only in where `s` is set: `(?s)` sets it for the whole
pattern, `(?s:...)` for the group. The group spans all of `re` whenever `re`
parses on its own, and outside it there are only `^` and `$`, which read no
flag `s` changes, so the programs are identical — `internal/chsql/regex_pattern_test.go`
checks it by program equality and by submatch indexes. `(?s)` rather than
dropping the flag, because `replaceRegexpOne` / `replaceRegexpAll` do not set
`.`-matches-newline on 24.8 (measured: `replaceRegexpOne('a\nb', '^(?:a(.)b)$',
'[\1]')` returns `a\nb` there and `[\n]` on 26.7), so the flag must travel
in the pattern. A regex whose `)` closes the reference group early (`a)|(b`)
would put text outside the group under `(?s)`, so it keeps the reference
spelling — that shape has a separate, pre-existing defect,
[#3663](https://github.com/tsouza/cerberus/issues/3663).

### Why a line filter's `.` is spelled `[^\n]` and not prefixed with `(?-s)`

The semantic corpus found that `{...} |~ "a.b"` kept a line `a\nb` that Loki
drops: Loki compiles a line filter with Go's default flags, ClickHouse's
`match()` with `.` matching a newline. Both `(?-s)<re>` and the `[^\n]`
respelling restore Go's meaning; only the second stays compilable — on 26.7
and 26.8 `(?-s)api.*`, `(?-s).+` and `(?-s)host-(.*)` fall back to RE2 while
`api[^\n]*`, `[^\n]+` and `host-([^\n]*)` compile. The respelling is done
by a small scanner, and is used only when the result parses to the program
the input has under Go's defaults, so a scanner mistake costs compilability,
never meaning; `(?-s)` is the fallback. A pattern that sets or clears `s`
itself is left to the fallback, since there a `.`'s meaning depends on its
position.

### Shapes that stay on RE2

- **Alternation** — `job=~"api|web"`, the shape a Grafana multi-value
  variable produces, and `|~ "error|timeout"`. The subset has no alternation.
- **The label-name sanitizer** `replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_')`,
  the most frequent emitted regex (every Loki label key). A negated class can
  match a non-ASCII byte, so with a count of one it falls back; it has to, since
  RE2 replaces a whole multi-byte code point with one `_`, which is Loki's
  behaviour. Measured over 20M keys on 26.7.13.12, single thread: 6.1 s with
  compilation off and 6.0 s on. A two-pass spelling that sends the ASCII class
  through the compiler (``[\x00-/:-@\[-^`{-\x7f]``, then `[^\x00-\x7f]`)
  measured 8.0 s off and 10.1 s on, so it was not adopted.
- **Named groups**, **anchors or flags inside the user's own pattern**
  (`^api` becomes `^(?:^api)$`), **a fixed count on `.`** (`status=5..`),
  **pure literals and the empty pattern** in a line filter (RE2's substring
  search already handles those), and **`extractAllGroupsHorizontal`**.

### Why the tests switch the query condition cache off

26.x's query condition cache (`use_query_condition_cache`, on by default)
remembers which granules a filter rejected. The corpus runs each query several
times in different modes; with the cache on, a later request skips the
granules an earlier one proved empty and never evaluates its regex there — the
first version of the test read "not compiled" for exactly the stream selectors
that matched no seeded row. The cache is orthogonal to regex evaluation, so
the tests and the benchmark turn it off to measure the mode under test.

### What the benchmark measured

`just regex-jit-bench` (`BenchmarkRegexJIT`) on ClickHouse 26.7.13.12 (official
build), in a container limited to 2 CPUs and 6 GiB, with the server's default
`compile_regular_expressions=1` / `min_count_to_compile_regular_expression=3`
against `compile_regular_expressions=0`, and `use_query_condition_cache=0`.
Seed: one hour of gauge samples every 10 s for 20,000 pods (7.2M rows) and
10M logfmt log lines. Every request is a 1-hour range query at a 1-minute
step through the production handler; each cell is the mean over three
iterations of server CPU time (user + system, from `system.query_log`) per
request — per batch of eight simultaneous requests for "concurrent". Warm
repeats one query; cold drops the compiled-code cache before each request;
churn sends a pattern the server has not seen on every request. The host was
shared with other work, so differences under about 5% are noise.

| Emitted shape (bound pattern)                                            | Warm off → on         | Cold off → on        | Churn off → on       | Concurrent ×8 off → on |
| ------------------------------------------------------------------------ | --------------------- | -------------------- | -------------------- | ---------------------- |
| PromQL matcher `pod=~"api-.*"` (`^(?:api-.*)$`)                          | 6616 → 4979 (−25%)    | 8371 → 7836 (−6%)    | 8117 → 6602 (−19%)   | 56724 → 48304 (−15%)   |
| PromQL matcher with alternation `api-.*` or `none-.*` (RE2 either way)   | 5695 → 5356 (−6%)     | 5622 → 5438 (−3%)    | —                    | 56060 → 56163 (0%)     |
| `label_replace` with `(.*)-[0-9]+-.*` (RE2) over the matcher above       | 5645 → 4966 (−12%)    | 5736 → 5440 (−5%)    | 5895 → 5263 (−11%)   | 60418 → 61051 (+1%)    |
| Line filter `timeout after [0-9]+ms`, unguarded                          | 4936 → 5862 (+19%)    | 4370 → 5198 (+19%)   | 4329 → 5315 (+23%)   | 46160 → 55462 (+20%)   |
| Line filter `timeout after [0-9]+ms`, `position()`-guarded               | 4865 → 3950 (−19%)    | 4558 → 4148 (−9%)    | 4068 → 3891 (−4%)    | 44996 → 43777 (−3%)    |
| Line filter `user=.*admin` (`user=[^\n]*admin`, guarded)                 | 6535 → 6371 (−3%)     | 6975 → 6580 (−6%)    | —                    | 73431 → 74370 (+1%)    |
| `unwrap duration(latency)` after a `timeout` substring filter            | 34156 → 34386 (+1%)   | 34391 → 35639 (+4%)  | —                    | 420826 → 397778 (−5%)  |

In a whole emitted query the regex is one cost among the scan, map access and
range-window work, so the gains are partial: the anchored matcher, the most
common PromQL shape, saves about a quarter of the request's CPU when warm.

**The unguarded line filter row is a regression the defaults cause.**
ClickHouse's RE2 path searches a pattern's required literal with a vectorised
substring search before it runs the regex; the compiled path runs its matcher
on every line instead. Measured directly over the same 10M lines, single
thread: `match(b, 'timeout after [0-9]+ms')` 0.9–1.1 s interpreted against
1.8–2.2 s compiled, and `match(b, 'latency=[0-9]+ms')` — a literal on every
line, so RE2 gets no help — 2.2 s interpreted against 1.1–1.6 s compiled.
`position(b, 'timeout after ') > 0 AND match(...)` measured 0.8 s compiled and
1.0–1.2 s interpreted, so the emitter guards every line filter whose matches
share a literal prefix. On a server without the compiler the guard repeats a
search RE2 does anyway, at the cost of one substring scan.

**Compilation is cheap and paid once per pattern per server.** A 4-row
`match()` right after `SYSTEM DROP COMPILED EXPRESSION CACHE`, with the
threshold at 0, took 10.4 ms against 3.7 ms interpreted for `^(?:api-.*)$`,
15.5 ms against 2.9 ms for `(?s)^(?:host-(.*))$` and 20.6 ms against 5.3 ms
for `timeout after [0-9]+ms`; the churn column shows a pattern compiled on
first sight still pays for itself inside one request over this volume. At the
default threshold of 3 a single request over many blocks crosses it within
the request.

**Microbenchmarks behind the subset analysis** (26.7.13.12, single thread):
upstream's own URL shape `match(URL, '^https?://(?:www\.)?([^/]+)/.*$')` over
20M rows ran 4.9–6.9 s interpreted and 0.9–1.2 s compiled. A `label_replace`
regex such as `(.*)-[0-9]+-.*` — a greedy group followed by a literal the
group can also consume — is not compiled in either spelling, which is why the
`label_replace` row's saving is the matcher's; `host-(.*)` and
`(\w+)(-(\d+))?` are compiled.

## Why `query_log_union` is opt-in and never fatal

The packet path already observes every query the dispatching process armed
for capture, on every shard and across every log rotation, so the union adds
rows only outside that coverage: stamped queries dispatched without capture,
and other processes' queries. Which of those an operator wants a pod to learn
from is a deployment decision, and the server half (the union section and
the grant) is the operator's to provision — so `auto` never selects it. A
blocked probe degrades rather than failing an explicit request under
`enforcing` because the local log is a complete fallback for every row it can
hold, and ClickHouse documents `system.all_query_log` as safe to drop at any
time and recreated automatically, so a boot-time refusal says nothing
permanent about the deployment. The measurements behind this are in
[`solver.background.md`](solver.background.md#query-log-source-what-the-local-log-misses-measured).

## Why `move_to_prewhere` is auto only on servers from 24.11 to before 26.2

The emitter's own `PREWHERE` split (promote the cheap, narrow conjuncts of a
filtered scan whose projection reads a wide column) came from the
pre-release hardening pass, whose stated aim was to discard granules before
the fat payload columns are read; it carried no measurement. An explicit
`PREWHERE` switches the server's `optimize_move_to_prewhere` off for that
scan, so whatever the emitter left in `WHERE` — the label matcher on the
`Attributes` map, and the time bounds an outer range-window layer pushes
down — is evaluated only after every row of the surviving granules is read
with all its columns. On the exporter's own `ServiceName`-first sorting key
the metric name does not prune well, which is how the split lost to the
server's (cerberus issue #3955).

Two fixes were measured against today's split ("current"): no explicit
`PREWHERE` at all, leaving the split to the server ("none"), and an explicit
`PREWHERE` holding every conjunct the server would move ("all"). The data is
the nightly lane's counter sample (`svc_http_requests_total`, one method
matched), on cerberus's provisioned tables ("cerberus key"), the exporter's
DDL ("stock key") and the same tables renamed, plus synthetic logs and spans
for the Loki and Tempo heads; `use_query_condition_cache=0` where the server
has the setting; peak memory as the range over 6 repeats, MB.

| ClickHouse   | query (table)                            | current       | none          | all           |
| ------------ | ---------------------------------------- | ------------- | ------------- | ------------- |
| 24.8         | label values, matcher (stock key)        | 41.3–46.6     | 26.0          | 26.0          |
| 24.8         | rate by route 5m (stock key)             | 54.0–58.1     | 61.3–66.6     | 679.2–747.9   |
| 24.8         | rate by route 5m (renamed)               | 56.4–58.4     | 59.9–63.7     | 656.7–783.0   |
| 24.10        | rate by route 5m (stock key)             | 53.8–58.4     | 58.3–63.1     | —             |
| 24.11        | rate by route 5m (stock key)             | 56.7–57.4     | 43.2–45.7     | —             |
| 24.11        | instant selector, matcher (cerberus)     | 47.7–53.3     | 31.2          | —             |
| 24.11        | Tempo `&&` intersect                     | 77.7–87.7     | 81.8–94.9     | —             |
| 25.9         | label values, matcher (stock key)        | 35.8–40.1     | 25.9          | 25.8          |
| 25.9         | label values, matcher (renamed)          | 36.8–45.3     | 26.0          | 26.0          |
| 25.9         | rate by route 5m (stock key)             | 55.4–60.6     | 38.7–47.9     | 590.7–700.9   |
| 25.9         | rate by route 5m (cerberus key)          | 56.4–57.9     | 45.9–52.3     | 301.5–445.9   |
| 25.9         | Tempo span attribute + name              | 37.4–37.6     | 17.1          | 25.3–29.6     |
| 25.9         | Loki line filter 1h                      | 28.5–28.7     | 24.5–28.7     | 28.5–28.7     |
| 26.1         | rate by route 5m (stock key)             | 56.8–60.4     | 44.4–48.4     | —             |
| 26.2         | rate by route 5m (stock key)             | 57.0–59.0     | 372.9–459.9   | —             |
| 26.6         | rate by route 5m (stock key)             | 41.0–45.2     | 231.2–321.1   | —             |
| 26.6         | label values, matcher (cerberus key)     | 35.6–36.1     | 51.8–56.7     | —             |

"all" is several times worse than both on every rate query on every
version: the time bounds and the map matcher in `PREWHERE` are evaluated on
every row of the metric's granules before the range window narrows it, so
it was rejected outright. Between the other two neither dominates, and the
version decides which wins:

- Below 24.11 the server's move is mixed — it wins label values but loses
  the rate queries by 5–10 MB on the stock key and the renamed tables.
- From 24.11 to 26.1 (every minor between was bisected: 24.11, 24.12, 25.1,
  25.3, 25.9, 25.11, 26.1) "none" wins or ties every query on every table,
  both log and trace shapes included. On 25.9 the stock-key rate query reads
  846 MB instead of 2.08 GB.
- From 26.2 (26.2, 26.3 and 26.6 measured) the server's move turns the rate
  queries several times worse. `EXPLAIN actions=1` shows why: the server
  moves the same conjuncts on 25.9 and 26.6, but from 26.2 it orders the
  `Attributes` map-subcolumn matcher first, so the map is decoded for every
  row before the metric name and the time bounds discard any.

The emitter's split stays the default, then, and `move_to_prewhere` is
auto-selected only where the server's split measured not worse anywhere:
`>= 24.11` and `< 26.2`. An explicit listing still selects it on any server,
since the feature is result-equivalent and needs no capability. Loki's
filtered scans project no wide column, so neither choice changes a Loki
statement.

The perf-nightly corpus measures both sides of the decision: it runs on
24.8, 25.9 and 26.6, so the server places PREWHERE on one of its servers and
the emitter does on the other two. Its label-values sentinels on the stock
key and the renamed tables and its rate-by-route sentinel on the stock key
run with the query condition cache off, where the server has it, so a repeat
cannot read what an earlier one cached. Its self-check breaks the decision on
each side: forcing the emitter's split on 25.9 (label values 39 MB against a
31 MB ceiling), bypassing the emitter's partition so its PREWHERE holds the
attribute matcher on 24.8 and 26.6 (rate by route 696 MB and 416 MB against
58 MB and 40 MB unmutated), and widening the auto range past 26.6 (rate by
route 296 MB against 40 MB). The
in-order aggregation stamp is a neighbouring decision that peak memory does
not separate: on cerberus's key a hash aggregation of the instant selector
measured 116–124 MB against 101–116 MB in order, so its nightly sentinel
asserts `AggregatingInOrderTransform` in `EXPLAIN PIPELINE` of the
dispatched statement instead. On the exporter's key the stamp, right or
wrong, changes nothing: `EXPLAIN PIPELINE` is identical stamped and
unstamped (no `AggregatingInOrderTransform` either way, since `ServiceName`
leads the key) and peak memory overlaps (131.8–138.9 MB against
134.8–142.8 MB), so that decision is pinned by the chDB test
`TestDeployedMetricsSortingKeyDecidesAggregationInOrder` rather than by a
sentinel.

## Audited, not adopted

Not every settings family the audit epic (#2778) reviews earns a registry
entry. Recording an audit that found nothing to stamp is itself the useful
artifact — the alternative is the same family getting silently re-reviewed by a
future pass with no memory of this one.

### S3/remote-filesystem read tuning (prefetch + concurrent-read thresholds)

Production is a single node with S3-backed storage, so the working hypothesis
was that cerberus's cold dashboard scans need explicit tuning of ClickHouse's
prefetch and remote-filesystem concurrent-read settings. Probed live via chDB
(ClickHouse 26.5.1.1) rather than assumed from the settings' documented
defaults — `system.settings` reports:

| Setting                                                             | Live value | Live default |
| ------------------------------------------------------------------- | ---------- | ------------ |
| `remote_filesystem_read_prefetch`                                   | `1`        | `1`          |
| `allow_prefetched_read_pool_for_remote_filesystem`                  | `1`        | `1`          |
| `merge_tree_min_rows_for_concurrent_read_for_remote_filesystem`     | `0`        | `0`          |
| `merge_tree_min_bytes_for_concurrent_read_for_remote_filesystem`    | `0`        | `0`          |

Prefetch is already on. The remote-filesystem concurrent-read thresholds are
already at `0` — the most aggressive setting available, meaning ClickHouse
already runs EVERY remote-filesystem part read concurrently regardless of size,
unlike the local-disk counterparts (`merge_tree_min_rows_for_concurrent_read` /
`..._bytes_...`, which default to 163840 rows / 240 MiB — a real threshold,
because a local read is cheap enough that a small part isn't worth
parallelizing). ClickHouse's own defaults, on the exact deployment shape this
issue targets, are already tuned past what a `chopt` stamp forcing
`remote_filesystem_read_prefetch=1` or lowering an already-zero threshold could
add — the entire premise "stamp only where the default is off or measurably
wrong" resolves to neither being true. Per the audit-epic mandate (#2778) to
verify a proposal against the live server rather than assume it from
documentation alone, no `chopt` feature is registered for either family: it
would be machinery duplicating what the server already does unconditionally.

**The follow-up question this audit surfaced — settled negative (cerberus
issue #2827).** The issue's own "fan-out-heavy wide scans vs point lookups"
framing implied the OPPOSITE tuning direction might pay off: deliberately RAISING the
remote-filesystem concurrent-read thresholds (toward the local-disk defaults,
163840 rows / 240 MiB) on a detected narrow, highly-selective point-lookup
shape, to avoid the coordination overhead of spinning up concurrent S3 fetches
for a read that only touches a handful of granules. Benchmarked directly
against a real ClickHouse 26.8 server backed by a live MinIO (S3-compatible)
disk — 20M rows, a single `id = <literal>` point-lookup shape,
`clickhouse-benchmark` over 2,000 distinct random-id queries per configuration
with the mark cache dropped between runs: the default (`0`/`0`, max
concurrency) and the raised local-disk-matching thresholds measured
statistically indistinguishable throughput (83.577 vs 83.655 QPS, a ~0.1% gap,
well inside run-to-run noise). Smaller repeated trials (500 queries x 3) leaned
the OPPOSITE direction from the hypothesis — raised thresholds ~2-3% SLOWER,
not faster. A single-key point lookup only ever touches on the order of one
granule regardless of the concurrent-read threshold, so there is no
coordination overhead this setting family removes for that shape on real
S3-backed storage: the threshold governs whether a read gets SPLIT into
concurrent sub-ranges, and a read this narrow has nothing left to split either
way. No `chopt` feature is adopted; #2827 is closed with this evidence rather
than left open.

**Local filesystem cache** (`enable_filesystem_cache` + the server-side cache
disk) IS adopted by this issue, but as documented operator guidance plus
`/info` reporting rather than a `chopt` stamp — it is a server-config /
disk-sizing concern, not a per-query setting. See
[`docs/operations.md`](operations.md)'s "Local filesystem cache" section.
