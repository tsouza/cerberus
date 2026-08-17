package chclient

import (
	"reflect"
	"testing"
	"time"
)

// cursor_logrow_test.go — issue #1430.
//
// A log stream has no numeric value. Before #1430 the Loki log-stream
// projection still emitted a `toFloat64(0) AS Value` column so the row was as
// wide as the cursor's fixed positional scan. These tests pin the decode side
// of its removal: the cursor recognises the two log-row layouts from their
// column names and binds a scan with NO numeric destination, while every
// sample-shaped layout keeps binding exactly the destinations it always did.

// logRowProjectionColumns / logRowMetadataProjectionColumns are the two
// column layouts a Loki log-stream query publishes: the bare row, and the row
// plus the trailing structured-metadata column a schema with an
// AttributesColumn adds. They mirror the aliases internal/logql/lang.go
// emits — chclient may not import logql (.go-arch-lint.yml), so the names are
// duplicated by value here exactly as histogramProjectionColumns duplicates
// chplan's.
var (
	logRowProjectionColumns         = []string{"Line", "Attributes", "TimeUnix"}
	logRowMetadataProjectionColumns = []string{"Line", "Attributes", "TimeUnix", "Metadata"}
)

// sampleProjectionColumns is the canonical four-column metric row every
// PromQL / TraceQL query and every LogQL metric query publishes.
var sampleProjectionColumns = []string{"MetricName", "Attributes", "TimeUnix", "Value"}

// sampleMetadataProjectionColumns is sampleProjectionColumns plus a trailing
// Metadata column — a layout NO head emits: structured metadata is a
// log-stream concept, so it rides on a log row or not at all. It appears here
// only as a negative case, pinning that the probe does not invent a decode
// for it.
var sampleMetadataProjectionColumns = []string{"MetricName", "Attributes", "TimeUnix", "Value", "Metadata"}

// TestProbeRowShape pins the whole classification table, including the two
// collisions that make the log-row layouts non-obvious to recognise:
//
//   - (Line, Attributes, TimeUnix, Metadata) is exactly as WIDE as the
//     four-column sample layout, so width alone cannot tell them apart;
//   - a wider projection that merely leads with `Line` is not a log row, so
//     the leading alias alone cannot either.
//
// Both signals together are what the probe requires.
func TestProbeRowShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cols []string
		want rowShape
	}{
		{"bare log row", logRowProjectionColumns, shapeLogRow},
		{"log row with structured metadata", logRowMetadataProjectionColumns, shapeLogRowMetadata},
		{"sample row", sampleProjectionColumns, shapeSample},
		{"histogram row", histogramProjectionColumns, shapeSampleHistogram},
		{"mixed row", mixedProjectionColumns, shapeSampleMixed},
		// One column short of the mixed width but ending in the mixed
		// discriminator's alias: not the trailing alias the histogram
		// probe keys on, and one short of mixedColumns, so this must
		// fall back to the default rather than being mistaken for
		// either sample-shaped layout.
		{
			"thirteen wide ending in the mixed discriminator alias",
			append(append([]string{}, sampleProjectionColumns...), "_setop_is_histogram"),
			shapeSample,
		},
		// No columns at all: nothing to key off, so the default
		// four-destination scan — the behaviour every path had before any
		// probe existed. fakeRows leaves Columns() nil in the pre-existing
		// sample-shaped tests, which is exactly this case.
		{"no columns reported", nil, shapeSample},
		// Leads with `Line` but is not three or four wide: not a log row.
		// Falls through to the sample-shaped tests, which do not match
		// either, so it decodes as the default.
		{
			"wider projection leading with Line",
			[]string{"Line", "Attributes", "TimeUnix", "Value", "Extra"},
			shapeSample,
		},
		// Four wide and ends in Metadata, but does NOT lead with `Line`.
		// This is no layout any head emits: the real sample+metadata shape
		// is FIVE wide (MetricName, Attributes, TimeUnix, Value, Metadata).
		// Classifying it as shapeSampleMetadata on the trailing name alone
		// would select a five-destination scan against a four-column
		// result set, so the probe pairs the trailing alias with the width
		// the scan actually binds and lets this fall back to the default.
		{
			"four wide ending in Metadata but leading with MetricName",
			[]string{"MetricName", "Attributes", "TimeUnix", "Metadata"},
			shapeSample,
		},
		// A sample row with a trailing Metadata column: no head emits this,
		// because structured metadata is a log-stream concept. The probe
		// must not invent a five-destination decode for it — it falls back
		// to the default, and a result set that really did arrive in this
		// shape would fail loudly on scan arity rather than silently
		// mis-binding.
		{
			"five wide sample layout with metadata",
			sampleMetadataProjectionColumns,
			shapeSample,
		},
		// Three wide but leading with something else: not a log row.
		{"three wide not leading with Line", []string{"MetricName", "Attributes", "TimeUnix"}, shapeSample},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := probeRowShape(c.cols); got != c.want {
				t.Errorf("probeRowShape(%v) = %d, want %d", c.cols, got, c.want)
			}
		})
	}
}

// TestRowsCursor_DecodesLogRowWithoutValueColumn pins the three-column
// log-stream decode end to end: the line, the stream labels and the timestamp
// all round-trip, no numeric destination is bound (fakeRows.Scan errors on a
// destination count it does not recognise, so a four-destination scan against
// this three-column result set fails the test rather than silently reading a
// float), and Sample.Value stays zero.
func TestRowsCursor_DecodesLogRowWithoutValueColumn(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 8, 9, 10, 30, 0, 0, time.UTC)
	labels := map[string]string{"service_name": "api", "detected_level": "error"}
	rows := &fakeRows{
		columns: logRowProjectionColumns,
		samples: []Sample{{
			// The line rides in the positional row's first, String-typed
			// slot — the one a metric query fills with the metric name.
			MetricName: `level=error msg="upstream timeout" duration=1.2s`,
			Labels:     labels,
			Timestamp:  ts,
		}},
	}
	cursor := &rowsCursor{rows: rows}

	if !cursor.Next() {
		t.Fatalf("Next: want true, cursor.Err() = %v", cursor.Err())
	}
	got := cursor.Sample()
	if err := cursor.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if got.MetricName != `level=error msg="upstream timeout" duration=1.2s` {
		t.Errorf("line: got %q, want the projected log body", got.MetricName)
	}
	if !reflect.DeepEqual(got.Labels, labels) {
		t.Errorf("labels: got %v, want %v", got.Labels, labels)
	}
	if !got.Timestamp.Equal(ts) {
		t.Errorf("timestamp: got %v, want %v", got.Timestamp, ts)
	}
	if got.Value != 0 {
		t.Errorf("Value: got %v, want 0 — the log-row scan binds no numeric "+
			"destination, so this field is never written (issue #1430)", got.Value)
	}
	if got.Metadata != nil {
		t.Errorf("Metadata: got %v, want nil on the bare three-column layout", got.Metadata)
	}
	if cursor.Next() {
		t.Fatal("Next should return false after the single row")
	}
}

// TestRowsCursor_DecodesLogRowWithMetadata pins the four-column sibling: the
// trailing Metadata column decodes into Sample.Metadata, and — the part that
// makes this layout worth its own test — the fourth destination is the
// metadata String, NOT the numeric Value. This is the layout that collides in
// width with the sample shape, so getting it wrong would bind a *float64
// where a JSON String arrives.
func TestRowsCursor_DecodesLogRowWithMetadata(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 8, 9, 10, 31, 0, 0, time.UTC)
	labels := map[string]string{"service_name": "api"}
	metadata := map[string]string{"duration": "1.2s", "query_id": "abc-123"}
	rows := &fakeRows{
		columns: logRowMetadataProjectionColumns,
		samples: []Sample{{
			MetricName: "request completed",
			Labels:     labels,
			Timestamp:  ts,
			Metadata:   metadata,
		}},
	}
	cursor := &rowsCursor{rows: rows}

	if !cursor.Next() {
		t.Fatalf("Next: want true, cursor.Err() = %v", cursor.Err())
	}
	got := cursor.Sample()
	if err := cursor.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if got.MetricName != "request completed" {
		t.Errorf("line: got %q, want %q", got.MetricName, "request completed")
	}
	if !reflect.DeepEqual(got.Labels, labels) {
		t.Errorf("labels: got %v, want %v", got.Labels, labels)
	}
	if !got.Timestamp.Equal(ts) {
		t.Errorf("timestamp: got %v, want %v", got.Timestamp, ts)
	}
	if !reflect.DeepEqual(got.Metadata, metadata) {
		t.Errorf("metadata: got %v, want %v", got.Metadata, metadata)
	}
	if got.Value != 0 {
		t.Errorf("Value: got %v, want 0 — the log-row scan binds no numeric "+
			"destination even on the four-wide layout (issue #1430)", got.Value)
	}
}

// TestRowsCursor_LogRowDecodeRoundTripsToLogRow pins the seam this whole
// change exists to keep honest: the positional Sample the cursor produces for
// a log-stream result set decodes into the named [LogRow] fields, and LogRow
// carries no numeric field for the removed column to reappear in.
func TestRowsCursor_LogRowDecodeRoundTripsToLogRow(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 8, 9, 10, 32, 0, 0, time.UTC)
	rows := &fakeRows{
		columns: logRowMetadataProjectionColumns,
		samples: []Sample{
			{
				MetricName: "first line",
				Labels:     map[string]string{"service_name": "api"},
				Timestamp:  ts,
				Metadata:   map[string]string{"bytes": "512"},
			},
			{
				MetricName: "second line",
				Labels:     map[string]string{"service_name": "api"},
				Timestamp:  ts.Add(time.Second),
			},
		},
	}
	cursor := &rowsCursor{rows: rows}

	var drained []Sample
	for cursor.Next() {
		drained = append(drained, cursor.Sample())
	}
	if err := cursor.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}

	logRows := DecodeLogRows(drained)
	if len(logRows) != 2 {
		t.Fatalf("DecodeLogRows returned %d rows, want 2", len(logRows))
	}
	if logRows[0].Line != "first line" || logRows[1].Line != "second line" {
		t.Errorf("lines: got %q / %q, want %q / %q",
			logRows[0].Line, logRows[1].Line, "first line", "second line")
	}
	if !reflect.DeepEqual(logRows[0].Metadata, map[string]string{"bytes": "512"}) {
		t.Errorf("row 0 metadata: got %v, want map[bytes:512]", logRows[0].Metadata)
	}
	if logRows[1].Metadata != nil {
		t.Errorf("row 1 metadata: got %v, want nil (empty metadata map)", logRows[1].Metadata)
	}
	// Both rows share one interned label map — the log path keeps the same
	// per-series interning the sample path has, so a long log drain retains
	// one map per stream rather than one per line.
	if logRows[0].Labels == nil || !reflect.DeepEqual(logRows[0].Labels, logRows[1].Labels) {
		t.Errorf("labels: got %v / %v, want both rows to carry the same stream identity",
			logRows[0].Labels, logRows[1].Labels)
	}
	if drained[0].SeriesID != drained[1].SeriesID {
		t.Errorf("SeriesID: got %d / %d, want both rows interned to one series",
			drained[0].SeriesID, drained[1].SeriesID)
	}
}

// TestRowsCursor_SampleShapesBindTheValueColumn is the control: the layouts
// that DO carry a numeric value must still bind it. It is the guard against
// the log-row branch widening to swallow a shape it does not own — the exact
// failure mode that would silently zero every metric result.
func TestRowsCursor_SampleShapesBindTheValueColumn(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 8, 9, 10, 33, 0, 0, time.UTC)
	cases := []struct {
		name    string
		columns []string
		want    float64
	}{
		{"four-column sample", sampleProjectionColumns, 42.5},
		// Columns() unreported — the shape every pre-probe path decoded as.
		{"no columns reported", nil, 3.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			rows := &fakeRows{
				columns: c.columns,
				samples: []Sample{{
					MetricName: "up",
					Labels:     map[string]string{"job": "api"},
					Timestamp:  ts,
					Value:      c.want,
				}},
			}
			cursor := &rowsCursor{rows: rows}

			if !cursor.Next() {
				t.Fatalf("Next: want true, cursor.Err() = %v", cursor.Err())
			}
			got := cursor.Sample()
			if err := cursor.Err(); err != nil {
				t.Fatalf("Err: %v", err)
			}
			if got.Value != c.want {
				t.Errorf("Value: got %v, want %v — the sample-shaped layouts must keep "+
					"binding their numeric column", got.Value, c.want)
			}
			if got.MetricName != "up" {
				t.Errorf("MetricName: got %q, want %q", got.MetricName, "up")
			}
		})
	}
}
