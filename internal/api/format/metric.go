package format

// WithMetricName returns a shallow copy of labels with the
// Prometheus-canonical __name__ entry set to name (if non-empty).
// Used by the Prom-flavored API to round-trip CH's separate
// metric-name column into the standard label form.
func WithMetricName(labels map[string]string, name string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	if name != "" {
		out["__name__"] = name
	}
	return out
}
