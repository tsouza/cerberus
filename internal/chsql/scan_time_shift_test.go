package chsql

import (
	"context"
	"errors"
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
// the side the shift moves towards.
func TestScanTimeShift_MovesTimestampsInInt64(t *testing.T) {
	cases := []struct {
		shift int64
		want  []string
	}{
		{60000, []string{
			"fromUnixTimestamp64Nano(toUnixTimestamp64Nano(`TimeUnix`) + 60000000000) AS `TimeUnix`",
			"WHERE toUnixTimestamp64Nano(`TimeUnix`) <= 9223371976854775807",
		}},
		{-60000, []string{
			"fromUnixTimestamp64Nano(toUnixTimestamp64Nano(`TimeUnix`) + -60000000000) AS `TimeUnix`",
			"WHERE toUnixTimestamp64Nano(`TimeUnix`) >= -9223371976854775808",
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

// TestScanTimeShift_Refusals pins that a shift whose nanoseconds do not
// fit Int64, or a shifted scan with no timestamp column, is refused.
func TestScanTimeShift_Refusals(t *testing.T) {
	if _, _, err := Emit(context.Background(), shiftTestScan(maxScanTimeShiftMillis+1)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("oversized shift: err = %v, want ErrUnsupported", err)
	}
	noTs := shiftTestScan(1)
	noTs.Roles = nil
	if _, _, err := Emit(context.Background(), noTs); err == nil {
		t.Fatal("shifted scan without a timestamp column: want an error")
	}
}

// TestScanTimeShift_TakesNoPrewhere pins that a filter over a shifted
// scan of a table whose filters are promoted to PREWHERE keeps them in
// WHERE: the shifted scan reads a subquery, which takes no PREWHERE.
func TestScanTimeShift_TakesNoPrewhere(t *testing.T) {
	for _, shift := range []int64{0, 60000} {
		scan := &chplan.Scan{
			Table:           "otel_logs",
			Roles:           []chplan.Column{{Name: "Timestamp", Role: chplan.RoleTimestamp}},
			TimeShiftMillis: shift,
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
		if got, want := strings.Contains(sql, "PREWHERE"), shift == 0; got != want {
			t.Fatalf("shift %d: PREWHERE present = %v, want %v:\n%s", shift, got, want, sql)
		}
	}
}
