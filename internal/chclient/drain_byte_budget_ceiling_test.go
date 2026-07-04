package chclient

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestDrainByteBudget_CeilingHeadroom is the corpus-floor validation: it measures
// the ACTUAL charge (via Peak) for realistic search and trace-by-id workloads
// through the production rowsCursor, grounding the 256 MiB ceiling. Search stays
// KB-scale (resource attrs intern/share); a large 10k-distinct-attr-span trace
// stays comfortably under the ceiling. So the ceiling 422s only genuinely
// OOM-scale traces (100k+ fat spans) — never a servable query.
func TestDrainByteBudget_CeilingHeadroom(t *testing.T) {
	t.Parallel()
	measure := func(rows []Sample) int64 {
		b := NewDrainByteBudget(1 << 40) // effectively unlimited: measure, don't trip
		cur := &rowsCursor{rows: &freshLabelRows{rows: rows}, byteBudget: b}
		for cur.Next() {
		}
		if err := cur.Err(); err != nil {
			t.Fatalf("measure drain: %v", err)
		}
		return b.Peak()
	}
	const ceiling = maxTempoSpanDrainBytes

	// SEARCH scale: 1000 result traces but only ~20 distinct services — resource
	// attrs are shared and interned, so unique-series count is the service count,
	// not the trace count.
	resAttrs := func(svc int) map[string]string {
		return map[string]string{
			"service.name": fmt.Sprintf("svc-%d", svc), "service.namespace": "prod",
			"k8s.pod.name": fmt.Sprintf("pod-%d-7f9c8b", svc), "k8s.namespace.name": "default",
			"host.name": fmt.Sprintf("node-%d.internal", svc), "telemetry.sdk.language": "go",
			"telemetry.sdk.name": "opentelemetry", "deployment.environment": "production",
		}
	}
	searchRows := make([]Sample, 1000)
	for i := range searchRows {
		searchRows[i] = Sample{MetricName: "s", Labels: resAttrs(i % 20), Timestamp: time.Unix(int64(i), 0), Value: 1}
	}
	searchPeak := measure(searchRows)
	t.Logf("SEARCH  (1000 traces / 20 services): peak=%d B (%.4f MiB) — ceiling %d MiB, margin %.0fx",
		searchPeak, float64(searchPeak)/(1<<20), ceiling>>20, float64(ceiling)/float64(searchPeak))
	if searchPeak > ceiling/100 {
		t.Errorf("search peak %d B exceeds 1%% of ceiling — expected a huge margin", searchPeak)
	}

	// TRACE-BY-ID scale: one large trace, 10k spans each with a DISTINCT ~450 B
	// span-attr map (http/db attrs, not interned).
	spanRows := make([]Sample, 10000)
	for i := range spanRows {
		spanRows[i] = Sample{MetricName: "s", Timestamp: time.Unix(int64(i), 0), Value: 1, Labels: map[string]string{
			"span.kind": "server", "http.method": "GET", "http.status_code": "200",
			"http.route":   fmt.Sprintf("/api/v1/resource/%d/detail", i),
			"db.statement": fmt.Sprintf("SELECT * FROM t WHERE id=%d AND %s", i, strings.Repeat("x", 380)),
		}}
	}
	tracePeak := measure(spanRows)
	t.Logf("TRACE   (10k distinct-attr spans):   peak=%d B (%.1f MiB) — ceiling %d MiB, margin %.1fx",
		tracePeak, float64(tracePeak)/(1<<20), ceiling>>20, float64(ceiling)/float64(tracePeak))
	if tracePeak > ceiling {
		t.Errorf("realistic 10k-span trace peak %d B exceeds the ceiling — a legitimate trace would be 422'd", tracePeak)
	}
}
