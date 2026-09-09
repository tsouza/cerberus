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
// `[ts - InstantLookback, ts]`. See #1470.
//
// The Loki consumer is NOT an upstream contract, and this comment used
// to claim it was. Loki has its own instant lookback,
// `-querier.engine.max-lookback-period`, whose default is 30s
// (pkg/logql/engine.go) and whose one window-shifting use is
// pkg/logql/evaluator.go's `params.Start = params.Start.Add(-maxLookBackPeriod)`.
// But reference Loki never reaches it over HTTP for a log selector:
// `QuerierAPI.InstantQueryHandler` (pkg/querier/http.go) rejects
// anything that is not a `SampleExpr` with
// `ErrUnsupportedSyntaxForInstantQuery` — "log queries are not supported
// as an instant query type" — which is a 400. So cerberus serving
// instant log queries at all is a deliberate superset of upstream, and
// the 5m envelope is cerberus's own choice on a path with no reference
// behaviour to match.
const InstantLookback = 5 * time.Minute
