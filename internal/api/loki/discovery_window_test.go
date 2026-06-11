package loki_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestDiscoveryEndpoints_HonorStartEnd pins the time-bounding contract
// of every Loki discovery endpoint Grafana's Logs Drilldown app uses to
// build and render its service pages: the app's service list comes from
// /index/volume issued WITH the scene time range (grafana/logs-drilldown
// ServiceSelectionScene → WrappedLokiDatasource.getVolume), and the
// label / field surfaces hit /labels, /label/{name}/values, /series and
// /index/stats with the same range. A service may only appear in the
// list if it has rows INSIDE the window — reference Loki scopes all of
// these calls to [start, end], and a cerberus regression that drops (or
// widens) the clamp would resurrect services whose pages then render
// "No data" for the same range (the k3d crawl's bug-B hypothesis on run
// 27327766381; the audit found cerberus already windowed all five —
// this test keeps it that way).
//
// The emitted bound is an inline `toDateTime64('…', 9)` literal (see
// timeBoundFrag), so the assertions check the rendered SQL itself for
// both half-open bounds carrying the requested epochs.
func TestDiscoveryEndpoints_HonorStartEnd(t *testing.T) {
	t.Parallel()

	const (
		startEpoch = 1717995600 // 2024-06-10 05:00:00 UTC
		endEpoch   = 1717999200 // 2024-06-10 06:00:00 UTC
	)
	wantStart := time.Unix(startEpoch, 0).UTC().Format("2006-01-02 15:04:05")
	wantEnd := time.Unix(endEpoch, 0).UTC().Format("2006-01-02 15:04:05")

	paths := map[string]string{
		"index_volume": `/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`,
		"index_stats":  `/loki/api/v1/index/stats?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`,
		"labels":       `/loki/api/v1/labels?start=1717995600&end=1717999200`,
		"label_values": `/loki/api/v1/label/service_name/values?start=1717995600&end=1717999200`,
		"series":       `/loki/api/v1/series?match%5B%5D=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`,
	}

	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			q := &stubQuerier{}
			srv := newServer(q)
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}

			sql := q.LastSQL()
			if sql == "" {
				t.Fatalf("no SQL captured for %s", path)
			}
			lowerBound := "`Timestamp` >= toDateTime64('" + wantStart
			upperBound := "`Timestamp` <= toDateTime64('" + wantEnd
			if !strings.Contains(sql, lowerBound) {
				t.Errorf("SQL missing start clamp %q:\n%s", lowerBound, sql)
			}
			if !strings.Contains(sql, upperBound) {
				t.Errorf("SQL missing end clamp %q:\n%s", upperBound, sql)
			}
		})
	}
}
