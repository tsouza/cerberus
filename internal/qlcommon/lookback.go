package qlcommon

import "time"

// InstantLookback is the default Prometheus staleness window: how far
// back an instant-vector selector looks for the latest sample per
// series. Prom defaults to 5 minutes; cerberus matches the upstream
// constant rather than reading a per-deployment override, so the LWR
// predicate behaves predictably across environments.
//
// One owner, three consumers: PromQL's own instant-selector lowering
// (internal/promql/modifiers.go), PromQL's subquery staleness lookback
// (internal/promql/subquery.go, which aliases this value rather than
// redeclaring it), and Loki's instant-query handler
// (internal/api/loki/handler.go), which windows its evaluation to
// `[ts - InstantLookback, ts]` per the same upstream contract. See
// #1470.
const InstantLookback = 5 * time.Minute
