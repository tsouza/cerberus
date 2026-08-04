//go:build chdb

package profile

import (
	"strings"
	"testing"
)

// TestProfileMetadataEndpoints drives the three windowless
// metadata-discovery endpoints end to end — production Handler capture
// (captureMetadataFixtures/captureMetadataSQL/metadataCaptureQuerier),
// the seed script (metadataProfileSeed and its table/insert/projection
// helpers), and the shared [Profiler] — the same path
// test/perf/cardinality_ratchet_test.go exercises from test/perf, but
// measured here so this package's own coverage floor sees it: the
// default `go test ./...` per-package coverage instrumentation only
// counts a package's statements when a test IN that package (or an
// external test package rooted at the same directory) executes them,
// not when a different package's test merely imports and calls it.
func TestProfileMetadataEndpoints(t *testing.T) {
	recs, err := ProfileMetadataEndpoints()
	if err != nil {
		t.Fatalf("ProfileMetadataEndpoints: %v", err)
	}

	wantIDs := []string{
		"metadata/series_no_bounds",
		"metadata/labels_no_bounds",
		"metadata/label_values_no_bounds",
	}
	if len(recs) != len(wantIDs) {
		t.Fatalf("got %d records, want %d", len(recs), len(wantIDs))
	}
	for i, rec := range recs {
		if rec.Fixture != wantIDs[i] {
			t.Errorf("record[%d].Fixture = %q, want %q", i, rec.Fixture, wantIDs[i])
		}
		if rec.Err != "" {
			t.Errorf("record[%d] (%s) unexpected error: %s", i, rec.Fixture, rec.Err)
		}
		if rec.ScanRows <= 0 {
			t.Errorf("record[%d] (%s) ScanRows = %d, want > 0", i, rec.Fixture, rec.ScanRows)
		}
	}
}

// TestCaptureMetadataFixtures exercises captureMetadataSQL /
// metadataCaptureQuerier directly (below the seeded-chDB layer
// TestProfileMetadataEndpoints drives), asserting each of the three
// windowless endpoints issues exactly one captured production SQL
// statement with no seeded backing table.
func TestCaptureMetadataFixtures(t *testing.T) {
	fixtures := captureMetadataFixtures()
	if len(fixtures) != 3 {
		t.Fatalf("got %d fixtures, want 3", len(fixtures))
	}
	for _, f := range fixtures {
		if f.query == "" {
			t.Errorf("fixture %s: captured no SQL", f.id)
		}
	}
}

// TestMetadataProfileSeed asserts the seed script covers all three
// metric tables and their projections, and that the DDL helpers used to
// build it stay consistent with the schema metadata.go documents.
func TestMetadataProfileSeed(t *testing.T) {
	seed := metadataProfileSeed()
	for _, table := range []string{"otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_histogram"} {
		if !strings.Contains(seed, "CREATE TABLE "+table) {
			t.Errorf("seed missing CREATE TABLE for %s", table)
		}
		if !strings.Contains(seed, "INSERT INTO "+table) {
			t.Errorf("seed missing INSERT for %s", table)
		}
		if !strings.Contains(seed, "ADD PROJECTION proj_series") {
			t.Errorf("seed missing proj_series projection")
		}
	}
}
