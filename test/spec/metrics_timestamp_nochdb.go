//go:build !chdb

package spec

import "testing"

// RunMetricsTimestampDifferential is a no-op when the `chdb` build tag is
// not set. The real implementation lives in metrics_timestamp_chdb.go.
func RunMetricsTimestampDifferential(t *testing.T, c *Case, dateTime64, dateTime Statement) {
	t.Helper()
	_, _, _ = c, dateTime64, dateTime
}
