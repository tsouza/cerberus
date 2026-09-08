package routerrules

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sinceCorpusLine renders one JSONL corpus line. eventTime < 0 omits the
// event_time field entirely, modelling a corpus written before the sink stamped
// one.
func sinceCorpusLine(shape string, eventTime, memory int64) string {
	if eventTime < 0 {
		return fmt.Sprintf(
			`{"shape_id":%q,"language":"promql","route":"A","exit_status":"ok","memory_usage":%d}`,
			shape, memory,
		)
	}
	return fmt.Sprintf(
		`{"event_time":%d,"shape_id":%q,"language":"promql","route":"A","exit_status":"ok","memory_usage":%d}`,
		eventTime, shape, memory,
	)
}

func writeSinceCorpus(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write corpus: %v", err)
	}
	return path
}

// maxOf aggregates memory_usage over whatever rows the source admitted, so the
// assertion is on the POPULATION the window selected rather than on a row count
// the source does not expose.
func maxOf(t *testing.T, src CorpusSource) float64 {
	t.Helper()
	v, err := src.Aggregate(context.Background(), AggSpec{Column: "memory_usage", Agg: AggMax})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	return v.Scalar
}

// TestJSONLSinceWindowsOnEventTime pins that --since actually windows the JSONL
// corpus. It used to be a silent no-op: the sink wrote no event_time, jsonlRow
// decoded the absent field to 0, and the filter's `jr.EventTime > 0` guard then
// admitted every row. An operator passing --since 720h got findings over the
// whole file history while believing the window applied — and the ClickHouse
// backend, which windows in SQL on the table's own event_time, answered the same
// flag over a different population.
//
// The old row is the one with the LARGER memory_usage, so a source that ignores
// the window reports the old row's value and fails. A source that applied the
// window reports the recent row's.
func TestJSONLSinceWindowsOnEventTime(t *testing.T) {
	t.Parallel()

	const (
		oldEventTime    = 1_000
		recentEventTime = 9_000
		windowFloor     = 5_000
		oldMemory       = 900
		recentMemory    = 100
	)
	path := writeSinceCorpus(
		t,
		sinceCorpusLine("prom:old", oldEventTime, oldMemory),
		sinceCorpusLine("prom:recent", recentEventTime, recentMemory),
	)

	if got := maxOf(t, NewJSONLCorpusSource(path, windowFloor)); got != recentMemory {
		t.Errorf("max(memory_usage) with --since = %v, want %v; the pre-window row leaked into the "+
			"result, so --since did not window the corpus", got, float64(recentMemory))
	}

	// The complement: with no window, the old row MUST be present. Without this
	// half, a source that dropped every row would pass the assertion above.
	if got := maxOf(t, NewJSONLCorpusSource(path, 0)); got != oldMemory {
		t.Errorf("max(memory_usage) with no --since = %v, want %v; the unwindowed source must see "+
			"the whole file", got, float64(oldMemory))
	}
}

// TestJSONLSinceRefusesUndatableRow pins that a corpus line with no event_time
// is REFUSED under --since rather than admitted. Such a row cannot be placed
// inside or outside the window; admitting it is precisely what made the flag a
// silent no-op, and silently dropping it would discard data just as quietly. It
// is a corpus written before event-time stamping, and the operator is told so.
func TestJSONLSinceRefusesUndatableRow(t *testing.T) {
	t.Parallel()

	const (
		datedEventTime = 9_000
		windowFloor    = 5_000
	)
	path := writeSinceCorpus(
		t,
		sinceCorpusLine("prom:dated", datedEventTime, 100),
		sinceCorpusLine("prom:undated", -1, 900),
	)

	_, err := NewJSONLCorpusSource(path, windowFloor).
		Aggregate(context.Background(), AggSpec{Column: "memory_usage", Agg: AggMax})
	if err == nil {
		t.Fatal("aggregating an undatable row under --since succeeded; the row can be placed neither " +
			"inside nor outside the window, so admitting it silently reinstates the no-op")
	}
	if !strings.Contains(err.Error(), "event_time") {
		t.Errorf("error %q does not name event_time; the operator cannot act on it", err)
	}
	// The position must be locatable: a multi-file corpus directory otherwise
	// gives the operator no way to find the offending line.
	if !strings.Contains(err.Error(), ":2") {
		t.Errorf("error %q does not carry the offending line number", err)
	}

	// Without --since the same corpus is answerable: nothing is being windowed,
	// so a missing event_time costs nothing and must not be an error.
	if got := maxOf(t, NewJSONLCorpusSource(path, 0)); got != 900 {
		t.Errorf("max(memory_usage) with no --since = %v, want 900; an undated row is only a problem "+
			"when a window is being applied", got)
	}
}
