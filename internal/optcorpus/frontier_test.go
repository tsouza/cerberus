package optcorpus

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNewCHFrontierSource_Fallbacks(t *testing.T) {
	// Non-positive timeout / window fall back to the conservative defaults so
	// the self-tune read can never block indefinitely or scan unbounded.
	src := NewCHFrontierSource(&capturingConn{}, 0, 0)
	if src.window != defaultQueryLogWindow {
		t.Errorf("non-positive window = %s; want default %s", src.window, defaultQueryLogWindow)
	}
	if src.timeout <= 0 {
		t.Errorf("non-positive timeout must fall back to a positive default, got %s", src.timeout)
	}
}

func TestCHFrontierSource_PassesWindowSeconds(t *testing.T) {
	conn := &capturingConn{}
	// 2h window -> 7200 seconds is the single bound arg (no value is ever
	// concatenated into the SQL).
	src := NewCHFrontierSource(conn, time.Second, 2*time.Hour)
	_, err := src.ReadFrontier(context.Background())
	if err == nil {
		t.Fatal("ReadFrontier: want stub error, got nil")
	}
	if len(conn.gotArgs) != 1 {
		t.Fatalf("query args = %d; want 1 (window seconds)", len(conn.gotArgs))
	}
	if got, ok := conn.gotArgs[0].(int64); !ok || got != 7200 {
		t.Errorf("first arg = %v; want int64(7200) window seconds", conn.gotArgs[0])
	}
}

// TestFrontierAggSQL_ShapeContract pins the aggregate SQL's structural
// contract: it reads from the corpus table, is bounded by a parameterised
// event_time window (never an inlined value), groups by the cost-grid
// coordinates, and classifies the danger set as the three OOM/cost outcomes
// route B exists to avoid. This is a cheap regression guard on the read shape.
func TestFrontierAggSQL_ShapeContract(t *testing.T) {
	sql := frontierAggSQL
	mustContain := []string{
		"FROM " + CorpusTableName,
		"event_time > now() - INTERVAL ? SECOND",
		"GROUP BY",
		"n_anchors",
		"fanout",
		"cumulative_d",
		"decision_reason = 'below-threshold'",
		"'oom', 'timeout', 'sample_budget'",
		"max(memory_usage)",
		"count()",
	}
	for _, frag := range mustContain {
		if !strings.Contains(sql, frag) {
			t.Errorf("frontierAggSQL missing %q", frag)
		}
	}
	// No GROUP BY on a non-aggregated cost column would be a logic bug; assert
	// the aggregates are real.
	if !strings.Contains(sql, "AS row_count") {
		t.Error("frontierAggSQL must alias the count() as row_count")
	}
}
