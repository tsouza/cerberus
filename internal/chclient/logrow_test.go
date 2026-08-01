package chclient

import (
	"testing"
	"time"
)

// TestDecodeLogRows_FieldMapping pins every field of the positional →
// named mapping with a DISTINCT value, so any cross-wiring fails: the line
// is not a label value, the label map is not the metadata map, and the
// timestamp is not the zero value. A decoder that dropped or swapped a
// field would still round-trip if the fixture reused one value everywhere,
// which is why each slot carries its own.
func TestDecodeLogRows_FieldMapping(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 8, 1, 10, 30, 15, 123456789, time.UTC)
	labels := map[string]string{"job": "api", "namespace": "prod"}
	metadata := map[string]string{"trace_id": "abc123", "duration": "42ms"}

	rows := DecodeLogRows([]Sample{{
		MetricName: "GET /healthz 200",
		Labels:     labels,
		Timestamp:  ts,
		// Value is the positional row's Float64 placeholder on the
		// log-stream shape: LogRow has no slot for it by construction, so a
		// decoder that tried to surface it would not compile.
		Value:    0,
		SeriesID: 7,
		Metadata: metadata,
	}})

	if len(rows) != 1 {
		t.Fatalf("DecodeLogRows returned %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.Line != "GET /healthz 200" {
		t.Errorf("Line: got %q, want %q (the positional row's first String column)",
			got.Line, "GET /healthz 200")
	}
	if !got.Timestamp.Equal(ts) {
		t.Errorf("Timestamp: got %v, want %v", got.Timestamp, ts)
	}
	if len(got.Labels) != len(labels) {
		t.Fatalf("Labels: got %v, want %v (the stream identity, not the metadata map)", got.Labels, labels)
	}
	for k, want := range labels {
		if got.Labels[k] != want {
			t.Errorf("Labels[%q]: got %q, want %q", k, got.Labels[k], want)
		}
	}
	if len(got.Metadata) != len(metadata) {
		t.Fatalf("Metadata: got %v, want %v (per-line structured metadata, not the stream labels)",
			got.Metadata, metadata)
	}
	for k, want := range metadata {
		if got.Metadata[k] != want {
			t.Errorf("Metadata[%q]: got %q, want %q", k, got.Metadata[k], want)
		}
	}
}

// TestDecodeLogRows_PreservesOrderAndPerRowFields pins that the decode is a
// positional 1:1 walk: row i in equals row i out, each carrying its OWN line
// and metadata. A decoder that hoisted a per-row field to the first row's
// value (the shape a mis-scoped loop variable produces) fails here.
func TestDecodeLogRows_PreservesOrderAndPerRowFields(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	in := []Sample{
		{MetricName: "first", Timestamp: base, Metadata: map[string]string{"seq": "1"}},
		{MetricName: "second", Timestamp: base.Add(time.Second), Metadata: map[string]string{"seq": "2"}},
		{MetricName: "third", Timestamp: base.Add(2 * time.Second), Metadata: map[string]string{"seq": "3"}},
	}
	wantLines := []string{"first", "second", "third"}
	wantSeq := []string{"1", "2", "3"}

	rows := DecodeLogRows(in)
	if len(rows) != len(in) {
		t.Fatalf("DecodeLogRows returned %d rows, want %d", len(rows), len(in))
	}
	for i, row := range rows {
		if row.Line != wantLines[i] {
			t.Errorf("row %d Line: got %q, want %q", i, row.Line, wantLines[i])
		}
		if row.Metadata["seq"] != wantSeq[i] {
			t.Errorf("row %d Metadata[seq]: got %q, want %q", i, row.Metadata["seq"], wantSeq[i])
		}
		wantTS := base.Add(time.Duration(i) * time.Second)
		if !row.Timestamp.Equal(wantTS) {
			t.Errorf("row %d Timestamp: got %v, want %v", i, row.Timestamp, wantTS)
		}
	}
}

// TestDecodeLogRows_EmptyInput pins the nil contract: an empty result set
// decodes to nil rather than an empty non-nil slice, so a caller ranging
// over the result allocates nothing.
func TestDecodeLogRows_EmptyInput(t *testing.T) {
	t.Parallel()

	if rows := DecodeLogRows(nil); rows != nil {
		t.Errorf("DecodeLogRows(nil): got %v, want nil", rows)
	}
	if rows := DecodeLogRows([]Sample{}); rows != nil {
		t.Errorf("DecodeLogRows(empty): got %v, want nil", rows)
	}
}

// TestDecodeLogRows_SharesInternedLabels pins the read-only aliasing
// contract: the cursor interns one label map per series, and the decode
// carries that instance through rather than copying it. Copying would
// silently multiply the retained heap by the row count — the exact
// regression the interning exists to prevent.
func TestDecodeLogRows_SharesInternedLabels(t *testing.T) {
	t.Parallel()

	shared := map[string]string{"job": "api"}
	rows := DecodeLogRows([]Sample{
		{MetricName: "a", Labels: shared},
		{MetricName: "b", Labels: shared},
	})
	if len(rows) != 2 {
		t.Fatalf("DecodeLogRows returned %d rows, want 2", len(rows))
	}
	shared["job"] = "mutated"
	if rows[0].Labels["job"] != "mutated" || rows[1].Labels["job"] != "mutated" {
		t.Errorf("Labels were copied per row (got %q / %q); the decode must carry the "+
			"cursor's interned map instance through so a series retains one map, not one per row",
			rows[0].Labels["job"], rows[1].Labels["job"])
	}
}
