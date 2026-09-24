package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestStaleMarkers_EverySelectorShapeReadsFlags pins that every selector
// shape reading a Value-bearing arm applies the stale-marker rules under a
// schema with a Flags column, and that a schema declaring none (FlagsColumn
// "") emits SQL that never names a Flags column. The chDB fixtures
// test/spec/promql/stale_marker_*.txtar pin the answers against reference
// Prometheus; this pins the reach across the selector builders.
func TestStaleMarkers_EverySelectorShapeReadsFlags(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	const step = 30 * time.Second
	cases := []struct {
		name  string
		query string
		// wantDrop: the range-selection rule (the marker row is dropped at
		// the scan). wantEncode: the instant-selection rule (the marker's
		// Value is rewritten, and the latest-sample pick filtered on it).
		wantDrop, wantEncode bool
		rangeMode            bool
	}{
		{name: "instant bare", query: `up`, wantEncode: true},
		{name: "range-mode bare", query: `up`, wantEncode: true, rangeMode: true},
		{name: "range-mode absolute at", query: `up @ 1767225600`, wantEncode: true, rangeMode: true},
		{name: "instant rate", query: `rate(http_requests_total[5m])`, wantDrop: true},
		{name: "range-mode rate", query: `rate(http_requests_total[5m])`, wantDrop: true, rangeMode: true},
		{name: "companion count union", query: `http_duration_seconds_count`, wantEncode: true},
		{name: "companion count union rate", query: `rate(http_duration_seconds_count[5m])`, wantDrop: true},
		{name: "regex name", query: `{__name__=~"http_.*"}`, wantEncode: true},
		{name: "regex name rate", query: `rate({__name__=~"http_.*"}[5m])`, wantDrop: true},
		{name: "range-mode subquery inner", query: `max_over_time(up[5m:1m])`, wantEncode: true, rangeMode: true},
	}
	p := parser.NewParser(parser.Options{})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.query, err)
			}
			emit := func(s schema.Metrics) string {
				t.Helper()
				var sql string
				if tc.rangeMode {
					plan, err := LowerAtRange(context.Background(), expr, s, start, end, step)
					if err != nil {
						t.Fatalf("lower %q: %v", tc.query, err)
					}
					sql, _, err = chsql.Emit(context.Background(), plan)
					if err != nil {
						t.Fatalf("emit %q: %v", tc.query, err)
					}
					return sql
				}
				plan, err := LowerAt(context.Background(), expr, s, end, end)
				if err != nil {
					t.Fatalf("lower %q: %v", tc.query, err)
				}
				sql, _, err = chsql.Emit(context.Background(), plan)
				if err != nil {
					t.Fatalf("emit %q: %v", tc.query, err)
				}
				return sql
			}

			sql := emit(schema.DefaultOTelMetrics())
			const (
				dropMark   = "not((bitAnd(`Flags`, ?) != ?))"
				encodeMark = "if((bitAnd(`Flags`, ?) != ?), reinterpretAsFloat64(?)"
				latestMark = "reinterpretAsUInt64(`Value`) != ?"
			)
			if got := strings.Contains(sql, dropMark); got != tc.wantDrop {
				t.Errorf("range-selection drop present = %v, want %v:\n%s", got, tc.wantDrop, sql)
			}
			if got := strings.Contains(sql, encodeMark) && strings.Contains(sql, latestMark); got != tc.wantEncode {
				t.Errorf("instant-selection encoding present = %v, want %v:\n%s", got, tc.wantEncode, sql)
			}

			noFlags := schema.DefaultOTelMetrics()
			noFlags.FlagsColumn = ""
			if sql := emit(noFlags); strings.Contains(sql, "Flags") || strings.Contains(sql, "reinterpretAs") {
				t.Errorf("a schema with no Flags column must not read one:\n%s", sql)
			}
		})
	}
}

// TestStaleMarkers_MetadataIgnoresFlags pins that metadata enumeration
// reads no Flags column: a series whose only in-window sample is a stale
// marker still exists for /series, /labels and /label/<name>/values.
func TestStaleMarkers_MetadataIgnoresFlags(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(`up`)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := LowerMetadataRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "Flags") {
		t.Errorf("metadata lowering must not read Flags:\n%s", sql)
	}
}
