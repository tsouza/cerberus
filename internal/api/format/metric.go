package format

// WithMetricName returns a shallow copy of labels with the
// Prometheus-canonical __name__ entry set to name (if non-empty).
// Used by the Prom-flavored API to round-trip CH's separate
// metric-name column into the standard label form.
//
// The name is passed through [OTelToPromMetric] so OTel-stored dotted
// names (`http.server.request.duration`) surface on the wire as the
// Prom-grammar form (`http_server_request_duration`). This matches the
// `/api/v1/label/__name__/values` catalog endpoint, which already
// normalises stored MetricNames the same way — so a metric the catalog
// lists as `http_server_request_duration` also appears with that
// `__name__` value in `/api/v1/series` and `/api/v1/query{,_range}`
// responses. Without the symmetric normalisation a Drilldown-Metrics
// label-chip fetch (catalog → bare-name match → series response) would
// see the catalog's underscored name but the series response's dotted
// `__name__` and flag the round-trip as a mismatch.
func WithMetricName(labels map[string]string, name string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	if name != "" {
		out["__name__"] = OTelToPromMetric(name)
	}
	return out
}
