//go:build chdb

package spec

import (
	"strings"
	"testing"

	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

// TestReadSeededSeries_FailingReferenceQueryIsAnError pins that a seed
// reader whose reference query fails over a table the fixture created
// returns that failure instead of an empty reference. Each case seeds one
// metrics table without the TimeUnix column every reader selects, so the
// table exists and its reference query cannot run.
func TestReadSeededSeries_FailingReferenceQueryIsAnError(t *testing.T) {
	for _, tc := range []struct {
		table, columns, names, values string
	}{
		{seededMetricsTables[0], "Value Float64", "Value", "1.0"},
		{seededMetricsTables[1], "Value Float64", "Value", "1.0"},
		{
			classicHistogramTable,
			"Count UInt64, Sum Float64, BucketCounts Array(UInt64), ExplicitBounds Array(Float64)",
			"Count, Sum, BucketCounts, ExplicitBounds", "1, 1.0, [1, 0], [1.0]",
		},
		{nativeHistogramTable, "Count UInt64, Sum Float64", "Count, Sum", "1, 1.0"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			chdbEngineMu.Lock()
			defer chdbEngineMu.Unlock()

			seed := "CREATE TABLE " + tc.table + " (MetricName String, Attributes Map(String, String), " +
				tc.columns + ") ENGINE = Memory;\n" +
				"INSERT INTO " + tc.table + " (MetricName, Attributes, " + tc.names + ") VALUES " +
				"('m', map('job', 'a'), " + tc.values + ");"
			db := OpenChDB(t)
			ApplySeed(t, db, seed)

			_, err := readSeededSeries(db, &RoundTripSections{Seed: seed}, nil, "m")
			if err == nil {
				t.Fatal("readSeededSeries over a table whose reference query fails returned no error")
			}
			if want := "reference query over seeded table " + tc.table + " failed"; !strings.Contains(err.Error(), want) {
				t.Fatalf("readSeededSeries error = %v, want it to contain %q", err, want)
			}
		})
	}
}

// TestReadSeededSeries_UnseededTableIsSkipped pins the other half: the
// tables a fixture never created are skipped without error, and the one it
// did create still yields its series.
func TestReadSeededSeries_UnseededTableIsSkipped(t *testing.T) {
	chdbEngineMu.Lock()
	defer chdbEngineMu.Unlock()

	const seed = "CREATE TABLE otel_metrics_gauge (MetricName String, Attributes Map(String, String), " +
		"TimeUnix DateTime64(9), Value Float64) ENGINE = Memory;\n" +
		"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES " +
		"('m', map('job', 'a'), toDateTime64('2026-01-01 00:00:00', 9), 1.0);"
	db := OpenChDB(t)
	ApplySeed(t, db, seed)

	got, err := readSeededSeries(db, &RoundTripSections{Seed: seed}, nil, "m")
	if err != nil {
		t.Fatalf("readSeededSeries: %v", err)
	}
	if len(got.series) != 1 {
		t.Fatalf("readSeededSeries returned %d series, want the 1 gauge series", len(got.series))
	}
}

// TestReadSeededClassicHistograms_UndeclaredBucketColumns pins that a
// classic-histogram table declaring Count and Sum but neither bucket
// column, the shape a fixture reading only _count and _sum seeds, is read
// as a histogram with no finite buckets rather than failing.
func TestReadSeededClassicHistograms_UndeclaredBucketColumns(t *testing.T) {
	chdbEngineMu.Lock()
	defer chdbEngineMu.Unlock()

	const seed = "CREATE TABLE otel_metrics_histogram (MetricName String, Attributes Map(String, String), " +
		"TimeUnix DateTime64(9), Count UInt64, Sum Float64) ENGINE = Memory;\n" +
		"INSERT INTO otel_metrics_histogram VALUES " +
		"('lat', map('job', 'a'), toDateTime64('2026-01-01 00:00:00', 9), 4, 10.0);"
	db := OpenChDB(t)
	ApplySeed(t, db, seed)

	byKey := map[string]*oracle.Series{}
	nameRestorer := newMetricNameRestorer()
	if err := readSeededClassicHistograms(
		db, &RoundTripSections{Seed: seed}, nil, byKey, &nameRestorer, map[string]bool{},
	); err != nil {
		t.Fatalf("readSeededClassicHistograms: %v", err)
	}
	want := map[string]float64{
		`__name__=lat_bucket,job=a,le=+Inf`: 4,
		`__name__=lat_count,job=a`:          4,
		`__name__=lat_sum,job=a`:            10,
	}
	if len(byKey) != len(want) {
		t.Fatalf("readSeededClassicHistograms built %d series, want %d: %v", len(byKey), len(want), byKey)
	}
	for key, wantValue := range want {
		s, ok := byKey[key]
		if !ok {
			t.Errorf("series %s was not reconstructed", key)
			continue
		}
		if len(s.Points) != 1 || !oracle.EqualValues(s.Points[0].Value, wantValue) {
			t.Errorf("series %s = %v, want one point of %v", key, s.Points, wantValue)
		}
	}
}
