package chsql

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// shiftTestScan is a scan of a gauge table read with the given shift.
func shiftTestScan(shift int64) *chplan.Scan {
	return &chplan.Scan{
		Table:           "otel_metrics_gauge",
		Columns:         []string{"TimeUnix", "Value"},
		Roles:           []chplan.Column{{Name: "TimeUnix", Role: chplan.RoleTimestamp}},
		TimeShiftMillis: shift,
	}
}

// TestScanTimeShift_MovesTimestampsInInt64 pins the emission of a
// time-shifted scan: the timestamp column is moved in Int64 nanoseconds
// over the rows whose moved timestamp a DateTime64(9) holds, bounded on
// the side the shift moves towards in an inner query, where the bound
// reads the stored timestamp rather than the moved one its alias names.
func TestScanTimeShift_MovesTimestampsInInt64(t *testing.T) {
	cases := []struct {
		shift int64
		want  []string
	}{
		{60000, []string{
			"fromUnixTimestamp64Nano(toUnixTimestamp64Nano(`TimeUnix`) + 60000000000) AS `TimeUnix`",
			"FROM (SELECT * FROM `otel_metrics_gauge` WHERE toUnixTimestamp64Nano(`TimeUnix`) <= 9223371976854775807)",
		}},
		{-60000, []string{
			"fromUnixTimestamp64Nano(toUnixTimestamp64Nano(`TimeUnix`) + -60000000000) AS `TimeUnix`",
			"FROM (SELECT * FROM `otel_metrics_gauge` WHERE toUnixTimestamp64Nano(`TimeUnix`) >= -9223371976854775808)",
		}},
	}
	for _, tc := range cases {
		plan := &chplan.Filter{Input: shiftTestScan(tc.shift), Predicate: &chplan.Binary{
			Op: chplan.OpGt, Left: &chplan.ColumnRef{Name: "Value"}, Right: &chplan.LitFloat{V: 0},
		}}
		sql, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("shift %d: %v", tc.shift, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(sql, w) {
				t.Fatalf("shift %d: SQL lacks %q:\n%s", tc.shift, w, sql)
			}
		}
	}
	sql, _, err := Emit(context.Background(), shiftTestScan(0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "toUnixTimestamp64Nano") {
		t.Fatalf("an unshifted scan moves no timestamp:\n%s", sql)
	}
}

// TestScanTimeShift_MovesMillisPastInt64Nanos pins the emission of a
// scan shifted further than Int64 nanoseconds hold: the timestamp's Unix
// milliseconds are moved, over the rows whose moved milliseconds a
// DateTime64(9) holds.
func TestScanTimeShift_MovesMillisPastInt64Nanos(t *testing.T) {
	cases := []struct {
		shift int64
		want  []string
	}{
		{maxScanTimeShiftMillis + 1, []string{
			"fromUnixTimestamp64Nano((toUnixTimestamp64Milli(`TimeUnix`) + 9223372036855) * 1000000) AS `TimeUnix`",
			"FROM (SELECT * FROM `otel_metrics_gauge` WHERE toUnixTimestamp64Milli(`TimeUnix`) <= -1)",
		}},
		{-maxScanTimeShiftMillis - 1, []string{
			"fromUnixTimestamp64Nano((toUnixTimestamp64Milli(`TimeUnix`) + -9223372036855) * 1000000) AS `TimeUnix`",
			"FROM (SELECT * FROM `otel_metrics_gauge` WHERE toUnixTimestamp64Milli(`TimeUnix`) >= 1)",
		}},
	}
	for _, tc := range cases {
		sql, _, err := Emit(context.Background(), shiftTestScan(tc.shift))
		if err != nil {
			t.Fatalf("shift %d: %v", tc.shift, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(sql, w) {
				t.Fatalf("shift %d: SQL lacks %q:\n%s", tc.shift, w, sql)
			}
		}
		if strings.Contains(sql, "toUnixTimestamp64Nano(`TimeUnix`)") {
			t.Fatalf("shift %d: the nanoseconds of a shift past Int64 are moved:\n%s", tc.shift, sql)
		}
	}
	sql, _, err := Emit(context.Background(), shiftTestScan(maxScanTimeShiftMillis))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "toUnixTimestamp64Milli") {
		t.Fatalf("a shift whose nanoseconds Int64 holds moves milliseconds:\n%s", sql)
	}
}

// TestScanTimeShift_Refusals pins that a shifted scan with no timestamp
// column is refused.
func TestScanTimeShift_Refusals(t *testing.T) {
	noTs := shiftTestScan(1)
	noTs.Roles = nil
	if _, _, err := Emit(context.Background(), noTs); err == nil {
		t.Fatal("shifted scan without a timestamp column: want an error")
	}
}

// TestScanTimeShift_TakesNoPrewhere pins that a filter over a shifted or
// a time-bounded scan of a table whose filters are promoted to PREWHERE
// keeps them in WHERE: the scan reads a subquery, which takes no
// PREWHERE.
func TestScanTimeShift_TakesNoPrewhere(t *testing.T) {
	for _, tc := range []struct {
		shift int64
		read  *chplan.MillisSpan
	}{{0, nil}, {60000, nil}, {0, &chplan.MillisSpan{From: 1, To: 2}}} {
		shift := tc.shift
		scan := &chplan.Scan{
			Table:           "otel_logs",
			Roles:           []chplan.Column{{Name: "Timestamp", Role: chplan.RoleTimestamp}},
			TimeShiftMillis: shift,
			ReadMillis:      tc.read,
		}
		plan := &chplan.Filter{Input: scan, Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "api"}},
			Right: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "Body"}, Right: &chplan.LitString{V: "x"}},
		}}
		sql, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("shift %d: %v", shift, err)
		}
		if got, want := strings.Contains(sql, "PREWHERE"), shift == 0 && tc.read == nil; got != want {
			t.Fatalf("shift %d, read %v: PREWHERE present = %v, want %v:\n%s", shift, tc.read, got, want, sql)
		}
	}
}

// TestScanReadMillis_BoundsStoredTimestamps pins the emission of a scan
// bounded to stored timestamps: an inner query keeps the rows whose
// stored Unix milliseconds lie in the span, before any shift moves them.
func TestScanReadMillis_BoundsStoredTimestamps(t *testing.T) {
	const bound = "FROM (SELECT * FROM `otel_metrics_gauge` WHERE toUnixTimestamp64Milli(`TimeUnix`) >= -5 AND toUnixTimestamp64Milli(`TimeUnix`) <= 7)"
	for _, shift := range []int64{0, 60000} {
		scan := shiftTestScan(shift)
		scan.ReadMillis = &chplan.MillisSpan{From: -5, To: 7}
		plan := &chplan.Filter{Input: scan, Predicate: &chplan.Binary{
			Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "MetricName"}, Right: &chplan.LitString{V: "m"},
		}}
		sql, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("shift %d: %v", shift, err)
		}
		if !strings.Contains(sql, bound) {
			t.Fatalf("shift %d: SQL lacks the stored-timestamp bound %q:\n%s", shift, bound, sql)
		}
		moved := strings.Contains(sql, "toUnixTimestamp64Nano(`TimeUnix`) + 60000000000")
		if moved != (shift != 0) {
			t.Fatalf("shift %d: timestamp moved = %v:\n%s", shift, moved, sql)
		}
	}
	sql, _, err := Emit(context.Background(), shiftTestScan(0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "toUnixTimestamp64Milli") {
		t.Fatalf("an unbounded scan bounds its timestamps:\n%s", sql)
	}
}
