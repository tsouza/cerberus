# Changelog

All notable changes to cerberus will be documented in this file. The format roughly follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), with one entry per tagged release.

## [Unreleased]

### Added

- **chsql,promql,chopt:** opt query_range changes()/resets()/irate()/idelta() onto a lagInFrame annotation pass with fixed-size per-anchor accumulators, retiring the array-fold fan-out for those shapes (#2759)

## [v1.19.0] — 2026-08-30

### Fixed

- **ci:** gate release PRs on CHANGELOG.md staying current with their own commits (#2740)
- **ci:** partition internal/promql's unsharded test suite across roundtrip-promql-shard legs (#2738)
- **ci:** relieve roundtrip-promql-shard's CPU contention and timeout budget
- **chsql:** bound emitted SQL at ClickHouse's own max_query_size (#2734)
- **promql:** answer an outer range fn over the doubly-nested subquery composition (#2732)
- **promql:** thread histogram/mixed shape through doubly-nested subquery composition (#2727)
- **promql:** close further and/unless/or wrapping a mixed-or subquery (#2724) (#2725)
- **promql:** close remaining sum/avg-wrapped mixed-or subquery gaps (#2714, #2715) (#2723)

### Documentation

- **release:** backfill v1.19.0's CHANGELOG/chart annotation for two late fixes
- **promql:** correct widenSubquerySpine's VectorSetOp nesting rationale (#2735)

## [v1.18.1] — 2026-08-29

### Fixed

- **promql:** compose resets/changes over sum/avg-wrapped mixed-or subquery (#2718)
- **solver,engine:** gate the per-rung predictive bypass on observed evidence (#2717)
- **ci:** widen the compat comparer's per-query deadline for the floor lane (#2713)
- **promql:** wire the mixed-or recognizer into absent()/limitk/limit_ratio/unary (#2711)
- **engine:** apportion route-B's RangeBucketGridNative bounds by shard count (#2712)

## [v1.18.0] — 2026-08-28

### Added

- **chaudit:** audit a live deployment against the histogram resource bound (#2691)
- **chsql:** carry the guard's own numbers in its rejection message (#2693)

### Fixed

- **chaudit,solver,ci:** apply the v1.18.0 pre-release audit findings (#2706)
- **routememo:** require corroboration before BothFail, and record why Key fuses group-by (#2701)
- **chopt:** re-probe fast while pinned to the capability floor (#2702)
- **solver,promql:** let the classic-histogram ladder route predictively (#2689)
- **config,chclient,engine:** derive the histogram density bound from the query memory cap (#2686)
- **solver:** let route B time-shard the native classic-histogram ladder (#2683)

### Performance

- **promql:** decide the DELTA arm's peak flag at construction, not by a walk (#2690)

### CI

- **mutation:** gate the timed-out share, which efficacy cannot see (#2703)
- warm nested go modules, and gate that a built module is a warmed one (#2704)
- **mutation:** pin the gremlins per-mutant budget to the declared ceiling (#2698)
- warm the Go module cache through a shared setup-go composite with fetch retry (#2699)

## [v1.17.0] — 2026-08-27

### Added

- **chsql,promql:** make five more resource-bound safety ceilings configurable (#2668)

### Fixed

- **chaos:** recalibrate route-memo-activation's memory cap with real margin (#2669)
- **chsql:** recalibrate RangeBucketGridNative's density axis against real production spill settings (#2666)

### Documentation

- document the solver-tuning env vars, fix two stale doc refs (#2673)
- **config:** document issue #2667's 5 resource-bound env vars (#2672)

## [v1.16.1] — 2026-08-26

### Fixed

- **chaos:** widen route-memo-activation's detection deadline to 180s (#2660)
- **chaos:** fix route-memo probe metric description and gate the decline dump (#2659)
- **routememo:** give the chaos lane's route-memo-activation scenario real historical depth (#2658)
- **routememo:** keep the chaos lane's pinned query off route B's ordinary auto-route (#2656)
- **deltaprefix:** detect and warn on backfill days already past the aggregate table's TTL (#2657)
- **chsql:** give RangeBucketGridNative's resource bound real production headroom (#2653)
- **e2e:** narrow the chaos lane's CH memory cap so route-memo-activation fires (#2649)

### Documentation

- **test-strategy:** document chdb regression-pin/untagged-companion pairing (#2655)

## [v1.16.0] — 2026-08-26

### Added

- **test:** shard the rejection-parity catalogue by function, not file (#2564)
- **promql:** group_left()/group_right() over two mixed-or operands (#2542)
- **promql:** compose vector-vector comparisons over a mixed or's both operands (#2533)
- **chsql:** extend the exact DELTA-prefix aggregate mechanism to query_range (#2530)
- **promql:** compose vector-vector arithmetic over two mixed or operands (#2521)
- **promql,chsql,chplan:** window-slide anchor-injection classic-histogram fast path (#2493)
- **perf:** add a derived native-histogram sentinel to the nightly lane (#2491)
- **schema:** DELTA-prefix aggregate table schema, DDL, and backfill CLI (#2485)
- **perf:** render a clear pass/regression verdict in perf-nightly's step summary (#2455)
- **promql:** widen exp-histogram float-vector scaling to on()/ignoring()/group_left/right (#2450)
- **perf:** add the periodic mutation self-check for the nightly perf gate (#2441)
- **perf:** gate the #2370 nightly lane and settings changes on sentinel coverage (#2436)
- **perf:** load the real production sample into a nightly measurement harness (#2430)
- **perf:** add a real-ClickHouse perf-smoke sentinel differential (#2370 PR 1) (#2410)

### Fixed

- **promql:** thread rate's per-second factor into the native quantile window (#2644)
- **ci:** give coverage-chdb real headroom over its actual runtime (#2643)
- **ci:** gitignore the patches/ CI artifact-staging directory (#2642)
- **ci:** give post-merge-drift real headroom over its actual runtime (#2632)
- **test/spec:** widen exp-histogram interpolation ULP tolerance detection (#2630)
- **ci:** bump roundtrip's per-leg go-test timeout from 12 to 25 minutes (#2628)
- **promql:** pre-release audit fixes batch (findings 1-14, A-E) (#2624)
- **test/spec:** let RunParity's comparator selection tolerate non-PromQL queries (#2623)
- **promql:** compose histogram value functions and info() over a mixed or (#2620)
- **promql:** compose avg/sum/first/last/ts_of_first/ts_of_last_over_time under outer aggregation (#2621)
- **promql:** compose sum/avg-wrapped mixed-or subqueries for 11 of 15 fns (#2616)
- **promql:** compose timestamp/scalar/sort_by_label over a mixed float/histogram or (#2614)
- **promql:** compose date/time functions over a mixed float/histogram or (#2612)
- **promql:** let absent()'s subquery form skip the exp-histogram check (#2608)
- **promql:** compose sort()/sort_desc() over a mixed float/histogram or (#2607)
- **promql:** compose topk/bottomk/quantile over a mixed float/histogram or (#2606)
- **test/spec:** add a 2-ULP tolerance for pow, same class as atan2 (#2604)
- **promql:** compose count/group/min/max/stddev/stdvar/count_values over a mixed or (#2601)
- **promql:** retry the drop-family recognizer for a subquery's own inner (#2603)
- **promql:** retry count_over_time/present_over_time exp-histogram recognizer under nested wrappers (#2599)
- **promql:** join a regex/negated info() second-arg exp-histogram selector (#2594)
- **promql:** stop discarding a histogram/mixed subquery set-op payload (#2597)
- **promql:** compose the clamp family over a mixed float/histogram or (#2596)
- **promql:** support unary minus/plus over an exp-histogram-valued operand (#2593)
- **promql:** compose SELECT/FOLD-family fns over a label_replace-wrapped mixed-or subquery inner (#2592)
- **promql:** compose round()'s 2-arg to_nearest form over a mixed-or histogram (#2588)
- **promql:** let info()'s second-argument selector name an exp-histogram metric (#2586)
- **promql:** recognize limitk/limit_ratio over exp-histogram as a preserved shape (#2585)
- **promql:** compose the SELECT/FOLD-family outer fn over a mixed-or-inner subquery (#2582)
- **promql:** compose an and/unless-forwarded histogram into an outer or (#2580)
- **promql:** compose SELECT/FOLD-family fns over a nested histogram subquery (#2579)
- **promql:** thread histogram-aware count_values into lowerCountValues (#2576)
- **promql:** compose a mixed float/histogram or nested under a further setop (#2573)
- **promql:** compose histogram_quantile/aggregations over a nested topk/bottomk drop (#2572)
- **promql:** drop float-only range-vector fns to empty over a native-histogram subquery (#2570)
- **promql:** compose a mixed float/histogram or over a plain vector operand (#2558)
- **promql:** accept a set-op operand in the exp-histogram +/-/==/!= binops (#2560)
- **promql:** accept a set-op operand in exp-histogram scalar binop lowering (#2561)
- **promql:** support the select/count family over a histogram-native subquery (#2552)
- **promql:** read a histogram-valued nested argument in native value functions (#2556)
- **promql:** retry resets()/changes()/count()/group() when nested over an exp-histogram (#2553)
- **promql:** drop histogram-involving pairs for ^/%/atan2 over mixed-or operands (#2550)
- **promql:** answer a bare top-level matrix selector over a native histogram (#2551)
- **promql:** route a subquery's inner expression through histogram-native lowering (#2546)
- **promql:** reconcile bucket layouts for hist,hist +/- over mixed-or operands (#2547)
- **promql:** recognise the scaling-join shape as histogram-valued for aggregation (#2544)
- **promql:** allow the histogram operand on either side of a scaling join (#2541)
- **promql:** retry exp-histogram preserve/drop recognizers on a binop operand (#2538)
- **chplan,chsql,promql:** remove RangeBucketWindowSlide, root-caused unfixable (#2536)
- **promql:** retry the exp-histogram drop family on a nested sub-expression (#2535)
- **chsql:** add a density-aware second bound to RangeBucketGridNative (#2531)
- **promql:** preserve a histogram-valued input through limitk/limit_ratio (#2529)
- **chsql,api/prom:** recalibrate RangeBucketGridNative's bound and wire its 422 mapping (#2524)
- **promql:** answer scalar NaN for a histogram-valued scalar() argument (#2519)
- **promql:** rewrite exp-histogram merge picker from rescan to direct slice (#2513)
- **promql:** join/preserve a histogram-valued base vector in info() (#2516)
- **promql:** scale a histogram-shaped row when MUL/DIV wraps a mixed or (#2510)
- **promql:** disable RangeBucketWindowSlide for sum_over_time pending #2511 (#2512)
- **promql:** drop exp-histogram samples for the date-component functions (#2508)
- **promql:** filter non-finite ExplicitBounds in classic-histogram fan-out merge (#2502)
- **promql:** compose scalar comparisons over a mixed histogram/float or (#2506)
- **chsql:** add RangeBucketGridNative resource bound and fix unsorted-bounds quantile (#2504)
- **promql:** support ts_of_first/last/max/min_over_time over exp-histograms (#2503)
- **promql:** guard the histogram-merge cost check against Int64 overflow (#2505)
- **promql:** resource-bound guard for exp-histogram cross-series merge, overflow-safe (#2501)
- **promql:** support ts_of_first/last/max/min_over_time over exp-histograms (#2499)
- **perf:** tighten nightly headroom per-sentinel using measured noise (#2494)
- **test:** sort the reference oracle's samples with cerberus's own labelKey (#2489)
- **perf:** bump real-CH image floor and fix the classic-histogram nightly sentinel (#2488)
- **promql:** compose drop-family arithmetic binops over a mixed histogram/float or (#2483)
- **promql:** support min/max/count/present/last/first_over_time over exp-histograms (#2484)
- **promql:** drop exp-histogram samples for float-only range reducers (#2481)
- **promql:** compose single-arg math functions over a mixed histogram/float or (#2479)
- **promql:** answer timestamp() over an exp-histogram-valued argument (#2478)
- **promql:** compose label_replace/label_join over a mixed histogram/float or (#2476)
- **promql:** preserve exp-histogram samples for sort_by_label()/sort_by_label_desc() (#2462) (#2475)
- **promql:** accept a both-histogram set-op input to label_replace/label_join (#2473)
- **promql:** make the dedicated-column overlay win structurally, not by mapConcat ordering (#2472)
- **promql:** accept a windowed float side in a sum/avg-wrapped mixed or (#2464)
- **chsql:** give RangeLWR its own fan-out threshold, separate from RangeBucketFanout's (#2471)
- **promql:** drop native-histogram samples for sort()/sort_desc() (#2463)
- **chsql:** bound RangeBucketFanout/RangeLWR sample fanout row count (#2447) (#2459)
- **scripts:** auto-expand a narrow golden-shard dispatch to its full DAG (#2460)
- **promql:** answer absent() over a raw exp-histogram selector (#2457)
- **promql:** accept a windowed float side in a mixed histogram/float or (#2333) (#2446)
- **ci:** quote step names truncated by an unquoted YAML `#` comment (#2454)
- **promql:** compose a mixed exp-histogram or under sum/avg (#2346) (#2451)
- **engine:** charge histogram range-fanout subquery grids against the anchor budget (#2448)
- **promql:** bound the exp-histogram binop merge's bucket-width fan-out (#2445)
- **promql:** drop native-histogram samples for clamp/clamp_min/clamp_max (#2444)
- **ci:** skip chdb.yml's redundant push run after a release PR merge (#2440)
- **ci:** skip perf-profile.yml's redundant push run after a release PR merge (#2439)
- **ci:** skip property.yml's redundant push run after a release PR merge (#2438)
- **chclient:** classify throwIf guard aborts by typed exception code (#2434)
- **chsql:** bound range-window sample fanout to close the #2429 OOM (#2433)
- **ci:** retry a proxy.golang.org 403 as a transient registry fault (#2432)
- **engine/promql:** bound the native-histogram merge's series/bucket-width fan-out (#2427)
- **ci:** skip coverage.yml's redundant push run after a release PR merge (#2425)
- **ci:** resolve the source PR before release-preflight demands fresh check-runs (#2417)
- **ci:** close the update-golden.yml migration-shard and merge-race gaps (#2368, #2350) (#2415)
- **release:** re-validate release-staging PR's own dashboard result (#2361) (#2413)
- **chsql:** answer an exhausted native rank walk from the iterator's bucket edge (#2407)

### Performance

- **promql:** bound the classic-histogram cross-series bucket merge (#2520)
- **schema:** index AggregationTemporality to prune the rate() split's redundant scan (#2465)
- **chsql:** fuse sum()/count() aggregation over a RangeLWR fan-out (#2452)
- **ci:** shard cardinality golden regen across CI matrix jobs (#2419)
- **ci:** shard perf-profile.yml's corpus fan-out walk across a matrix (#2421)
- **promql:** lower the classic-histogram rate fold onto a native grid ladder (#2401)

### CI

- **coverage:** move coverage-chdb's TestCardinalityRatchet fanout to its own matrix (#2646)
- **mutation:** rebalance phase2 to 4 legs, split phase4-promql into 12 (#2641)
- **chdb:** shard roundtrip (promql) across dedicated runners (#2638)
- **post-merge-drift:** shard the drift job's regeneration across a matrix (#2640)
- **coverage:** shard coverage.yml's coverage job across parallel runners (#2639)
- **e2e:** notify on a nightly that does not reach a clean pass (#1861) (#2418)
- **lint:** raise golangci-lint's timeout to a runaway guard, not a contention tripwire (#2412)

## [v1.15.4] — 2026-08-20

### Added

- **solver:** make auto start on route A and escalate on real evidence (#2398)

### Fixed

- **chsql:** rebase the native-quantile fraction in the rank walk's own direction (#2404)
- **engine:** activate the costly-cancellation route evidence and warn on the legacy env name (#2400)
- **solver:** stop auto-routing carriers whose peak does not divide with the grid (#2396)
- **chsql:** prune the DELTA prefix scan on a CUMULATIVE-only metric (#2395)

### Documentation

- drop deployment attribution from the solver and chsql comments (#2399)

## [v1.15.3] — 2026-08-19

### Fixed

- **solver:** sweep HistogramQuantile's GroupBy/PhiExpr and fix carrier depth (#2392)
- **chsql:** bound the DELTA-temporality prefix-reconstruction scan (#2390)
- **solver:** slice RangeBucketFanout, fixing classic-histogram range OOMs (#2387)

### Performance

- **promql:** bind the exponential-histogram merged bucket start once per merge site (#2386)

## [v1.15.2] — 2026-08-19

### Added

- **engine:** log a WARN line for every non-ok query exit (#2376)

### Fixed

- **chclient:** stop clickhouse-go from clobbering the query timeout setting (#2377)

## [v1.15.1] — 2026-08-18

### Fixed

- **chsql:** materialize native-quantile bucket-edge scans instead of re-deriving them (#2366)

### Performance

- **chsql:** materialise the classic histogram_quantile array walks once per row (#2367)

## [v1.15.0] — 2026-08-18

### Added

- **promql:** scale a histogram by a float-vector MUL/DIV operand (#2343)
- **promql:** answer mixed float/histogram and/or/unless operands (#2337)
- **promql:** answer 'or' between float and histogram operands (#2335)
- **promql:** answer group_left()/group_right() for exp-histogram binops (#2334)
- **promql:** answer on()/ignoring() matching and bool modifier for exp-histogram binops (#2329)
- **promql:** answer == and != between two exponential histograms (#2323)
- **ci:** add forbid-verbatim-concat, catch verbatim() shape-building (#2322)
- **promql:** support +/- between two exponential-histogram selectors (#2278)
- **promql:** answer scalar * and / over a native-histogram sample (#2179)
- **chplan:** add the sealed Fn vocabulary and chsql resolution table (#2176)
- **property:** cover merged native histograms (#2165)
- **qlcommon:** probe capture participation to widen label_replace's shared-name guard (#2124)
- **solver:** make the UnionAll/RangeWindowNative spine re-anchorable for route B (#2121)
- **promql:** answer resets(), changes() and count() over an exp-histogram selector (#2112)
- **promql:** answer avg() over an exponential histogram (#2088)
- **logql:** scan the log stream once for a multi-arm variants(...) query (#2049)
- **dash:** break the cerberus error rate down by status class and failure reason (#2042)
- **parity:** carry a native-histogram answer through the promql oracle (#2022)
- **promql:** answer rate()/increase() over a native histogram with a histogram-valued result (#2018)
- **promql:** answer sum() over a native-histogram selector with a histogram-valued result (#2015)
- **promql:** answer a bare native-histogram selector with a histogram-valued result (#2006)
- **spec:** check TraceQL txtar fixtures against the real Tempo engine (#1995)
- **spec:** check LogQL txtar fixtures against the real Loki engine (#1988)
- **spec:** check txtar fixtures against the real Prometheus engine (#1983)
- **chclient,chplan,chsql,api/prom:** native-histogram wire prerequisite (#1926 steps 1-3) (#1968)
- **compat/tempo-grpc:** add the gRPC-side status-parity axis (#1961)
- **compat/prometheus:** differential parity for the metadata match[] grammar (#1959)
- **tempo:** one shared metrics-pipeline core, and q= narrowing on the tag-name routes (#1929)
- **loki:** one field inventory for detected_fields, its values route, and unwrap (#1903)
- **promql:** subquery args, sample-preserving inner transforms, computed K (#1846)
- **chart:** per-head query.timeout override in split mode (#1834)
- **rejection-parity:** ratchet the divergence class's count and age (#1776)
- **rejection-parity:** add divergence class for deliberate, tracked backend gaps (#1767)
- **promql:** add wireArms classifier skeleton for histogram matcher domains (#1757)
- **compat:** add a status-code parity axis to the compatibility corpora (#1717)
- **compat/tempo:** add gRPC/h2c StreamingQuerier differential lane (#1676)
- **corpus:** record non-PromQL dispatch reasons (#1421)

### Fixed

- **test:** pin mixedIsHistogram in TestLocateSampleColumns expectations (#2352)
- **promql:** regenerate stale expected_rows for two mixed-or fixtures (#2349)
- **promql:** stop flattening asymmetric-shape and/unless set-op chains (#2347)
- **promql:** drop, don't reject, a float-vector op histogram-vector binop (#2332)
- **promql:** route and/or/unless between two exp-histogram operands (#2326)
- **compat:** reclassify closed histogram matching divergences to rejection (#2327)
- **promql:** hoist exp-histogram merge sort out of the per-bucket loop (#2317)
- **chsql:** render nested-set window frames via typed Window Frags (#2319)
- **promql,chplan:** answer histogram-histogram incompatible binops empty; split AggFunc combinators (#2313)
- **api/tempo:** add the reqctx.ApplyQueryTimeout backstop to every query entrypoint (#2318)
- **optimizer:** align mixed GroupBy passthrough policy for both transposes (#2316)
- **optimizer:** close constant-fold and dead-code audit findings (#2300)
- **promql:** reattach lowerVectorSelector doc comment and dedup modifier-strip (#2311)
- **routememo:** touch LRU on corroboration bump and stale-PreferB revalidation (#2309)
- **chsql:** render the structural-join depth bound via typed Lt/InlineLit (#2301)
- **ci:** give the mutation lane's gremlins job a deep checkout for its diff_ref (#2294)
- **ci:** widen the property histogram-sweep timeout margin and stop truncating its own failure log (#2292)
- **migrate:** strict-decode corpus.json on both read paths (#2288)
- **e2e:** gate the required dashboard check on dashboard-crawl-merge (#2279)
- **ci:** shard the coverage job's TestCardinalityRatchet corpus walk (#2276)
- **promql:** reconstruct DELTA first counter level (#2240)
- **ci:** fan the property job's histogram sweep across processes (#2266)
- **promql:** pin the native-histogram cross-series merge's fold order (#2258)
- **test/property:** stop the instant-window oracle rejecting empty series (#2265)
- **rejection-parity:** repoint the exp-histogram V-V-op divergence at #2263 (#2264)
- **promql:** compose native histogram consumers (#2245)
- **ci:** mark CodeQL required in the lane registry to match release.yml (#2259)
- **e2e:** concentrate the api-service log seed onto one drain cluster (#2255)
- **ci:** isolate main pushes from scheduled deep tests (#2252)
- **release:** require CodeQL on the release publish gate (#2256)
- **promql:** evaluate native histogram over-time reducers (#2232)
- **ci:** resolve downloaded golden patches from workspace (#2248)
- **promql:** support native histograms in delta and instant-rate functions (#2231)
- **promql:** allow absent_over_time over native histograms (#2228)
- **promql:** drop native histograms in float-only functions (#2229)
- **promql:** drop native histograms from float aggregations (#2227)
- **promql:** preserve native histograms through label_replace (#2222)
- **promql:** drop incompatible histogram scalar binops (#2217)
- **traceql:** ignore rootless spans in structural operators (#2215)
- **tempo:** tolerate incomplete tag discovery queries (#2212)
- **e2e:** seed current database span system attribute (#2216)
- **ci:** gate package floor enrollment on pull requests (#2214)
- **migration:** isolate MIG-18 probe reruns (#2211)
- **migration:** require evidence for every verify query (#2208)
- **coverage:** fail closed on package test errors (#2209)
- **loki:** bound patterns and honor step (#2205)
- **drain:** tokenize structured logs by field (#2204)
- **perf:** count all union arms in cardinality scans (#2199)
- **promql:** fix native-histogram quantile domain order and NaN-Sum rank base (#2178)
- **chsql:** alias anchor_ts AS TimeUnix for array-reduce subquery reducers (#2192)
- **rejection-parity:** re-file the exp-histogram scalar-binop divergence tracking issue (#2190)
- **e2e:** pin Metrics Drilldown's legacy /trail redirector as a seed (#2186)
- **promql:** compare each counter-reset pair at its own scale, not the window's merged one (#2177)
- **promql:** honour a subquery's @ pin in absent()[range:step] lowering (#2175)
- **e2e:** seed metrics crawl at its canonical route (#2174)
- **traceql:** implement the rest of the reference's static-validation pass (#2159)
- **promql:** omit absent grouping labels from histogram sum-by output (#2168)
- **e2e:** select Grafana metrics by keyboard (#2169)
- **api/loki:** close the WebSocket instead of refusing /tail's handshake at the cap (#2166)
- **cmd/cerberus:** wire MaxQuerySamples into the Loki head's engine (#2167)
- **promql:** probe mandatory sibling alternations (#2147)
- **optimizer:** collapse pushdown column enumerators, fix Having/Temporality drops (#2158)
- **e2e:** de-gate k3d crawl coverage (#2156)
- **promql:** preserve classic quantile interpolation order (#2155)
- **e2e:** swap gh run download for actions/download-artifact in crawl merges (#2157)
- **migration:** decode complete tier2 rule responses (#2152)
- **e2e:** preserve compose crawl slices (#2154)
- **ci:** enable LogQL oracle in chDB coverage (#2151)
- **promql:** retain first classic duplicate bound (#2145)
- **promql:** preserve missing le histogram semantics (#1986) (#2142)
- **crawl:** partition frontier ownership across shards (#2005) (#2136)
- **parity:** translate OTel metric names for Prometheus oracle (#2133)
- **promql:** honor exp histogram aggregation temporality (#2115) (#2134)
- **test:** enroll exponential histogram ULP parity fixtures (#2132)
- **tests:** derive exponential histogram seed counts (#2023) (#2130)
- **promql:** split native rate by aggregation temporality (#2114) (#2131)
- **traceql:** read rootness from an empty ParentSpanId, never an absent parent row (#2129)
- **test:** pin the cardinality fan-out's test names so a rename cannot no-op it (#2125)
- **test:** close the chDB session at exit so -race chdb suites stop segfaulting (#2118)
- **qlcommon:** narrow the shared-capture-name guard to real participation ambiguity (#2116)
- **traceql:** reject scalar-filter operand shapes reference Tempo rejects (#2119)
- **promql:** honor AggregationTemporality for irate and subquery counters (#2113)
- **chsql:** divide rate()'s extrapolation factor, not its product, over a float counter (#2111)
- **optcorpus:** record a byte-budget-rejected drain instead of a clean query (#2110)
- **traceql:** window the structural closure's anchor arm, not just its step (#2106)
- **chclient:** bound a PromQL sample drain on bytes, not just rows (#2107)
- **compat/loki:** diff every advertised detected field, not an 8-field prefix (#2103)
- **promql:** detect a native-histogram counter reset per whole histogram (#2094)
- **ci:** log in to the GHCR mirror first and survive a Docker Hub outage (#2102)
- **ci:** bound and shard the chDB round-trip legs so promql stops racing go test's default (#2100)
- **api/tempo:** honour the q narrowing filter on the tag-value routes (#2098)
- **test/spec:** isolate chDB fixtures via database, not full session teardown (#2097)
- **coverage:** count a package's coverage from every test that runs it (#2092)
- **test/e2e:** shard the every-published-metric sweep so no test carries the whole catalog (#2086)
- **engine:** declare the response shape on every route-B solver dispatch (#2084)
- **deploy:** register the cerberus OCI chart on Artifact Hub (#2079)
- **promql:** honour a subquery offset in the absent() subquery lowering (#2075)
- **chsql:** walk native-histogram buckets backward for phi >= 0.5, as reference does (#2073)
- **solver:** widen the native and absent grid walks by Offset+Range (#2070)
- **api/tempo:** charge compare()'s synthesised sample grid against the query budget (#2056)
- **api/loki:** give /tail its own admission budget so tails cannot starve queries (#2047)
- **logql:** give log streams their own scan shape instead of a padding column (#2045)
- **traceql:** type-check comparison operands instead of letting ClickHouse reject them (#2034)
- **promql:** make the native-histogram result path decode, end to end (#2037)
- **e2e:** derive the crawl canonical key from declared vocabularies, not option counts (#2020)
- **e2e:** resync both crawl inventories' doc field with stacks.ts (#2021)
- **e2e:** bootstrap the k3d crawl surface inventory (#2013)
- **e2e:** pin the primarySignal Database option's actual label text (#2012)
- **compat/promql:** seed a shifting-shape exp-histogram oracle for aggregated rate (#1958)
- **spec:** isolate each fixture's chDB session (#1987) (#2003)
- **e2e:** classify the anonymous primarySignal Select as enumerate (#2000)
- **test:** accept atan2's proven 1-ULP libm divergence in the parity oracle (#2001)
- **e2e:** union lean membership across fold candidates, not the fold winner (#1993)
- **traceql:** lower arithmetic between aggregates in a scalar filter (#1981)
- **prom:** stream matrixFromCursor per series instead of buffering every row (#1979)
- **schema:** emit TTL ... TO VOLUME so a multi-volume storagePolicy actually tiers (#1980)
- **preflight:** scope requirements + Prom metadata to enabled signals (#1978)
- **e2e:** derive the crawl canonical key from control identity, not data or order (#1977)
- **traceql:** resolve the true root span for spanset aggregates (#1975)
- **solver:** admit an anchor-compatible scalar interior instead of kind-based scalar-heavy (#1974)
- **logql:** seed both attribute maps so the selector-unchanged fixture can fail (#1973)
- **chdb:** stop a session's exception from corrupting the next query's Parquet decode (#1972)
- **promql:** honor AggregationTemporality for rate/increase over DELTA counters (#1964)
- **ci:** scan untracked-but-not-ignored files in forbid-skip and repo-hygiene (#1970)
- **qlcommon:** anchor label_replace with (?s:...) to match reference Prometheus (#1966)
- **ci:** scan a merge-queue batch's PR descriptions per pull request (#1965)
- **promql:** report the selected sample's timestamp from range-mode timestamp() (#1953)
- **rejection-parity:** admit reference-verbatim guard messages into the catalogue (#1962)
- **ci:** narrow forbid-deferral's bare "deferred" match to predicate grammar (#1960)
- **qlcommon:** re-cite the label_replace divergence and close the nullability coverage gap (#1957)
- **qlcommon:** answer label_replace over non-nullable shared capture names (#1952)
- **e2e:** recover the stranded crawl mount-order and Drilldown version-pin fixes (#1950)
- **compat/loki:** restore the loki-compliance-tester coverage floor with real behaviour tests (#1946)
- **boot:** gate the metric-table existence check on configuration, not the resolved name (#1947)
- **migration:** let the Tier-0 harness answer the drift lane instead of refusing on sight (#1945)
- **logql:** model nested json flattening and unpack extraction in the SQL-side lowering (#1931)
- decide from the source, not a proxy — timestamp() argument shape and the spans-scan pass-through (#1939)
- **promql:** guard label rewrites over a matrix range window, and pin the boundary in the compat corpus (#1914)
- **ci:** derive generated territory by destination and validate the merged bytes (#1934)
- **promql:** reduce native exp-histogram windows per series before merging across series (#1930)
- **promql:** peel ParenExpr/StepInvariantExpr before matrix-arg type checks (#1913)
- **telemetry:** surface rollback failures instead of discarding them (#1923)
- **e2e:** derive structural-param retention from a declared closed option set (#1919)
- **promql,metadata:** fan out unpinned selectors to histograms, fail fast on absent metric tables (#1915)
- **spansscan:** decide co-scope by what a paren opens, not by nesting depth (#1911)
- **test:** anchor the loki tail overflow fixture to the tail's own start (#1918)
- **promql:** key a computed histogram_quantile scalar on the scope it lands in (#1916)
- **promql:** bind computed scalar args per step and share their domain checks (#1886)
- **promql:** evaluate non-reducer subquery inners per anchor instead of rejecting them (#1895)
- **e2e:** re-marshal the compose surface inventory into codepoint order (#1908)
- **chsql:** make the vector-join uniqueness guard a HAVING predicate (#1734) (#1893)
- **promql:** derive topk columns and the duplicate-labelset guard from the plan (#1897)
- **ci:** pair every go test timeout with the CI job budget that runs it (#1847) (#1894)
- **prom:** fan exemplar lookup across every table the schema resolves (#1904)
- **e2e:** make full-depth crawl interaction driving a function of the app (#1891)
- **histogram_quantile:** clamp the native zero bucket and lower shaping aggs (#1879)
- **promql:** carry `__name__` through subqueries and reject duplicate labelsets (#1842)
- **test:** classify generated artefacts by their writer, not by their name (#1870)
- **e2e:** stop the nightly full-depth crawl being cancelled and parameterize the field segment (#1871)
- **promql:** move seven lowering guards onto upstream's accept/reject boundary (#1856)
- **chsql:** saturate native histogram_quantile to populated buckets (#1835)
- **e2e:** parameterize var-groupBy in the crawl's canonical surface key (#1855)
- propagate cleanup errors instead of swallowing them (#1851)
- **harness:** resolve the guarded git branch from the directory the command runs in (#1853)
- **tempo:** accept the full upstream tag-scope vocabulary (#1593) (#1843)
- **test:** make the coverage claims match what the tests exercise (#1828)
- **logql:** project detected_level as ordinary structured metadata (#1819)
- **chsql:** bind the top-N trace scope once instead of re-deriving it per site (#1821)
- **runtime:** report live breaker phases, breaker-derived Retry-After, re-probed caps (#1823)
- **e2e:** unblock the red release gates — unary-! showcase contract + OTel resource env (#1822)
- **promql:** carry `__name__` through the subquery spine for last/first_over_time (#1812)
- **test:** match generated-artefact markers against the whole path, not the basename (#1816)
- **e2e:** drive the Traces-Drilldown wire pin over the proxy, restore report upload (#1814)
- **traceql:** flip the trace-scoped showcase panels to their real, now-implemented answers (#1813)
- **test/property:** anchor the TraceQL oracle's =~ / !~ to match Tempo (#1808)
- **promql:** add exp-histogram companion arms to the regex `__name__` path (#1781)
- **compat:** run the rejection-parity reference flag-on and inside the fixture window (#1796)
- **rejection-parity:** reclassify stale lowerHoltWinters internal entries (#1771)
- **rejection-parity:** repoint the exp-histogram divergence at its own tracking issue (#1774)
- **promql:** bind nested time() per range-query step (#1762)
- **chsql:** fully anchor regex label matchers before lowering to match() (#1746)
- **promql:** route exp-histogram companion selectors, reject the rest (#1750)
- **promql:** classic-histogram bare-name, le restriction, no-range-wrapper quantile (#1742)
- **chclient:** thread ResponseShape through CursorQuerier as defense-in-depth (#1754)
- **traceql:** compute trace-scoped root-identity filters and fix NOT-evaluation parity (#1740)
- **tempo:** count exemplar-attach failures on the metrics path (#1749)
- **tempo:** record cerberus_queries_* for gRPC StreamingQuerier RPCs (#1747)
- **chsql:** carve out NaN-both-sides pairs from changes() (#1737)
- **loki:** bucket /patterns mining per detected level (#1744)
- **prom:** return upstream's recursive AST shape from /api/v1/parse_query (#1745)
- **promql:** thread scalar args for predict_linear/holt_winters/quantile_over_time over a subquery (#1739)
- **promql:** collapse info() series sharing one identity signature (#1735)
- **chplan:** commensurate epoch-aligned subquery inner grids across shards (#1736)
- **traceql:** stop dotted attributes colliding with intrinsic names (#1731)
- **chplan:** honor RangeWindow.Offset when widening a spine's input grid (#1733)
- **api/prom:** parse metadata match[] with ParseMetricSelector (#1730)
- **bench:** reconcile docs/benchmarks.md set-op wall-vs-cardinality claims (#1728)
- **chsql:** resolve late-materialisation shape via request schema, not default table name (#1723)
- **optimizer:** restate scan-resource-bound witness set to include literal InList (#1718)
- **optimizer:** narrow Scan under fanout/LWR/metrics-agg/histogram-quantile (#1715)
- **perf:** make fan_factor honest — null instead of a fabricated 1.00 (#1713)
- **ci:** lower query-parallelism for the floor-pinned compat lane (#1699)
- **tempo:** stamp non-nil SearchMetrics on every gRPC streaming frame (#1697)
- **logql:** wire queryShouldSurfaceDetectedLevel to the drop-stage label set (#1682)
- **compat/loki:** seed a generic structured-metadata key and diff it (#1498) (#1686)
- **e2e:** use TimeUnix, not Timestamp, in the metrics stale-row DELETEs (#1677)
- **ci:** verify GitHub server-side merge ignores -merge, add structural guard (#1675)
- **e2e:** use heavyweight ALTER TABLE DELETE for stale-row cleanup (#1671)
- **spec:** reconstruct production wrap-projection for TraceQL round-trip (#1670)
- **e2e/playwright:** re-verify and repair drilldown-app click-drill selectors (#1667)
- **e2e:** bound rolling re-seed duplication for every fixture family (#1666)
- **e2e:** wire a real $job template variable into showcase-promql (#1663)
- **e2e/loki:** anchor loki_tail window to ClickHouse's own clock (#1660)
- **e2e:** assert drilldown-app drill depth instead of only annotating it (#1661)
- **ci:** skip empty placeholder commits in commitlint's PR range (#1658)
- **spec:** resolve bare-table SELECT * against the seed's own CREATE TABLE DDL (#1657)
- **codeql:** close remaining static-analysis findings (#1656)
- **compat:** add cerberus-owned corpus covering LogQL `| unpack` (#1648)
- **strict-scan:** reconstruct production wrap-projection for TraceQL search fixtures (#1654)
- **ci:** gate every AGPL parser import behind the agpl_oracle build tag (#1651)
- **ci:** route build-with-registry-retry's argv through bash positionally (#1645)
- **security:** sanitize request-derived values before logging (CodeQL go/log-injection) (#1644)
- **ci:** reject flag-shaped values in env-var script overrides (CodeQL #66) (#1646)
- **ci:** create the promql-surface-gate reference config via mkdtempSync (#1639)
- **ci:** anchor the goreleaser deprecation URL regex without breaking real anchors (#1638)
- add missing ^ anchors to exclude_files regexes in mutation-phases.mjs (#1637)
- use null instead of empty string for default ClickHouse password (#1636)
- **solver:** prove route B against a real ClickHouse, not just chDB or a fake emitter (#1641)
- **chaos:** pin the not-applicable rate so precondition drift is visible (#1640)
- **promql:** reduce classic-histogram agg windows per series before folding across series (#1633)
- **qlcommon:** expand label_replace backrefs above ClickHouse's \9 ceiling (#1632)
- **promql:** info() ignore-set pass-through and conflicting-label abort (#1631)
- **promql:** fold quantile by(le) over classic histogram buckets (#1627)
- **ci:** unbreak the compatibility gate by making the scan set one list (#1625)
- **logql:** narrow the range-aggregation grouping key by drop/keep (#1609)
- **loki:** stamp the parser-error labels Loki sets on a failed unpack (#1612)
- **promql:** keep `__name__` per series for regex-matched last/first_over_time (#1603)
- **qlcommon:** expand label_replace backrefs the way Go's ExpandString does (#1601)
- **ci:** resolve build base images through the mirror and pin the e2e pre-pull (#1605)
- **promql:** return the first bucket's bound when it is non-positive (#1604)
- **tempo:** populate span name when the query references the name intrinsic (#1599)
- **ci:** retry the mirror login and read every copy back out of GHCR (#1586)
- **promql:** merge classic bucket layouts in the cumulative per-le domain (#1592)
- **traceql:** match Tempo spanset && semantics (#1422)
- **traceql:** lower a spanset-aggregate operand to spans, not one row per trace (#1413)
- **chclient:** stall the teardown destroy arm's probe so the cancel cannot race the drain (#1555)
- **tempo:** reject scope=all on the tag-discovery endpoints (#1594)
- **tempo:** unify entrypoint error classification and search-metrics accounting (#1581)
- **traceql:** prune non-root compare root enrichment (#1423)
- **promql:** honour info() drop semantics and non-equality `__name__` matchers (#1589)
- **api:** report a cancelled query as canceled, not a server fault (#1585)
- **ci:** tell a missing issues:read permission from a nonexistent issue (#1582)
- **logql:** apply _extracted collision policy to json and regexp parser stages (#1578)
- **promql:** evaluate subquery inners on the epoch-aligned anchor grid (#1557)
- **promql:** key classic histogram bucket aggregation by bucket layout (#1571)
- **promql:** project resource labels for histograms (#1424)
- **solver:** gate RangeLWR slice commensurability (#1419)
- **chclient:** cancel in-flight breaker recovery ping on Close (#1425)
- **perf:** re-derive the cardinality baseline and give its ratchet teeth (#1446)
- **promql:** shift synthetic subquery offset grid (#1418)
- **traceql:** align select union arms (#1417)
- **engine:** stamp routed drain outcomes (#1420)
- **test:** fail on seeded-but-inert TXTAR fixtures instead of skipping them (#1411)
- **prom:** fan out histograms for regex metric names (#1415)
- **traceql:** align spanset and semantics with Tempo (#1416)
- **justfile:** record the cardinality baseline after the golden rewrite (#1414)
- **optcorpus:** publish partial route-B fan-outs instead of dropping them (#1410)
- **optcorpus:** fold routed cost at the Executor's effective shard concurrency (#1408)
- **solver:** extract features from every GridCarrier kind, not just RangeWindow (#1403)
- **engine:** record routed (route-B) dispatches in the router corpus (#1402)
- **engine:** size the spill threshold at half the cap, not 512 MiB (#1400)

### Performance

- **engine:** disable ClickHouse's new analyzer for native-histogram merge queries (#2358)
- **promql:** bind count_values' native-histogram float formatter instead of re-deriving it (#2353)
- **promql:** materialize rate/increase's extrapolation factor once per series row (#2351)
- **ci:** scope PR mutation testing to changed lines (#2253)
- **api/prom:** prune histogram arms for pinned gauges (#2206)
- **logql:** fuse variants sharing a value expression (#2210)
- **spec:** reuse round-trip work in parity (#2207)
- **ci:** size each cardinality-baseline leg's GOMAXPROCS to its share (#2188)
- **traceql:** fold phase-B trace-id restriction into the inverse closure anchor (#2164)
- **solver:** classify the decision ratchet under both lowering tables (#2160)
- **test:** shard the cardinality baseline write so its regeneration fans out (#2123)
- **chclient:** decline the columnar decode before dispatch on a declared non-matrix shape (#2105)
- **api/prom:** retire the redundant metadata-side metric-name fan-out (#2058)
- **traceql:** evaluate each spanset-intersect fallback arm once, not twice (#2057)
- **promql:** extend the fused subquery emitter to range queries (#2054)
- **traceql:** read the spans table once for a bare-selector spanset intersect (#2052)
- **golden:** fan the promql round-trip walk out across processes (#2041)
- **golden:** run same-stage update-golden shards concurrently (#2036)
- **golden:** shard `just update-golden` behind a derived coverage check (#1900)
- **chsql:** bind the compare root-lookup trace_id_ts envelope once (#1922)
- **ci:** shard the gremlins phase2 leg three ways

### Changed

- **promql:** dedup avg/scalar-binop histogram scaling, drop dead cols (#2314)
- **traceql:** reuse ast.NegateStatic instead of duplicating it in lower.go (#2298)
- **migration-lane:** dedupe HTTP getOK and errWriter helpers (#2307)
- **ci-scripts:** dedupe the buffered leg runner into spawn-tagged.mjs (#2306)
- **logql:** drop redundant private twin of NormalizeDottedLabels (#2295)
- **api:** dedupe writeEngineHeaders into shared httperr package (#2289)
- **promql:** dedup count_values group-key switch via subqueryAggregateGroupBy (#2287)
- **traceql:** dedup lowerAggregate's envelope AggFuncs via spansetEnvelopeAggFuncs (#2283)
- **chplan:** seal function and node vocabularies (#2220)
- **chplan:** seal traceql and shared function names (#2203)
- **chplan:** seal api and chsql function symbols (#2202)
- **logql:** replace raw function names with sealed symbols (#2201)
- **promql:** convert FuncCall/AggFunc construction sites to Fn (#2193)
- collapse mirrored production dispatch onto one classifier, and gate it (#2126)
- **test:** hoist duplicated instant-query wire helper into test/spec/wire (#1991)
- **chsql:** give the compare root leg a renderable boundary (#1976)
- **test:** delegate the chdb-go EOF sentinel to testsql.TolerantRowsErr (#1909)
- **test:** give the chDB SQL shim one owner and unlink test/spec from the heads (#1906)
- derive the facts three declarations were restating by hand (#1882)
- extract duplicated helpers into shared utilities (#1850)
- **perf:** shard the cardinality and solver-decision baselines one file per record (#1829)
- **rejection-parity:** shard the catalogue one file per lowering source (#1804)
- **promql:** migrate rewriteMetricName onto WireArms classification (#1775)
- **promql:** consolidate wire-arm classification into a WireArms resolver (#1763)
- **qlcommon:** lift instant-lookback constant to one owner (#1760)
- **chplan:** hoist trace_id_ts envelope builder into a shared helper (#1751)
- **chsql:** replace preflight double-render with sticky Builder.err (#1748)
- **loki:** decode log-stream rows into a named chclient.LogRow (#1426)

### CI

- extract registry-login and free-disk-space into composite actions (#2315)
- **link-check:** exclude slsa.dev from the external link checker (#2274)
- implement the two-tier merge/release test fence (#2230) (#2260)
- coalesce replaceable deep main tests (#2247)
- require the published quickstart canary (#2246)
- **goldens:** add manual shard update workflow (#2239)
- add test-fence enrollment guards (#2234)
- add shadow test-fence registry contract (#2233)
- **codeql:** exclude non-production Go binaries from extraction (#2109)
- **crawl:** ratchet the Grafana surface inventories to their canonical form (#2019)
- **e2e:** add a dispatch-driven regen path for the compose crawl inventory (#2016)
- **perf-guards:** shard the cardinality ratchet across 8 runner processes (#2011)
- **forbid-deferral:** stop reading a description the change's author did not write (#1940)
- **release-gate-drift:** assert live branch protection equals the in-tree pin (#1831)
- **spec:** round-trip the OPTIMIZED plan, not just the pre-optimizer one (#1710)
- **compatibility:** add a floor-pinned prometheus lane at ClickHouse 24.8 (#1691)
- **strict-scan:** wire TestHistogram_RealExporterSchema_Integration (#1678)
- **security:** pin GitHub Actions to commit SHAs (#1655)
- **compat:** finish compat-QL rename for the prometheus head (#1437) (#1624)
- add forbid-sql-raw gate for raw token writes in internal/chsql (#1441) (#1623)
- **e2e:** retire stale otel-collector-gateway BackOff flake caveat (#1621)
- migrate CodeQL to advanced setup, add merge_group trigger (#1558) (#1622)
- **forbid-skip:** remove 'not implemented' prose scan (#1538) (#1619)
- add agpl_oracle lane so tagged oracle tests actually run (#1610) (#1620)
- **lefthook:** mirror repo-hygiene gate into pre-push (#1618)
- **lint:** analyse every build configuration, not just the default one (#1613)
- close four gate-hygiene holes — required-set drift, a report that cannot fail (#1608)
- mirror every upstream CI image to GHCR and pull from there first (#1579)
- post every required context on the merge queue's projected trunk (#1559)
- put every image acquisition on the authenticated pull path (#1572)
- **git:** refuse to line-merge generated baselines (#1567)
- **registry:** a rate limit is not a retryable fault (#1563)
- **labels:** deterministic issue auto-labeler + backfill for the 62 unlabeled issues (#1553)
- **forbid-deferral:** own workflow, section scope, three-branch remedy (#1560)
- **forbid-deferral:** enforce the no-deferrals rule as a required status check (#1550)
- retry image builds on transient Docker Hub registry faults (#1534)

### Documentation

- **promql:** repoint #2296's citations now that #2245 closed the gap (#2320)
- **chplan:** repoint HistogramRowShape's stale #1967 citation to #2296 (#2299)
- correct merge-gate roster and coverage sharding drift (#2310)
- **performance,health:** correct perf-guards/profile gate tier and breaker file ref (#2308)
- **operations:** fix stale de-gated-lanes count after CodeQL row removal (#2305)
- **api/reqctx:** stop claiming Tempo shares the timeout backstop (#2303)
- **forbid-skip:** fix stale row numbering for should_skip/escape-hatch (#2291)
- **compatibility:** correct compat/<head> checks to release-gate, not PR-required (#2290)
- **optimizer:** correct false mirroring claim in range-window transpose (#2284)
- **chplan:** repoint stale combinator-split citation to #2280 (#2282)
- **ci:** fix stale mutation-context comment in mutation.yml (#2286)
- **promql:** drop the hqMerge alias' blanket compensated-fold claim (#2281)
- **compat:** correct the prometheus-floor timing claim with re-measured numbers (#2268)
- 1.15.0 pre-release audit (#2250)
- align engine and PREWHERE documentation (#2161)
- **readme:** add the Artifact Hub badge (#2083)
- correct the stale no-compose-isolation claim (#2046)
- **chsql:** pin why resets() staying float-only is correct, not partial (#2004)
- **engine:** record why a subquery is bounded by rejection, not by streaming reduction (#1936)
- **ci:** stop e2e.yml claiming its lanes are branch-protection gates (#1892)
- **process:** require local reproduction of a red check before the next push (#1860)
- **security:** document the authn / tenancy boundary operators must provide (#1852)
- **schema:** fix stale TablesForUnknownName comment on histogram UnionAll (#1764)
- correct six comments that contradict the code they describe (#1600)
- out-of-scope work becomes an issue, never PR deferral prose (#1427)
- require a PR for every pushed branch; promote strict-scan + CodeQL (#1409)

## [v1.14.0] — 2026-07-30

A minor rather than a patch because of the PromQL label-shaping hoist (#1388): OTel attribute shaping now happens after the native rate grid instead of before it, which changes the emitted SQL shape for every `rate()`-family query over shaped labels. Answers are unchanged — the rewrite is only sound for `-State`/`-Merge` aggregates, and the naive form of it returns wrong results — but the plan a query produces is not what v1.13.x produced, so it does not belong in a patch release.

### Fixed

- **ci:** pre-release audit fixes for v1.14.0 (#1391)
- **compose:** give every stack a per-checkout project name (#1390)
- **routerrules:** stop geometry gates firing on every unclassified failure (#1389)
- **solver:** distinguish routing-disabled from below-threshold (#1385)
- **solver:** record the real cost grid on every routing refusal (#1384)
- **brew:** migrate existing formula installs to the cask (#1380)

### Performance

- **promql:** defer OTel label shaping past the native rate grid (#1388)

### CI

- **mutation:** run the legs a PR's scope changed instead of skipping all (#1378)

### Documentation

- cask-only brew instructions and a README humans can skim (#1386)
- **migration:** install the cask, not the formula (#1383)

## [v1.13.2] — 2026-07-29

### Fixed

- **tempo:** stop zero-fill buckets turning quantiles into NaN (#1374)
- **ci:** analyse cold in the lint lane so a cache cannot decide the gate (#1363)
- **ci:** publish compat-scores from a scratch worktree, not the checkout (#1361)
- **chsql:** name the canonical key-order function once, not twice (#1365)
- **chsql:** canonicalise raw attribute-Map series keys at the emit chokepoint (#1362)
- **ci:** run the Tier-0 explain goldens on pull requests (#1364)
- **loki:** canonicalise Map key order on metadata series-identity keys (#1360)
- **logql:** canonicalise Map key order on stream-identity keys (#1358)
- **chsql:** dedupe TraceQL structural unions on span identity (#1357)
- **promql:** canonicalise Map key order on the non-join and histogram paths (#1356)
- **audit:** unwedge the routed fan-out, harden the enum guard and shutdown ordering (#1353)
- **release:** smoke the documented brew command on Linux too (#1352)
- **config:** one duration grammar for every CERBERUS_* knob (#1348)
- **chsql:** canonicalise Map-valued vector-match keys with mapSort (#1346)
- **chclient:** release pooled ClickHouse connections on cursor teardown (#1347)
- **ci:** retry the buildkit bootstrap pull behind one buildx setup action (#1345)
- **chsql:** bind a set-op arm's timestamp to the arm's own outer scope (#1344)
- **optcorpus:** ingest query_log exception exits and reconcile the exit_status enum (#1341)
- **cli:** lead the root help with an ASCII banner and a plain-English blurb (#1343)
- **promql:** evaluate vector set operators per evaluation timestamp (#1340)
- **release:** only the newest line takes Homebrew and the Latest pointer (#1335)

### CI

- **release:** open the release PR against the dispatched line (#1372)
- **compat:** gate the parity ratchet on per-case identity, not a count (#1350)
- **compat:** extract the compat-scores badge publisher to a module (#1342)
- detect release-gate rot before it breaks a publish (#1334)

### Documentation

- **config:** say what `0` does on every duration knob that accepts it (#1355)
- **operations:** audit the delta before the backport, not after (#1354)
- **operations:** document the canonical ordered release ritual (#1349)
- **migration:** replace the configuration dump with a step 0 (#1339)

## [v1.13.1] — 2026-07-28

### Changed

- **release:** container images publish as a single multi-arch index (#1320). The per-arch `:<version>-amd64` / `-arm64` tags are gone — they only ever existed as inputs to the manifest, and nothing in the repo, chart, or docs pulled one. buildx now also attaches an SBOM attestation to the index.

### Fixed

- **promql:** keep the `@` pin in range mode on every range-vector lowering (#1325)
- **prom:** bound windowless metadata discovery by real retention (#1327)
- **ci:** make the release gate, image probe and doc claims hold under audit (#1329)
- **migration:** forward the story dispatch input to the tier-2 run (#1328)
- **optimizer:** keep AggFunc.Params through a constant fold (#1326)
- **release:** close the three gaps that let a release ship unverified (#1322)
- **ci:** skip buildable images in the compose pre-pull (#1321)
- **ci:** retry every image pull instead of failing the lane (#1319)
- **release:** name the workflow a preflight wait is blocked on (#1318)

### Performance

- **chsql:** skip the spans-scan emit guard when the SQL never names the spans table (#1330)

### Documentation

- correct parity-gate, config-source and forbid-skip claims (#1323)

## [v1.13.0] — 2026-07-27

### Added

- **config:** accept the chart's nested shape in cerberus.yaml (#1316) — `clickhouse.addr`, `query.maxSamples`, `migrate.verify.ref` and the rest, so one file mirrors the chart's `values.yaml` instead of restating the `CERBERUS_*` table in YAML syntax. A nested file is exactly equivalent to exporting the corresponding variables, precedence unchanged (flag > env > file > default), and the flat `CERBERUS_*` key still works as the long-tail escape hatch. `docs/configuration.md` lists the config-file path beside every variable.
- **migrate:** read verify/inventory settings from cerberus.yaml (#1314)

### Changed

- **config:** a `cerberus.yaml` that exists but does not parse, or that carries a key cerberus does not recognise, is now a startup error naming the nearest key that does (#1316). Previously such a file was tolerated and its unrecognised settings silently ignored. **Upgrading:** a file with a typo, or one pointed at chart-only keys, will now stop the process at startup instead of running with defaults.

### Fixed

- **release:** make the Homebrew publish path actually work (Formula/ dir + a runner with brew) (#1306)
- **test/e2e:** scope the tempo duration-filter search to the seed corpus (#1305)

### Documentation

- **migration:** cut the configuration section down to what needs configuring (#1313)
- **migration:** restructure the guide as numbered steps with pre/post-conditions (#1312)
- **migration:** add the configuration step the guide jumped over (#1311)
- **migration:** rewrite as a readable guide, split the contract into a reference (#1310)
- document the Homebrew installer as the migration entry point (#1307)

## [v1.12.0] — 2026-07-27

### Added

- **release:** gate publish on migration-e2e and smoke the released artifact (#1296)
- **migration:** make Layer-14 coverage mean executed, and pin the PASS assertions (#1286)
- **migration:** stand up Tier-2 ruler substrate (Layer 14, phase 3a) (#1284)
- **migration:** drive cerberus migrate verify through Gherkin against the live tier-1 stack (#1283)
- **migrate:** extend inventory + rulegraph to Loki, fold into gate (#1276)
- **solver:** failure-driven route memo (`internal/routememo`) — when a route-A dispatch fails on ClickHouse resource exhaustion, retry it once on the sharded route and remember the outcome against a literal-free cost-shape fingerprint, so future cost-equivalent traffic routes directly instead of paying the same failure again; bounded by two-failure corroboration, a cluster-wide pressure damper, a single process-wide dispatch-token budget, and TTL-with-midpoint-revalidation. Off by default (`CERBERUS_SOLVER_ROUTE_MEMO_ENABLED`) (#1275)
- **migrate:** judge log-stream, trace-search and trace-by-id parity in verify (#1269)
- **telemetry:** classify query failures, extend latency tails, attribute stages by language (#1274)
- **migration:** pinned reference stack + deterministic all-signal seeder (#1267)
- **migrate:** verify the Loki + Tempo metric lanes for migration parity (#1266)
- **chopt:** native deriv/predict_linear via timeSeries*ToGrid (25.9) (#1259)
- **migrate:** explain + classify TraceQL queries (#1258)
- **migrate:** explain + classify LogQL corpus queries (#1257)
- **migrate:** harvest all three heads (LogQL + TraceQL) into one corpus (#1256)
- **migrate:** verify — failure verdict, --report json, bug-report repro, experimental-CH attribution (#1244)
- **migrate:** gate — fold migration artifacts into a go/no-go decision (#1243)
- **migrate:** rulegraph — recording-rule output → consumer dependency graph (#1242)
- **migrate:** classify — bucket each corpus query as PromQL-pure / rewritable / no-equivalent (#1241)
- **migrate:** inventory — cardinality + churn from the source Prometheus (#1240)
- **migrate:** verify — replay the corpus against Prometheus and cerberus and diff (parity gate) (#1239)
- **migrate:** harvest — build a query corpus from rule files + Grafana dashboards (#1238)
- **migrate:** explain — preview the ClickHouse SQL for your PromQL (#1236)
- **migrate:** offline schema preview (ddl.RenderAll + cmd/migrate --schema) (#1231)
- **engine:** add DryRunSQL to emit query SQL without executing (#1233)

### Fixed

- **ci:** retry the lychee download so link-check can't die before checking a link (#1303)
- **release:** eol-retire needs RELEASE_PAT — release/*.x is ruleset-protected (#1299)
- **solver:** count chDB shard opens atomically so the cap contract can't lose increments (#1297)
- **mutation:** cap per-mutant test timeout so runaway mutants can't OOM the runner (#1294)
- **ci:** let tier-2 write the run report its scenarios are attested from (#1293)
- **ci:** run the tier a job was given, not the closure of what it needs (#1292)
- **ci:** make migration-tier0 a plain job and teach every seed the ServiceName column (#1291)
- **promql:** project service_name on every selector + unstick the migration-e2e lane (#1289)
- **migration:** make the Tier-1 scenarios assert their stories (#1288)
- **migration:** make MIG-18 and MIG-19 pass against the live Tier-2 ruler (#1287)
- **migration:** keep tier-1 archetypes apart by identity, not by window (#1285)
- **ci:** log in to Docker Hub before k3d pulls in dashboard/chaos/bwc-minio (#1282)
- **mutation:** scope internal/logql/lsyntax out of the four logql legs (#1280)
- **ci:** pass migration-e2e's enumerator output to migration-tier0 as an artifact (#1278)
- **promql:** resolve label catalog answers at the scan, not above a per-series rebuild (#1270)
- **solver:** read the eval grid through a carrier interface, not a kind list (#1272)
- **prom:** resolve unpinned `__name__` matchers against the synthetic name set (#1271)
- **crawl:** reconcile the init-race 400 on both transports + its console twin (#1264)
- **e2e:** move the k3d ClickHouse substrate off the defective 26.5 line (#1263)
- **perf:** delegate traceql_compare scaling to the cardinality axis (#1261)
- **chsql:** whole-second axis for native deriv/predict_linear; bump CI CH substrate to 26.5 (#1260)
- **cli:** use cerberus migrate form in verify repro, classify hint, and migration docs (#1253)
- **migrate:** final audit batch — gate soundness, creds leak, preview fidelity, polish (#1249)
- **migrate:** gate + rulegraph honesty, CLI UX, inventory tests (#1247)
- **migrate:** verify — fix false-PASS honesty defects + correctness/security hardening (#1245)

### Changed

- **solver:** retire the route-threshold autotune loop for static config (#1273)
- **cli:** consolidate all CLIs into one cobra-based cerberus binary (#1250)

### Documentation

- **migration-testing:** pin Gherkin/godog as the Layer-14 scenario language (#1265)
- **cli:** finish CLI-consolidation sweep in migration-testing + router-rules (#1254)
- **migration-testing:** pin the verify-tier comparator as internal/migrateverify (#1251)
- **migration:** reconcile the migration playbook to the shipped CLI + promote in README (#1248)
- **migration:** scheduled end-to-end scenario plan for all migration user-stories (#1246)
- **migration:** add pre-cutover migration playbook (#1235)
- **readme:** center architecture diagram as a convergent hub (#1228)
- **readme:** redraw architecture diagram as a hub around ClickHouse (#1227)

## [v1.11.1] — 2026-07-17

### Fixed

- **chsql:** window-prune the compare root-lookup scan for root-scoped selections (#1223)

## [v1.11.0] — 2026-07-16

### Added

- **solver:** register VectorJoin as slice-invariant so step-aligned ratio joins route through the sharded path; fail-close instant joins on route A (#1215)

### Fixed

- **prom:** don't re-shift native rate offset output (double-shift); sync docs/comments (#1217)
- **promql:** report range-mode offset output on the unshifted grid, not t-offset (#1216)
- **chsql:** push compare() root-lookup bound below GROUP BY, not above it (#1214)
- **e2e:** reconcile the Traces-Drilldown undefined-groupBy init race (#1209)

### Changed

- **chsql:** dedupe SELECT-passthrough loops and offset-unshift frag (#1219)
- **solver:** drop dead ScanFrom/spineReach; share offset-reanchor IR helper (#1218)

### CI

- auto-tidy test/oracle on dependabot go.mod bumps (#1212)

### Documentation

- remove goreportcard badge from README (#1210)

## [v1.10.0] — 2026-07-04

### Added

- **tempo:** extend the structural two-phase split to the gRPC Search path (#1201)
- **tempo:** admit union `&>>`/`&<<` into the structural two-phase seam (#1198)
- **tempo:** route `!>>` through two-phase + default-on structural-two-phase toggle (#1197)
- **tempo:** route `>> | select(...)` through the structural two-phase seam (#1195)
- **autotune:** expose loop state via /info/autotune + per-pod metrics (#1193)
- **solver:** self-driving autotune loop lowering thresholds toward observed OOM line (#1190)

### Fixed

- **tempo:** bound the wide-projection drain with a fail-closed byte budget (#1200)
- **e2e:** warm the stack before compose-smoke's zero-tolerance sweep (#1196)

### Documentation

- **tempo:** record the deliberate no-density-gate decision + pin phase-A narrowness (#1199)
- **autotune:** correct stale certify/off-policy language and OOM-population wording (#1191)

## [v1.9.1] — 2026-07-03

### Fixed

- **schema:** widen proj_metric_metadata with IsMonotonic to keep routing (#1186)
- **chsql:** pass timeSeries*ToGrid family whole-second DateTime, not DateTime64 (#1185)
- **chart:** chMaxMemory reaches split heads + int64 all numeric emitters + guard split ingress (#1182)

## [chart 0.10.1] — 2026-07-02

### Fixed

- **chart:** chMaxMemory reaches split heads + int64 all numeric emitters + guard split ingress (#1182)

## [v1.9.0] — 2026-07-02

### Added

- **traceql:** two-phase structural search to bound the wide projection (#1166)

### Fixed

- **prom:** exp-histogram support on the real OTel-CH exporter schema (#1171)
- **traces:** window-bound every otel_traces scan + memory-bound compare and structural (#1163)

### Changed

- split phase4-traceql into three memory-balanced matrix entries

### CI

- **mutation:** rebalance traceql split into four RE2-safe package-scoped legs (#1178)
- **mutation:** run phase4-traceql at workers=1 to fit the runner budget (#1177)

### Documentation

- rewrite README to be human-first (plain intro, lighter 1.0 disclaimer, progressive depth) (#1162)
- **operations:** recommend ClickHouse 26.6+ for parallel recursive CTEs on structural-join (#1160)

## [v1.8.4] — 2026-06-29

### Fixed

- **traceql:** enforce resource-bound invariant on every spans scan (traces drilldown OOM) (#1154)

## [v1.8.3] — 2026-06-29

### Added

- **engine:** nested-subquery anchor product (GAP-C) + plan-node bound registry (#1120)
- **engine:** reject subqueries whose anchor grid busts the sample budget (GAP-2) (#1115)
- **chart:** bwc mode — bundled production ClickHouse on object storage (S3/GCS/Azure), no operator (#1106)

### Fixed

- **license:** de-AGPL audit cleanup (#1136)
- **logql:** clean-room Apache LogQL parser (remove AGPL pkg/logql/syntax) (#1130)
- **traceql:** clean-room Apache TraceQL parser (remove AGPL pkg/traceql) (#1131)
- **traceql:** numeric-coerce attribute-vs-attribute ordering comparisons (#1127)
- **chclient:** enforce the drain budget — 0 no longer disables it + cap line-peek bytes (#1123)
- **tempo:** push /api/search limit into spanset-aggregation as server-side ORDER BY+LIMIT (#1122)
- **traceql:** window spanset-aggregation searches (GAP-3) via generic leaf recurse (#1121)
- **chclient:** bound metadata drains with the per-query sample budget (GAP-A) (#1119)
- **api/loki:** clamp metadata-peek line_limit to bound the result drain (#1111)

### Performance

- **chsql:** memory-bounded fused emit for instant reducer-over-subquery (GAP-2) (#1116)
- **promql:** bound instant-subquery inner scan to the eval window (GAP-2 axis-1) (#1114)
- **traceql:** fold the search window into every compound search shape (GAP-3) (#1113)
- **engine:** spill GROUP BY + sort to disk on every query, not just compare() (#1112)
- **traceql:** bound structure-tab row source to the search trace limit (#1109) (#1110)

### Changed

- **chsql:** single-source the extrapolation arithmetic (audit M1) (#1118)

### CI

- **chdb:** disable async preemption to kill the purego SIGABRT flake (#1133)

## [chart-v0.9.0] — 2026-06-27

Chart-only release (appVersion stays 1.8.2 — no binary/image change).

### Added

- **chart:** bwc mode — bundled production ClickHouse on object storage (S3/GCS/Azure), no operator (#1106)

## [v1.8.2] — 2026-06-27

### Fixed

- **prom:** bound metadata enumeration to retention + cover always-unbounded metricMetaSQL (#1104)

### Performance

- **prom:** per-series projection (proj_series) retires the metrics enumeration tier + curated projection registry (#1107)
- **prom:** serve windowless metric-name enumeration from an aggregating projection (#1105)

## [v1.8.1] — 2026-06-26

### Added

- **optimizer:** fail-closed scan-time-bound invariant for instant windowed scans (#1100)

### Fixed

- **release:** preflight ignores empty check-suites (e.g. GitGuardian) in the CI wait (#1097)

### Performance

- **chsql:** bound instant windowed-array scans to the eval window (#1098)

### Documentation

- swap CI/mutation badges for Ask DeepWiki in README (#1099)

## [v1.8.0] — 2026-06-26

### Added

- **chopt:** auto-enable native time-series aggregates on capable servers (#1091)

### Fixed

- **chsql:** dedup duplicate-timestamp samples in row-path rate/increase/delta (#1092)
- **chopt:** ts_grid capability probe falsely disabled native aggregates on healthy servers (#1094)

## [v1.7.1] — 2026-06-26

### Fixed

- **tempo:** bound windowless tag/attribute discovery to recent window (#1089)
- **prom:** push request window into no-match metadata discovery scans (#1088)

## [v1.7.0] — 2026-06-25

### Added

- **api:** add /info metadata + optimization fingerprint endpoint (#1082)
- **schema:** configurable storage_policy + MergeTree settings on auto-created tables (#1081)

### Fixed

- **chclient:** re-key query_id on columnar row-path fallback to avoid CH 216 (#1086)
- **release:** maintenance preflight waits for CI to finish instead of snapshotting (#1083)
- **traceql:** push time bound into compare scan + root leg, not above the join (#1080)
- **chopt:** probe CH version over the default connection so a missing otel DB no longer pins 24.8 (#1079)

## [v1.6.1] — 2026-06-25

### Added

- **chopt:** composable auto token in CERBERUS_CH_OPTIMIZATIONS (#1076)
- **optcorpus:** capture cerberus-side rejections in the router corpus (#1065)
- **routerrules:** detection-fidelity benchmark + degradation sweep (#1063)
- **routerrules:** concrete 5-rule ruleset + harness + chDB-parity CI fix (#1062)
- **routerrules:** generic router-rules catalog + offline analysis engine (#1060)

### Fixed

- **release:** maintenance preflight excludes de-gated informational lanes (#1077)
- **routerrules:** gate route_a_memory_near_cap on the configured cap, not the corpus p95 (#1066)
- **e2e:** reconcile Traces-Drilldown init-race 400 when both bodies are lost (#1070)
- **routerrules:** wrap integer CH columns to match Go scan types (strict clickhouse-go) (#1064)
- **traceql:** bound compare() query memory to avoid 2GiB ClickHouse OOM (#1059)

### CI

- **lint:** exclude .claude worktrees from golangci-lint scan (#1071)
- **release:** auto-retire the out-of-window release line on a new minor (active EOL) (#1072)

### Documentation

- theme-aware README hero via <picture> (#1074)
- **readme:** branded hero banner + regrouped badges (#1073)
- forbid "pre-existing" as an escape hatch for leaving bugs unfixed (#1069)
- **release:** define the maintenance support-window / EOL policy (latest 3 minor lines) (#1068)
- **coverage:** reframe PromQL start()/end() rejection as permanent parity (#1061)

## [v1.6.0] — 2026-06-25

### Added

- **optcorpus:** capture cerberus-side rejections in the router corpus (#1065)
- **routerrules:** detection-fidelity benchmark + degradation sweep (#1063)
- **routerrules:** concrete 5-rule ruleset + harness + chDB-parity CI fix (#1062)
- **routerrules:** generic router-rules catalog + offline analysis engine (#1060)

### Fixed

- **routerrules:** gate route_a_memory_near_cap on the configured cap, not the corpus p95 (#1066)
- **e2e:** reconcile Traces-Drilldown init-race 400 when both bodies are lost (#1070)
- **routerrules:** wrap integer CH columns to match Go scan types (strict clickhouse-go) (#1064)
- **traceql:** bound compare() query memory to avoid 2GiB ClickHouse OOM (#1059)

### CI

- **lint:** exclude .claude worktrees from golangci-lint scan (#1071)
- **release:** auto-retire the out-of-window release line on a new minor (active EOL) (#1072)

### Documentation

- theme-aware README hero via <picture> (#1074)
- **readme:** branded hero banner + regrouped badges (#1073)
- forbid "pre-existing" as an escape hatch for leaving bugs unfixed (#1069)
- **release:** define the maintenance support-window / EOL policy (latest 3 minor lines) (#1068)
- **coverage:** reframe PromQL start()/end() rejection as permanent parity (#1061)

## [v1.5.0] — 2026-06-24

### Added

- **optcorpus:** record routing decision + cost-grid for route A/B calibration (stage 0) (#1053)

### Fixed

- remediate verified audit findings (#1046)
- **promql:** pass TimeUnix through scalar-wrapped rate arm in range joins (#1045)
- **api:** harden Loki/Tempo HTTP surface against POST + DoS vectors (#1049)
- **chart:** per-head PDB + auto-derived GOMEMLIMIT for split mode (#1040)
- **e2e:** re-provision Grafana after split-mode datasource rewrite (#1037)

### Performance

- **chsql:** push inner-scan time bound on range query lowerings (#1048)

### CI

- **release:** support maintenance-line (release/X.Y.x) hotfix publishing (#1054)
- **pr-label:** self-healing backfill + shared mapping (#1051)
- **mutation:** skip gremlins matrix on non-release PRs (aggregator passes through) (#1052)
- **release:** publish on merge of a validated release PR, not on raw tag (#1044)
- **release:** make opening a release PR label-triggered (#1039)
- **e2e:** isolate split_isolation in its own dashboard shard (#1038)
- **e2e:** run the FULL matrix (split + crawl) on release PRs (#1036)

## [v1.4.0] — 2026-06-22

### Added

- **chart:** split mode — isolated per-head deployments (no proxy) (#1031)
- **api:** O(output) drain counter + falsifiable bounds-drain regression harness (#1030)
- **config:** add CERBERUS_ENABLED_HEADS per-head toggle (#1029)

### Fixed

- **test:** thread a time window into the TraceQL property harness (#1032)
- **config:** default-on per-query sample budget at 5M (#1028)
- **tempo:** bound /api/search drain with SQL trace-limit + window pushdown (#1027)

## [v1.3.0] — 2026-06-19

### Added

- **config:** accept humanized byte sizes (2Gi) for memory caps, BWC-preserving (#1017)

### Fixed

- **telemetry:** apply CERBERUS_LOG_LEVEL to the OTLP slog bridge (stop debug leaking to otel_logs) (#1018)

### CI

- **release:** make release tooling backport-aware (maintenance lines + :latest guard) (#1019)

## [v1.2.0] — 2026-06-19

### Added

- **chclient:** surface ch-go telemetry on cerberus's own telemetry (#1007)

### Fixed

- **loki:** tail drops overflow rows when a poll window exceeds the limit (#1011)
- **release:** gate strictly on main HEAD fully settled green (#1008)

### Documentation

- collapse native-upstream roadmap into a single note (uses-today + positioning) (#1014)
- **roadmap:** mark 5A external-table push DEFERRED — no qualifying call-site (#1010)
- native-CH roadmap execution addendum (code-now + ambitious chase) (#1009)

### Dependencies

- bump `github.com/ClickHouse/ch-go` 0.71 → 0.72, which exposes the client-side query telemetry surfaced in #1007 (#1005)

## [v1.1.0] — 2026-06-19

### Added

- **ci:** manual prepare-release workflow + generator (#1003)
- **chopt:** generate opt feature table from registry + drift gate (#998)
- **config:** generate docs/configuration.md from viper config + CI drift gate (#1000)
- **promql:** adopt native timeSeriesChangesToGrid/ResetsToGrid (25.9) (#990)
- **chclient:** columnar query_range matrix decode via ch-go (flag-gated) (#983)

### Fixed

- **e2e:** gate showcase probes on seed-fixture signal to kill unless-panel flake (#993)
- **e2e:** stop the false-positive DiskPressure breadcrumb mislabelling (#992)
- **test:** serialize chDB engine access to stop SIGABRT in result.Free (#984)

### Performance

- **solver:** share immutable off-spine in plan slicing (copy-on-write) (#988)

### Changed

- **chopt:** adopt columnar decode as a CH_OPTIMIZATIONS feature, drop standalone env (#989)
- **promql:** pure polymorphic range-lowering dispatch (no nil-check) (#986)

### CI

- **forbid-skip:** assert-from-source doc-count gate; fix forbid-skip 6->5 + layer 12->13 drift (#997)
- add internal/external link + doc-to-code reference gates (#999)
- **clickhouse:** central versions.yaml SoT + version-sync gate (#995)
- **e2e:** cache Playwright chromium across e2e shards (#994)
- **forbid-skip:** drop the wording-tests vocabulary scan, keep the five behavioural checks (#991)
- **e2e:** free runner disk before the stack to stop DiskPressure evictions (#981)

### Documentation

- **changelog:** restructure pre-1.1.0 — backfill v1.0.1/v1.0.2, stage [Unreleased] (#1004)
- fix configuration.md anchor links broken by generator (#1000) (#1001)
- sync docs to code ahead of v1.1.0 (feature table, COW, dead links, counts) (#996)
- native-ClickHouse roadmap + staged timeSeriesIncreaseToGrid contribution (#982)

## [v1.0.2] — 2026-06-18

### Added

- **ClickHouse-optimization suite + auto-picker.** A cohesive optimization
  layer driven by two knobs: `CERBERUS_CH_OPTIMIZATIONS` (`auto` | `off` |
  comma-separated feature ids, default `auto`) and
  `CERBERUS_CH_OPTIMIZATIONS_MODE` (`permissive` | `enforcing`, default
  `permissive`). At startup cerberus probes `SELECT version()` once and resolves
  an immutable enabled-set: under `auto` it enables every **stable** feature the
  server supports and never an experimental one; an explicit list honours the
  mode for unsupported features (WARN+skip vs FATAL) and a typo'd id is always
  fatal. The seeded registry: `aggregation_in_order` (24.8, stable, auto-enabled —
  stamps `optimize_aggregation_in_order=1` on sort-key-prefix GROUP BY plans),
  `condition_cache` (25.3, stable — stamps `use_query_condition_cache=1` on
  predicate-stable read paths), and `ts_grid_range` (25.6, experimental,
  explicit-only). Everything is version-safe: a feature whose floor exceeds the
  connected server is simply not enabled, so cerberus keeps emitting its
  24.8-safe SQL. See [`docs/clickhouse-optimizations.md`](docs/clickhouse-optimizations.md).
- **Async `system.query_log` performance-corpus reconciler**
  (`CERBERUS_CH_OPT_CORPUS_ENABLED`, off by default; `CERBERUS_CH_OPT_CORPUS_INTERVAL`,
  `CERBERUS_CH_OPT_CORPUS_SINK_PATH`). A bounded background reconciler joins
  recently-dispatched cerberus query_ids back to `system.query_log` for their
  server-side cost (read rows/bytes, duration, memory, ProfileEvents) and
  appends `(shape-id, opts, timings)` tuples to a durable JSONL sink an operator
  can mine. Production-only (chDB has no `system.query_log`); errors are logged,
  never fatal. The dispatch seam is non-blocking and O(1) (a single buffered
  channel send into a fixed-size circular ring), so it never serializes the
  prom/loki/tempo heads or taxes the data plane; the `system.query_log` scan is
  resource-capped (`max_execution_time`, `max_threads=1`, low `priority`,
  row/byte read limits) so it cannot starve data-plane queries.

### Deprecated

- **`CERBERUS_EXPERIMENTAL_TS_GRID_RANGE`** is soft-deprecated in favour of
  `CERBERUS_CH_OPTIMIZATIONS` (list `ts_grid_range`). It keeps working — it is
  re-routed through the optimization resolver (under `auto`: explicit `true`
  force-enables, `false` force-disables, unset has no effect; any explicit
  `CERBERUS_CH_OPTIMIZATIONS` choice — a list **or** the `off` kill-switch —
  overrides the legacy flag, so `off` stays absolute) — and emits a one-time
  startup deprecation warning.

### Changed

- **Per-query instrumentation** (query_id / `log_comment` shape id) and a
  ClickHouse settings map, plus the `aggregation_in_order` optimization. (#978)
- `histogram_quantile` phi-domain handling (+/-Inf out of range) and
  `vector(scalar)` vector-typing fixes. (#974)

## [v1.0.1] — 2026-06-18

### Added

- **Publishable cerberus Helm chart + OCI release pipeline**, exposing the full
  `CERBERUS_*` config surface, prod-HA typed values with ClickHouse co-location,
  and a chart-validate / kubeconform / helm-docs drift gate. (#962, #968)

### Fixed

- Restore integer per-head admission caps with bool aliases. (#973)
- `on()`/`ignoring()` one-to-one binop leaking non-matching labels; vector-join
  dropping operand `MetricName`/`TimeUnix` (code-47). (#971)
- Uniform boolean parsing (1/0/true/false) across all `CERBERUS_*` env vars.

## [v1.0.0] — 2026-06-17

First general-availability release. Cerberus is a drop-in Prometheus /
Loki / Tempo HTTP gateway for ClickHouse: each head parses with its
reference upstream parser, lowers to a shared plan IR, runs a rule-based
optimizer, and emits parameterised ClickHouse SQL — so Grafana, alerting,
and CLI tooling see three normal datasources speaking unmodified PromQL /
LogQL / TraceQL.

**Wire-format API stability.** The three upstream HTTP surfaces cerberus
serves are the 1.0 compatibility contract and follow semantic versioning
from here. The query languages are the upstream parsers' own, so they
track upstream. The `CERBERUS_*` configuration surface is stable;
additive changes only within 1.x.

This is a young, actively-developed project: a confident 1.0 because the
behaviour is held to reference engines by differential harnesses on every
merge — not because every edge is explored. Two areas carry honestly lower
confidence and are called out below.

### Capabilities at 1.0

- **PromQL** scored against the third-party CNCF / PromLabs
  [PromQL Compliance Tester](https://github.com/prometheus/compliance) —
  574/574 cases passing against a real Prometheus, no allow-list — plus
  subqueries, `histogram_quantile` over classic and native histograms,
  `predict_linear` / `holt_winters`, `@start()` / `@end()`,
  `group_left` / `group_right`, and the full instant + range-query surface.
- **LogQL** diffed against a real Loki on Grafana's own `pkg/logql/bench`
  corpus: pipeline stages (`| json` / `| logfmt` / `| pattern` / `| unpack`
  / `| line_format` / `| label_format` / …), metric queries, structured
  metadata, and the `/labels` / `/series` / `/index/stats` metadata surface.
- **TraceQL** diffed against a real Tempo: structural operators, nested-set
  intrinsics in `| select(...)`, set ops, `group` / `coalesce`, the metrics
  pipeline, and the `/api/search` + tag-discovery surface.
- **Coverage** ([`docs/coverage.md`](docs/coverage.md)): 226 of 228
  catalogued symbols supported across the three heads, 2 intentional
  parity rejections (bare `start()` / `end()`), **zero** wrong-rejections.
- **OpenTelemetry-native** schema (the `clickhouseexporter` table shape),
  with resource attributes projected as Prometheus labels and the
  `CERBERUS_SCHEMA_*` overrides for non-default layouts.
- **Operations**: `ReplicatedMergeTree` / `Replicated`-database schema
  bootstrap (`CERBERUS_AUTO_CREATE_SCHEMA`), per-head ClickHouse circuit
  breakers, OTLP self-telemetry export, `/readyz` / `/healthz` probes, and
  the full ClickHouse connection surface (TLS, timeouts, pool sizing).
- **Performance**: single-pass prefix-sum range aggregation, sharded
  pushdown solver, PREWHERE promotion + late materialisation, metadata
  fan-in batching, and an optional experimental native-rate path
  (`CERBERUS_EXPERIMENTAL_TS_GRID_RANGE`, off by default) — all held
  against regression by the compute-fan-out perf-guard ratchets.

### Changed since the v1.0.0-rc series

- **Metadata endpoints scan the full `[start,end]` window.** `/api/v1/series`,
  `/labels`, and `/label/<name>/values` enumerate every series/label/value
  with any sample in the requested window instead of an instant staleness
  window at `now`, fixing intermittent empty results for late-arriving
  (delta-temporality) data.
- **Instant range-vector queries anchor to `time=T`**, not ClickHouse
  wall-clock, closing an intermittent empty-window class.
- **GCP / cloud metric-name translation**: slash-containing OTel names,
  `histogram_quantile` over `sum_over_time` of delta-histogram buckets,
  aggregated-range ÷ scalar, and standalone `_sum` / `_count`-suffixed
  gauges all resolve.
- **Resource attributes as Prometheus labels** (env / namespace / pod /
  cluster) with bounded query-time memory.
- **Replicated schema**: emit explicit bare `ReplicatedMergeTree` under a
  `Replicated` database; cold-cluster boot creates the database itself
  instead of fatally exiting.
- **Full ClickHouse connection configuration** surface exposed (TLS,
  read timeout, pool limits).

### Known limitations (honest at 1.0)

- **TraceQL conformance is the lightest of the three heads.** There is no
  third-party TraceQL conformance suite; its corpus is cerberus-owned
  author-written TXTAR diffed against a real Tempo, so its breadth is
  author-bounded rather than reference-derived. Raising TraceQL's
  confidence is the top post-1.0 item. See
  [`docs/compatibility.md`](docs/compatibility.md).
- **Cerberus is a query gateway, not a store.** It runs no ingestion and
  caches nothing (only the `/readyz` TTL); bring your own ClickHouse and
  OTel pipeline.
- **Per-head circuit-breaker recovery may briefly flap** as HALF-OPEN
  probes re-converge after a ClickHouse restart ([#94]).

## [v1.0.0-rc.1]

The first published release candidate on the `v1.0.0` line — the core
slice plus the advanced-QL surface. This was an early cut: the three
heads parse → lower → execute and the differential harnesses gate every
merge, but the surface was still evolving. The `rc.2` → `rc.9`
prereleases that followed (replicated-schema bootstrap, resource-attribute
labels, the perf collapses, and the GCP / metadata query-translation
fixes) are summarised under [v1.0.0] above and listed individually on the
[releases page](https://github.com/tsouza/cerberus/releases).

### Added

- TraceQL nested-set intrinsics in `| select(...)` — the projection Grafana Traces Drilldown's "Structure" tab sends (`… | select(status, resource.service.name, name, nestedSetParent, nestedSetLeft, nestedSetRight)`) now works end-to-end instead of 422ing. A new `chplan.NestedSetAnnotate` node recomputes reference Tempo's ingest-time nested-set numbering at query time (recursive CTE over the `(TraceId, SpanId, ParentSpanId)` adjacency; DFS bounds per `assignNestedSetModelBoundsAndServiceStats` semantics: counter from 1, root parent `-1`, disconnected spans `0/0/0`, counter continues across multiple roots; sibling order is `(Timestamp, sipHash64(SpanId))` since OTel-CH does not record Tempo's ingest order). `/api/search` responses now surface user-selected attribute values inside `spanSets[].spans[].attributes` (OTLP `intValue` for nested-set intrinsics, `stringValue` otherwise, lowercased `status` / `kind` enum casing per Tempo's wire encoding) and populate the per-span `name` field when `select(name)` requests it — exactly reference Tempo's placement. Two more latent bugs on the same Drilldown path are fixed: mixed structural/plain `||` arms are now column-aligned (ClickHouse rejected the positional `UNION DISTINCT` with code 258), and structural-join wrap subqueries expose the columns `select()` can read (`StatusCode`, `SpanAttributes`, …) so `{A} >> {B} | select(status)` resolves. The PLAIN-FILTER arm gets the same plumbing: the optimizer's `ProjectionPushdown` expression walker now descends into every child-bearing `chplan.Expr` kind (`FieldAccess` sources, `Subscript`, `Lambda` bodies, the map-carrier nodes, `NestedArrayExists` values — plus the `NestedArrayExists.Column` string carrier), so `{ status = error } | select(span.http.method, resource.service.name)` no longer prunes `SpanAttributes` out of the narrowed scan (ClickHouse error 47 → HTTP 502 on the showcase "select / by / coalesce" panel).

- Sort-key-aware filter emission + `PREWHERE` promotion. The chsql emitter now fuses `Filter(Scan)` into a single `SELECT … FROM <table> [PREWHERE …] WHERE …` and partitions conjuncts into a sort-prefix bucket / skip-index bucket / rest, then promotes cheap predicates that touch no wide column into `PREWHERE` when the projection reads any wide column. ~219 existing TXTAR fixtures across `test/spec/{chsql,promql,logql,traceql,optimizer}` were re-emitted; the diff is a pure structural rewrite (one less subquery layer, predicates reordered by sort-key rank, optional `PREWHERE` split) and the rendered SQL is semantically equivalent. 29 of the re-emitted fixtures now carry a `PREWHERE` clause. New unit tests cover the predicate classifier (`prewhere_test.go`); new `test/spec/codegen/prewhere/` fixtures pin the four codegen-only behaviours (wide-column-excluded, partial-promotion, no-wide-no-promotion, sort-prefix-order).

#### Advanced QL

The advanced-QL surface: PromQL subqueries (P0 4.1–4.11), `predict_linear` / `holt_winters` / `@start()` / `@end()`, `histogram_quantile` over both classic and native (exp) histograms, `group_left` / `group_right` cardinality edges; LogQL `| unpack`, `| pattern`, `| line_format`, `| decolorize`, `| label_format` template stages with Loki template funcs, `bytes_*` alignment, `/api/v1/tail` WebSocket, `/labels`, `/label/.../values`, `/series`, `/detected_fields`, `/patterns`, `/index/stats`, `/index/volume`; TraceQL `status = error` / `kind = client` enum statics, `sum / avg / max / min` over inner attributes, link traversal + span-event queries, set ops, `group / coalesce` pipeline elements, `histogram_over_time`, MetricsPipeline lowering, Tempo `/api/search/recent`, `/api/search/tags`, `/api/search/tag/<n>/values`, `/api/metrics/query_range`. The Tempo `unsafe.Pointer` shim is retired via the [`tsouza/tempo:cerberus-accessors`](https://github.com/tsouza/tempo/tree/cerberus-accessors) fork; the OTel CH Exporter schema is the source of truth via the [`tsouza/opentelemetry-collector-contrib:cerberus-ddl`](https://github.com/tsouza/opentelemetry-collector-contrib/tree/cerberus-ddl) fork (no hand-maintained DDL).

#### PromQL

- Fold scalar-only PromQL in Go for Grafana's health probe (`1+1`-style queries). [#95]
- `chplan.RangeWindow.OuterRange` + `Identity` for subquery emit. [#98]
- Step-grid SQL emission for matrix-shape `RangeWindow`. [#101]
- Wire matrix `RangeWindow` through `chclient.Sample` shape (P0 4.4). [#104]
- Lower subquery over range-vector calls — `max_over_time(rate(m[5m])[1h:5m])` (P0 4.6). [#107]
- Pin optimizer "no mis-rewrite" on matrix `RangeWindow` (P0 4.9). [#109]
- Subquery roadmap + `Lower()` docs (P0 4.11). [#110]
- Subquery E2E + Playwright coverage (P0 4.10). [#111]
- `/api/v1/format_query` + `/api/v1/parse_query` handlers. [#114]
- `/api/v1/query_exemplars` handler. [#137]
- `group_left` / `group_right` cardinality + extra-label edges. [#144]
- `predict_linear`, `holt_winters`, `@start()` / `@end()` modifiers. [#159]
- `histogram_quantile` on classic histograms. [#170]
- `histogram_quantile` on native (exp) histograms. [#171]

#### LogQL

- Point stale "not yet supported" LogQL messages at the implemented stages. [#118]
- `| line_format` + `| decolorize` as Go-side post-process. [#124]
- Handle nil predicate from no-op stages; `line_format` / `decolorize` handler tests. [#127]
- TXTAR fixtures for `| line_format` and `| decolorize`. [#128]
- `| label_format` rename + template stages. [#130]
- Expose Loki template funcs in `| line_format` / `| label_format`. [#132]
- `/index/stats` + `/index/volume` handlers. [#141]
- `| unpack` + `| pattern` parser stages. [#142]
- `/labels`, `/label/values`, `/series`, `/detected_fields`, `/patterns` handlers. [#151]
- `bytes_*` alignment + `/api/v1/tail` WebSocket. [#157]

#### TraceQL

- Lower `status` / `kind` static literals (P0 6). [#96]
- Lower `sum` / `avg` / `max` / `min` over inner attribute (P0 7). [#99]
- `/api/search/recent` + `chplan.OrderBy` IR node. [#123]
- Cover `/api/search/recent` handler edges. [#126]
- `/api/search/tags` + `/api/search/tag/<name>/values`. [#150]
- `MetricsPipeline` lowering — `rate` / `*_over_time` → `Aggregate(Scan(traces))`. [#153]
- Set ops (`&&`, `||`, `~`) + `group` / `coalesce` pipeline elements. [#156]
- `MetricsAggregate` IR + `RangeWindow` over metric-shape input. [#160]
- `/api/metrics/query_range` handler. [#163]
- Link traversal + span-event queries. [#169]
- `histogram_over_time(attr)` lowering + emission. [#173]

#### Schema source-of-truth (plus the `tsouza/opentelemetry-collector-contrib` fork)

- Wire `tsouza/opentelemetry-collector-contrib:cerberus-ddl` via `replace`. [#154]
- Mirror upstream OTel CH Exporter columns (exp_histogram + missing classic histogram fields). [#158]
- Wrap upstream OTel CH Exporter DDL via the `schema/ddl` package. [#161]
- `CERBERUS_AUTO_CREATE_SCHEMA` env-gated startup hook. [#166]
- Refactor `harness/prometheus-compliance` to seed via `schema/ddl` package. [#167]
- Refactor `test/e2e` to seed via `schema/ddl` package + Go fixture inserts. [#168]
- Self-contained `otel-collector` + `sample-app` for real E2E data. [#172]

#### Tooling, CI, repo hygiene

- Daily Dependabot for upstream parsers + auto-merge. [#100]
- Bump Playwright deps. [#102]
- Align markdown tables for MD060 (markdownlint v0.40). [#105]
- Don't advance `:latest` on prereleases + SLSA attestation. [#112]
- Defer P0 3 (k3s otel-collector) + P0 5 (recursive `>>` / `<<`). [#113]
- Align indented table that PR #105 missed. [#117]
- Consolidate lint into one job + add `bodyclose`, `errorlint`. [#119]
- Switch `auto-merge-deps` to `workflow_run` trigger. [#120]
- Bump the github-actions group across 1 directory with 10 updates. [#121]
- SQL-builder evaluation (R6.0) recommends custom builder. [#125]
- Align CLAUDE.md + roadmap with R6.0 custom-builder choice. [#129]
- Scaffold public `chsql.Builder` + `QueryBuilder` (R6.1). [#131]
- Fuzz + perf-benchmark workflow scaffolds. [#133]
- Scaffold local Go PromQL evaluator. [#134]
- Pattern-based optimizer `Rule` API scaffold. [#135]
- Scaffold differential-testing harness. [#136]
- Port `emitScan` / `Filter` / `Project` / `Limit` to Builder (R6.2). [#138]
- Plan to fork upstream Tempo, retire `unsafe.Pointer` shims. [#139]
- Port `emitAggregate` + `emitAggFunc` to Builder (R6.3). [#140]
- Wire `tsouza/tempo:cerberus-accessors` fork via `replace` directive. [#143]
- Run dashboard E2E on merge-to-main only, drop PR trigger. [#145]
- Actually use `QueryBuilder` for R6.2 / R6.3 ports + repo audit. [#146]
- Retire `unsafe.Pointer` + reflect shims via `tsouza/tempo` accessors. [#148]
- Tighten no-raw-SQL rule to forbid `Builder.WriteSQL` clause keywords. [#149]
- `forbidigo` lint forbids `unsafe.Pointer` + `reflect.FieldByName` in `internal/traceql`, `internal/api/tempo`. [#152]
- Plan 6 scalability levers. [#155]
- Add lefthook pre-commit + commit-msg hooks (formatters only; CI owns validation). [#162]
- Drop empty `pkg/` — cerberus is a service, not a library. [#165]

#### Core slice

The seed (PR1–PR7 + admin + v0.1.0) plus M1–M4 (full PromQL / LogQL / TraceQL parsing → lowering → execution) + corpus expansion (TXTAR 122 → 166 fixtures, ~280 new unit-test sub-cases, E2E HTTP 12 → 26, Playwright 10 → 19 scenarios).

#### PromQL (M1.1 – M1.7)

- Real `RangeWindow` SQL emission via the promshim-clickhouse windowed-array idiom (`groupArray` + `arraySort` + `arrayFilter` + `arrayPopBack/Front` for counter-reset deltas). [#40]
- `BinaryExpr` lowering: scalar/vector arithmetic and pow / mod. [#41]
- Instant-vector functions: `abs`, `ceil`, `floor`, `round`, `sqrt`, `exp`, `ln`, `log2`, `log10`, `sgn`. [#42]
- Aggregation completeness: `without (...)` (new `chplan.MapWithoutKeys`), `stddev`, `stdvar`, `group`, parameterised `quantile(phi, ...)`. [#43]
- `offset` and `@` modifiers thread through `RangeWindow.Offset` / `End` plus a `Timestamp <= anchor` predicate on instant-vector queries. [#44]
- Vector matching: default + `on(...)` + `ignoring(...)` via the new `chplan.VectorJoin` (per-series argMax + INNER JOIN). [#45]
- Comparison ops + `bool` modifier (Filter shape vs `toFloat64(...)` Project). [#48]
- Clamp family and 2-arg `round(v, to_nearest)`. [#49]

#### Prom HTTP API (M2.1 – M2.7)

- Real per-step bucketing in `/api/v1/query_range` with 5-min lookback. [#50]
- Aggregate result shaping — Sample-shape Project on top of `chplan.Aggregate` so `sum by (job)` etc. flow through the existing chclient decoder. [#52]
- `/api/v1/labels`, `/api/v1/label/{name}/values`, `/api/v1/series` with UNION ALL across metric tables. [#51]
- `/api/v1/metadata` sourcing `MetricDescription` + `MetricUnit` from each table. [#53]
- `X-Prometheus-API-Version` + `X-Cerberus-CH-Millis` debug headers via a header-stamping middleware that times each CH call into a request-scoped counter; `match[]` selector support on `/labels` and `/label/.../values`. [#54]

#### LogQL (M3.1 – M3.5)

- `schema.Logs` + `chplan.LineContent`; stream selectors (`{job="api"}`) and the line-filter family (`|=` / `!=` / `|~` / `!~`) with chained-filter AND-folding and `or`-disjunction. [#55]
- Label filters (`| label="val"` / `| label=~"r"`); `BinaryLabelFilter` and `LineFilterLabelFilter` share the same `*labels.Matcher`-based lowering helper. [#58]
- Metric form: `rate({...}[5m])`, `count_over_time(...)`, `bytes_rate(...)`, `bytes_over_time(...)`. New `log_rate` emitter binds `range_seconds` via a `?` placeholder rather than Sprintf'ing it inline. [#61]
- Aggregations: `sum(rate(...))`, `avg by (job) (count_over_time(...))`, `sum without (pod) (...)`, with stddev / stdvar / group / quantile parity to PromQL. [#62]
- Loki HTTP `query` + `query_range` handlers; metric queries return Prom-style matrix/vector, log queries return Loki "streams" shape. [#63]

#### TraceQL (M4.1 – M4.5)

- `schema.Traces` + `chplan.FieldAccess` for dotted-path attribute references; SpansetFilter with intrinsic resolution (`duration`, `name`, `kind`, `status`, `statusMessage`, `parent`, scoped `trace:id` / `span:id`) and scope-prefixed paths (`resource.` → ResourceAttributes, `span.` → SpanAttributes). [#64]
- Direct structural ops `>` (parent of) and `<` (child of) via `chplan.StructuralJoin` rendering an INNER JOIN of two span subqueries on `(TraceId, ParentSpanId)`. [#65]
- `| count() > 0` aggregate + scalar-filter wrapping; reuses the M1.4 `chplan.Aggregate` shape. [#66]
- `| select(span.x, resource.y)` projection: reflects out `SelectOperation.attrs` (Tempo keeps it on an unexported field) and emits one column per requested attribute aliased to its TraceQL name. [#70]
- Tempo HTTP API: `/api/echo`, `/api/status/version`, `/api/search?q=<TraceQL>`, `/api/traces/{id}`. trace-by-id skips the parser and builds the chplan tree directly. Tempo's distinct error envelope (`{"traceID":"","spanID":"","error":true,"message":"..."}`) drives Grafana's "trace not found" UI. [#71]

#### Test corpus expansion

- TraceQL TXTAR fixtures grow from 8 to 26 — boolean `||`, regex / not-regex matchers, every intrinsic (`name`, `kind`, `statusMessage`, `parent`, scoped `trace:id` / `span:id`), span-attribute scoping variants, scalar-filter thresholds, and resource-scoped select projection. [#72]
- chsql TXTAR fixtures grow from 15 to 29 — direct tests for every chplan IR node (VectorJoin, StructuralJoin, MapWithoutKeys, LineContent variants, parameterised `quantile`, RangeWindow with `Offset` + LogQL `log_rate`, FieldAccess, FuncCall). [#74]
- Meaningful Grafana Playwright scenarios for all three datasources: LogQL streams + metric, TraceQL search + traceByID, richer PromQL (rate matrix + labels + metric names). Per-signal seed files (`otel_logs.sql`, `otel_traces.sql`). [#76]
- Cerberus-side HTTP integration tests for every shipped surface: Prom rate / labels / label-values, Loki streams + metric, Tempo echo / version / search / trace-by-id (found + not-found). [#77]

#### Engineering / CI

- Required-status checks: `check`, `lint`, `dashboard` (full-stack k3d + cerberus + Grafana + Playwright smoke). `enforce_admins: true`; `gh pr merge --admin` is forbidden. [#56, #59, #60]
- Compatibility harness drops the `pull_request` trigger initially; runs nightly + on `main` push as informational baseline. [#56]
- Hard rule established: no `fmt.Sprintf` (or string concatenation) for ClickHouse SQL going forward; existing emitter Sprintf is grandfathered until the typed-builder port replaces it. [#57]
- SQL-builder evaluation: a written security + impact + build-vs-buy analysis recommends third-party (`huandu/go-sqlbuilder` + cerberus extension layer), custom (`internal/chsql.Builder`), or defer. [#73]
- `internal/engine/` ExecutionEngine framework scoped with the same evaluation-first pattern: audit pipeline divergence across 5 callsites before any code lands; recommendation among (a) Build, (b) Partial — helpers-only extraction, (c) Defer. [#75]

### Changed

- `QueryBuilder.Limit(int64)` and Builder API refactors to thread typed clauses through the chplan emitter (R6.2 / R6.3 audit). [#138, #140, #146]
- Migrate test seeders (`harness/prometheus-compliance`, `test/e2e`) off hand-rolled `*.sql` files onto the upstream-derived `schema/ddl` package. [#167, #168]

### Security

- Bump `apache/thrift` v0.22.0 → v0.23.0 (CVE-2026-41602). [#164]

### Infrastructure

- `tsouza/tempo:cerberus-accessors` fork wired via `replace` to retire the `unsafe.Pointer` shim ([#143]); shim removed in [#148]; `forbidigo` gate added in [#152].
- `tsouza/opentelemetry-collector-contrib:cerberus-ddl` fork wired via `replace` so the OTel CH Exporter DDL is the source of truth (no hand-maintained schema). [#154]
- Lefthook pre-commit + commit-msg hooks (formatters only; CI owns validation). [#162]
- `auto-merge-deps` switched to `workflow_run` trigger; `dashboard` job moved to merge-to-main only. [#120, #145]
- Self-contained k3s deployment: per-node OTel Collector DaemonSet + gateway Deployment + sample-app `telemetrygen` for real E2E data. [#172]

### Documentation

- New **per-function / per-construct coverage matrix** ([`docs/coverage.md`](docs/coverage.md)), the user-facing answer to "does cerberus support the queries my dashboards run?". Every PromQL function / aggregation / operator / modifier, every LogQL stage / aggregation / filter, and every TraceQL intrinsic / metrics-op (228 symbols across the three heads) is listed with an honest support status — Supported, Supported (experimental), Supported (cerberus extension), or Rejected (parity with reference). The tables are generated from the `test/surface-parity/inventory.json` conformance ledger (`scripts/gen-coverage.py`), translating the ledger's machine-readable `parity-accept` / `parity-reject` / `wrong-accept` / `wrong-reject` classes into user-facing support language. Current coverage: 226 of 228 symbols supported, 2 intentional parity rejections (bare `start()` / `end()`), and **zero** wrong-rejections. Linked from the README documentation index.

- **`docs/test-strategy.md` layer map + CI-gate inventory reconciled with reality.** The map is now 12 layers (was understated at 11): added Layer 6d (the function-surface parity ledger — `test/surface-parity/` + `test/rejection-parity/` + `test/inventory/`) and Layer 12 (compute fan-out guards — the static fan-out lint, per-construct scaling harness, cardinality / scale-wall / solver-decision ratchets, and the corpus profiler). The CI-gate table now lists every job that runs — including the previously-omitted `compatibility/promql-surface`, `compatibility/prometheus-forced-route`, `perf-guards`, `perf-profile`, `startup-bench`, and `coverage` lanes — with each one's accurate required-vs-informational status (the eleven required checks are spelled out explicitly).

- **Operations: circuit-breaker blast radius documented.** [`docs/operations.md`](docs/operations.md) now spells out that the ClickHouse circuit breaker is a *single* per-`Client` breaker shared across all three API heads **and** the `/readyz` pinger, so one trip 503s every head and flips `/readyz` red (evicting the pod under Kubernetes) — an all-or-nothing, whole-replica coupling operators must tune against. The per-query wall-clock timeout (`CERBERUS_QUERY_TIMEOUT`) is cross-referenced as the separate bound for slow-but-healthy queries, closing the documented "no query deadline" gap.

- **Configuration file documented.** [`docs/configuration.md`](docs/configuration.md) gains a "Configuration file (optional)" section describing the viper loader's optional `cerberus.yaml` (probed in `.` then `/etc/cerberus`), the env > file > default precedence, and the missing-or-malformed-is-tolerated contract. The stale "there is no YAML file to load" claim in `configuration.md` and `operations.md` is corrected.

- **Native-rate parity prose reconciled** (`operations.md`). The dual-emit ULP-divergence statement now cites the test-enforced bound — at most two cells diverge, each by no more than 1 ULP (`maxDualEmitUlpDivergentCells = 2`) — and the observed pinned-fixture count (8 of 9 cells bit-identical), replacing the unverified "16/18" figure. `performance.md`'s `perf-guards` lane is corrected from "required" to its actual informational status.

### Known gaps captured at the core-slice point

Tracked at the time the core slice landed; the advanced-QL work above
delivers most of these. The remainder are honest gaps still to close:

- PromQL: `topk` / `bottomk` / `count_values` (output-shape changes).
- LogQL: `| json`, `| logfmt`, `| regexp` parser stages; `unwrap`-based ops.
- TraceQL: recursive structural ops `>>` / `<<` and sibling ops.

### Known limitations / experimental notes (RC1 posture)

RC1 is an early, experimental cut. The differential harnesses gate every
merge, but correctness, performance, and operational behaviour are still
being shaken out — **validate cerberus against your own corpus before
pointing anything real at it** (see the README warning). The
maintainer-accepted caveats specific to this candidate:

- **TraceQL conformance is the weakest of the three heads.** PromQL is
  scored against the third-party CNCF / PromLabs
  [PromQL Compliance Tester](https://github.com/prometheus/compliance) and
  LogQL against a real reference Loki seeded from Grafana's own
  `pkg/logql/bench` corpus, but **there is no third-party TraceQL
  conformance suite** to draw on. The TraceQL harness diffs against a real
  reference Tempo, yet its corpus is author-written cerberus-owned TXTAR, so
  its breadth is author-bounded rather than reference-derived. Raising
  TraceQL's confidence is the top post-RC1 improvement item. See
  [`docs/compatibility.md`](docs/compatibility.md#traceql--cerberus-owned-driver)
  for the per-head confidence table and the full reasoning.

- **Per-head circuit-breaker isolation is in place but not fully hardened
  ([#94]).** The single `chclient.Client` holds a registry of breakers —
  one per data-plane head (`prom` / `loki` / `tempo`) plus a dedicated
  `probe` breaker for `/readyz` — so one head tripping OPEN no longer 503s
  the others or evicts the pod (see
  [`docs/operations.md`](docs/operations.md#clickhouse-circuit-breaker) for
  the blast-radius contract). Full isolation and independent-recovery
  hardening is post-RC1 work: after a ClickHouse restart, recovery may
  briefly flap as the per-head HALF-OPEN probes re-converge. This is being
  improved separately and is a known experimental rough edge, not a
  blocker.

- **Not production-ready.** Cerberus remains experimental and under active
  development; the surface is evolving and breaking changes are expected.
  Do not stand it in for a running Prometheus / Loki / Tempo deployment
  without first evaluating it against your own queries and data.

## v0.1.0 — Seed (pre-release history, not tagged)

The seed series (PR1–PR7 + admin + roadmap) that predates the published
`v1.0.0-rc.*` tags:

- Module `github.com/tsouza/cerberus` on `go 1.26.2` with the `replace github.com/hashicorp/memberlist => github.com/grafana/memberlist@…` hygiene fix.
- Shared plan IR (`internal/chplan`), ClickHouse SQL emitter (`internal/chsql`), TXTAR spec runner under `test/spec/`.
- Rule-based optimizer (`internal/optimizer`) with three rules: filter fusion, constant folding, projection pushdown.
- PromQL vertical slice (`internal/promql/lower.go`) covering instant vector selectors, label matchers (eq / ne / regex), range vectors (placeholder SQL), and aggregations (`sum`, `count` with `by(…)`).
- HTTP API surface (`internal/api/prom`) for `/api/v1/query` + `/api/v1/query_range` (range_range returns a single point until full `RangeWindow` lowering lands in M1.1).
- CH client wrapper (`internal/chclient`) over `clickhouse-go/v2` with a testcontainers integration test.
- CI: two-job workflow (`check` + `lint`), commitlint relaxed for Dependabot, markdownlint, mutation testing (gremlins) on a nightly cron.
- Branch protection on `main`: required checks, linear history, no force pushes / deletions.

[v1.0.0]: https://github.com/tsouza/cerberus/releases/tag/v1.0.0
[v1.0.0-rc.1]: https://github.com/tsouza/cerberus/releases/tag/v1.0.0-rc.1
