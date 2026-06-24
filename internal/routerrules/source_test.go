package routerrules

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestJSONLPercentileScoped(t *testing.T) {
	src := NewJSONLCorpusSource("testdata/seed.jsonl", 0)
	frac := 0.5
	// Median memory of the HEALTHY route-A rows (exit_status=ok): the
	// cerb:sum/promql class has ok memory_usage {90, 80, 70} -> median 80.
	v, err := src.Aggregate(context.Background(), AggSpec{
		Column:     "memory_usage",
		Percentile: &frac,
		Scope:      Scope{"route": "A", "exit_status": "ok"},
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if v.Scalar != 80 {
		t.Fatalf("scoped median memory = %v, want 80", v.Scalar)
	}
}

func TestJSONLPercentilePartitioned(t *testing.T) {
	src := NewJSONLCorpusSource("testdata/seed.jsonl", 0)
	frac := 0.5
	v, err := src.Aggregate(context.Background(), AggSpec{
		Column:      "memory_usage",
		Percentile:  &frac,
		PartitionBy: []string{"language"},
		Scope:       Scope{"route": "A", "exit_status": "ok"},
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if !v.IsPartitioned() {
		t.Fatalf("expected partitioned value")
	}
	if v.Partition["promql"] != 80 {
		t.Fatalf("promql median = %v, want 80", v.Partition["promql"])
	}
}

func TestJSONLAggMax(t *testing.T) {
	src := NewJSONLCorpusSource("testdata/seed.jsonl", 0)
	v, err := src.Aggregate(context.Background(), AggSpec{
		Column: "memory_usage",
		Agg:    AggMax,
		Scope:  Scope{"route": "A"},
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if v.Scalar != 550 { // logql sample_budget row has memory 550, the route-A max
		t.Fatalf("max memory = %v, want 550", v.Scalar)
	}
}

func TestJSONLCountRatio(t *testing.T) {
	src := NewJSONLCorpusSource("testdata/seed.jsonl", 0)
	v, err := src.Aggregate(context.Background(), AggSpec{
		CountRatio: true,
		NumScope:   Scope{"exit_status": "oom"},
		DenScope:   Scope{"route": "A"},
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	// 4 oom rows (any route) over 13 route-A rows in the seed.
	want := 4.0 / 13.0
	if v.Scalar < want-1e-9 || v.Scalar > want+1e-9 {
		t.Fatalf("oom ratio = %v, want %v", v.Scalar, want)
	}
}

func TestJSONLSinceWindowDropsOldRows(t *testing.T) {
	// Window after every seed row's event_time -> no rows -> count ratio den=0.
	src := NewJSONLCorpusSource("testdata/seed.jsonl", 1_000_000)
	v, err := src.Aggregate(context.Background(), AggSpec{
		Agg:    AggMax,
		Column: "memory_usage",
		Scope:  Scope{"route": "A"},
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if v.Scalar != 0 {
		t.Fatalf("windowed-out aggregate = %v, want 0", v.Scalar)
	}
}

func TestJSONLDirectoryCorpus(t *testing.T) {
	// A directory of JSONL shards is read in sorted order and concatenated.
	dir := t.TempDir()
	a := `{"event_time":1,"shape_id":"s","language":"promql","route":"A","exit_status":"oom","memory_usage":10}` + "\n"
	b := `{"event_time":2,"shape_id":"s","language":"promql","route":"A","exit_status":"oom","memory_usage":20}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte(a), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.jsonl"), []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
	src := NewJSONLCorpusSource(dir, 0)
	v, err := src.Aggregate(context.Background(), AggSpec{Agg: AggMax, Column: "memory_usage", Scope: Scope{"route": "A"}})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if v.Scalar != 20 {
		t.Fatalf("directory max = %v, want 20", v.Scalar)
	}
}
