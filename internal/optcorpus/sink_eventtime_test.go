package optcorpus

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestJSONLSink_StampsEventTime pins that every JSONL corpus line carries the
// write-time event_time, at the same instant and from the same clock that
// CHTableSink.Write binds to the table's event_time column.
//
// It exists because the JSONL sink used to write no event_time at all. Nothing
// noticed, because Row has no such field and the round-trip tests only compare
// Row back to Row — but a corpus reader windowing on event_time (routerrules'
// --since) then had nothing to window on, so the flag silently degraded to a
// full-history scan on the DEFAULT source while the ClickHouse backend honoured
// it. The two backends answered the same flag over different populations.
func TestJSONLSink_StampsEventTime(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "corpus.jsonl")
	sink, err := NewJSONLSink(path)
	if err != nil {
		t.Fatalf("NewJSONLSink: %v", err)
	}
	// A fixed clock, so the assertion is on the stamped value itself rather
	// than on a tolerance window around wall-clock time.
	stamp := time.Unix(1_700_000_000, 0).UTC()
	sink.now = func() time.Time { return stamp }

	rows := []Row{
		{ShapeID: "prom:a", Language: "promql", NormalizedQueryHash: 1},
		{ShapeID: "prom:b", Language: "promql", NormalizedQueryHash: 2},
	}
	if err := sink.Write(rows); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		// Decode into a map, not a struct: a struct with an EventTime field
		// would deserialize a MISSING event_time to the zero value and pass,
		// which is exactly the failure this test exists to catch.
		var got map[string]any
		if err := json.Unmarshal(line, &got); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", n+1, err)
		}
		raw, ok := got["event_time"]
		if !ok {
			t.Fatalf("line %d has no event_time field; a corpus line with no event time cannot be "+
				"windowed by a reader, which is what made --since a silent no-op", n+1)
		}
		secs, ok := raw.(float64)
		if !ok {
			t.Fatalf("line %d event_time is %T, want a unix-seconds number", n+1, raw)
		}
		if int64(secs) != stamp.Unix() {
			t.Errorf("line %d event_time = %d, want %d (the write-time clock)", n+1, int64(secs), stamp.Unix())
		}
		// The Row columns must still be flattened into the same object, or the
		// JSONL form has stopped being column-for-column comparable with the table.
		if got["shape_id"] != rows[n].ShapeID {
			t.Errorf("line %d shape_id = %v, want %q", n+1, got["shape_id"], rows[n].ShapeID)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != len(rows) {
		t.Fatalf("read %d lines, wrote %d rows", n, len(rows))
	}
}
