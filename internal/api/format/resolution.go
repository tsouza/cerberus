package format

// MaxResolutionPoints caps the number of points returned per timeseries on a
// range query: (end-start)/step must not exceed it. The ceiling is shared
// across the Prom, Loki, and Tempo heads so an unauthenticated client cannot
// force an arbitrarily wide matrix fan-out (compute-DoS) on any of them, and
// so all three reject the same shape. The value mirrors upstream Prometheus's
// web/api/v1.queryRange ceiling (sufficient for 60s resolution over a week, or
// 1h over a year), so Prom clients that already handle Prom's cap see
// identical behaviour.
const MaxResolutionPoints = 11000

// ResolutionCapMessage is the bad_data error body emitted when a range query
// exceeds MaxResolutionPoints. It is byte-identical to upstream Prometheus's
// message so clients (and the compat harness) that match on it keep working;
// the Loki and Tempo heads reuse it for cross-head consistency.
const ResolutionCapMessage = "exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)"
