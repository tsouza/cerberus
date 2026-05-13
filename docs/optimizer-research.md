# Optimizer research notes

Curated reading list for evolving cerberus's `internal/optimizer/` past the v0.1 seed (filter fusion, constant folding, projection pushdown). Each entry names a concrete technique to port and points at primary sources, not background reading.

When one of these is implemented, open a PR — the reference here is the PR description's starting point.

---

## 1. Canonical relational rules to port next

- **Apache Calcite — `org.apache.calcite.rel.rules` package.**
  [Javadoc index.](https://calcite.apache.org/javadocAggregate/org/apache/calcite/rel/rules/package-summary.html)
  The closest thing to a checklist. Top picks for cerberus: `FilterProjectTransposeRule` (push filter past Project — our current pushdown only fires on `Project(Scan)`), `FilterAggregateTransposeRule` (push filter past Aggregate when predicate references only group-by columns — critical for `sum by (job) (m{job="x"})`), `AggregateProjectMergeRule`, and `ReduceExpressionsRule` (a beefier constant-folder).
- **Apache Calcite: A Foundational Framework for Optimized Query Processing.** Begoli et al., SIGMOD 2018.
  [PDF.](https://arxiv.org/pdf/1802.10233)
  Section 5 spells out the pattern→transform rule contract and HepPlanner's bottom-up iteration to fixpoint — exactly cerberus's model. Useful spec when refactoring our `Rule` interface to a pattern-based form.
- **Querify Labs — "Rule-based query optimization."**
  [Blog post.](https://www.querifylabs.com/blog/rule-based-query-optimization)
  Practical taxonomy: *transpose* (push past), *merge* (collapse adjacent same-kind — our filter fusion is one), *simplify* (constant fold, identity elimination), *convert*. Lets you classify each new rule and decide traversal order per class.

## 2. Time-series-specific rewrites

- **VictoriaMetrics `metricsql/optimizer.go`.**
  [Source.](https://github.com/VictoriaMetrics/metricsql/blob/master/optimizer.go)
  The single highest-signal Go file for cerberus. `PushdownBinaryOpFilters` shows how to lift label matchers from one side of `a / b on(job)` and apply them to the other side at the AST level — translates directly to pushing `Filter` predicates across `Project`/`Aggregate` in our IR. The companion `optimizer_test.go` is a ready-made golden-test layout to mirror.
- **Promscale issue #152 — "Support continuous aggregates via PromQL."**
  [Discussion.](https://github.com/timescale/promscale/issues/152)
  Documents the design space for *pre-aggregation substitution*: rewrite onto a coarser materialised rollup when its bucket size divides the query step. Enumerates the alignment pitfalls (boundary offsets, `rate` over rollups, partial buckets). Cerberus's `RangeWindow` operator is exactly where this rule fires.
- **ClickHouse issue #57545 — "Support for PromQL."**
  [Thread.](https://github.com/ClickHouse/ClickHouse/issues/57545)
  ClickHouse maintainers' notes on why `rate`/`increase` over range vectors don't translate cleanly to SQL aggregations — they depend on ordered scan within a window. Read before designing real `RangeWindow` lowering: tells you which Prom functions need `arrayJoin`/`groupArray` patterns vs plain `GROUP BY toStartOfInterval`.

## 3. Columnar-store-aware rewrites

- **"The definitive guide to ClickHouse query optimization."**
  [ClickHouse engineering page.](https://clickhouse.com/resources/engineering/clickhouse-query-optimisation-definitive-guide)
  The ORDER BY / primary-key chapter is the one technique to internalise: *sort-key-aware filter ordering*. Emit predicates on the leading sort columns first in the `WHERE` so granule skipping fires. This is a codegen rule (lives in `internal/chsql`), not an IR rule — partition predicates into "matches sort prefix" / "matches skip-index" / "rest" and emit in that order. Same principle drives `PREWHERE` promotion.
- **"Selective Late Materialization in Modern Analytical Databases."** Tsinghua, VLDB 2025.
  [PDF.](http://people.iiis.tsinghua.edu.cn/~huanchen/publications/slm-vldb25.pdf)
  Explains *when* late materialization (defer fetching wide columns until after filter/limit) actually wins. Directly applicable to `Project(Limit(Filter(Scan)))` patterns and to OTel logs queries where `Body` / `ResourceAttributes` are fat — emit a projection that excludes wide columns until after the `LIMIT`, then JOIN back by row-id.
- **DuckDB blog — "Optimizers: The Low-Key MVP."**
  [Post.](https://duckdb.org/2024/11/14/optimizers)
  Walkthrough of DuckDB's optimizer pass order. Takeaway: filter pushdown → projection pushdown → constant folding → late-materialization rewrite. Our iterate-to-fixpoint loop will eventually hit a rule that needs explicit ordering; this post is the cheapest education on which.

## 4. Reference codebases — what to steal from each

- **Apache Calcite (Java).**
  [Repo.](https://github.com/apache/calcite)
  Steal: the `RelOptRule` pattern API and `HepPlanner` bottom-up traversal contract. Refactor cerberus rules from "function takes a tree, returns a tree" to "rule declares an operator pattern + transform on the match." See `core/src/main/java/org/apache/calcite/plan/hep/`.
- **DataFusion (Rust).**
  [docs.rs.](https://docs.rs/datafusion-optimizer/latest/datafusion_optimizer/)
  Steal: split rules into `AnalyzerRule` (semantic, must-run) vs `OptimizerRule` (heuristic). Our `ConstantFold` actually has two flavours — semantic (`WHERE 1=0` short-circuits to empty scan) and heuristic (`x+0 → x`); mixing them in one fixpoint loop hides bugs.
- **Spark Catalyst — `Optimizer.scala`.**
  [Source.](https://github.com/apache/spark/blob/master/sql/catalyst/src/main/scala/org/apache/spark/sql/catalyst/optimizer/Optimizer.scala)
  Steal: the `Batch` abstraction. Group rules that should iterate together to fixpoint, then move to the next batch. Cerberus's single fixpoint loop will eventually thrash (rule A creates work for rule B which re-creates work for rule A); batching gives you a place to break the cycle without going cost-based.
- **DuckDB optimizer.**
  [Source.](https://github.com/duckdb/duckdb/tree/main/src/optimizer)
  Steal: the *filter pushdown* implementation. Self-contained C++ visitor that handles all the cases (push past join, project, set ops, subquery) cerberus will eventually need. Translates to idiomatic Go more easily than Calcite's class hierarchy.
- **Trino — "Connector pushdown" docs.**
  [Page.](https://trino.io/docs/current/optimizer/pushdown.html)
  Steal: separate "what the engine can rewrite" from "what the source can absorb." For cerberus the source is ClickHouse SQL: rules in `internal/optimizer/` should be source-agnostic; ClickHouse-specific rewrites (sort-key ordering, `PREWHERE` promotion, `arrayJoin` patterns) belong in the codegen layer. Trino's doc is the clearest argument for the separation.

## 5. Prom/Loki/Tempo → SQL prior art

- **SigNoz query-service architecture.**
  [DeepWiki.](https://deepwiki.com/SigNoz/signoz/4-query-service)
  Closest production analogue. Builds CH SQL from a structured query-builder IR and accepts raw PromQL. The pitfall they document: when both a PromQL `time` range and CH `Timestamp` predicates apply, you need a single canonical bucketing function (`toStartOfInterval`) and all step-aligned rewrites must round to the same epoch. Source: `pkg/query-service/app/` in the signoz repo.
- **Uptrace — PromQL compatibility notes.**
  [Page.](https://uptrace.dev/get/promql-compat.html)
  Enumerates the corner cases of `rate` / `increase` / `irate` over CH-stored series — counter resets and out-of-order samples break naive `(last - first) / window` translations. The spec for the failure modes when implementing real `RangeWindow` SQL.

## 6. When does rule-based break down?

- **"An Overview of Query Optimization in Relational Systems."** Chaudhuri, PODS 1998.
  [PDF.](https://www.microsoft.com/en-us/research/wp-content/uploads/2016/02/pods98-tutorial.pdf)
  Still the canonical statement of the rule-vs-cost line. Decisions a fixpoint rule set cannot make: (a) join ordering beyond two relations, (b) materialised-view selection among overlapping candidates, (c) predicate ordering when predicates have very different selectivities. Cerberus hit (b) first in RC3 R3.6 — see the Jindal entry below.
- **"Selecting Subexpressions to Materialize at Datacenter Scale."** Jindal et al., VLDB 2018.
  [PDF.](http://www.vldb.org/pvldb/vol11/p800-jindal.pdf)
  The precise problem cerberus faces once multiple OTel rollups (`_5m`, `_1h`, raw) are in play: given a query and overlapping candidate materializations, pick the subset. Section 4 (subexpression matching) and section 6 (cost model) are the parts to read. Addressed in v1 by `internal/optimizer.MVSubstitution` (RC3 R3.6, #198) with a simple-heuristic `firstApplicable` cost model — picks the first registry-listed rollup that passes the four safety conditions (step ≥ window, range a multiple of window, outer aggregate commutes with rollup AggOp, rollup exists for the base table). The `costModel` interface is stubbed so v2 can swap in a real estimator without touching the rule.

---

## Suggested implementation order

1. Port the three Calcite transpose rules (§1) — shipped via #177 (RC3 R3.2).
2. Mirror VictoriaMetrics's `PushdownBinaryOpFilters` test structure (§2) — gives us a known-good fixture pattern.
3. Adopt Spark Catalyst's `Batch` grouping for the fixpoint driver (§4) — prevents thrashing as the rule set grows. (RC3 R3.3.)
4. Add a sort-key-aware filter emitter in `internal/chsql` (§3) — first ClickHouse-specific codegen rule. (RC3 R3.4 — PREWHERE promotion.)
5. Real `RangeWindow` SQL via `groupArray` / ordered-scan patterns (§2 + §5) — shipped through RC1 M1.1 and refined through RC2.
6. MV substitution for `otel_metrics_*` rollups (§2 Promscale + §6 Jindal) — the milestone where cerberus commits to a cost model. Shipped via #198 (RC3 R3.6) with a simple-heuristic v1; the `costModel` interface stub keeps the seam open for a real estimator in a future RC.

The pattern-based `Rule` API itself shipped via #135 (RC3 R3.1) — every new rule above lands on top of that API.
