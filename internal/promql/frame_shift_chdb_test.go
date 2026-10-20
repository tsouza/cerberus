//go:build chdb

package promql

import (
	"context"
	"strconv"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

// TestUnshiftRoot_WideShiftLandsExactly_ChDB pins, in ClickHouse, that a
// root lowered in a frame shifted further than Int64 nanoseconds hold
// reports its instant as a DateTime64(9), moved back to the exact
// nanosecond Prometheus evaluates at: a frame instant near one edge of the
// domain lands near the other, as a query root and as an isolated run.
func TestUnshiftRoot_WideShiftLandsExactly_ChDB(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	const subMilli = 123_456_789
	nearFloor := time.Date(1677, 9, 21, 0, 20, 0, subMilli, time.UTC)
	nearCeiling := time.Date(2262, 4, 11, 23, 46, 0, subMilli, time.UTC)
	for _, tc := range []struct {
		name          string
		frame, actual time.Time
		rootFrame     int64
	}{
		{"query root moved up", nearFloor, nearCeiling, 0},
		{"query root moved down", nearCeiling, nearFloor, 0},
		{"isolated run moved up", nearFloor, nearCeiling, -60_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The frame lies shift milliseconds after Prometheus's instants;
			// a run reports its instants in the frame rootFrame names, so it
			// moves them back by shift less rootFrame.
			shift := tc.frame.UnixMilli() - tc.actual.UnixMilli() + tc.rootFrame
			if offsetFits(shift - tc.rootFrame) {
				t.Fatalf("shift %dms has an Int64 nanosecond form", shift-tc.rootFrame)
			}
			db := spec.OpenChDB(t)
			spec.ApplySeed(t, db, "CREATE TABLE frame_probe (TimeUnix DateTime64(9)) ENGINE = Memory;\n"+
				"INSERT INTO frame_probe VALUES (fromUnixTimestamp64Nano(toInt64("+strconv.FormatInt(tc.frame.UnixNano(), 10)+")));\n")
			plan := unshiftRoot(&chplan.Scan{
				Table:   "frame_probe",
				Columns: []string{s.TimestampColumn},
				Roles:   []chplan.Column{{Name: s.TimestampColumn, Role: chplan.RoleTimestamp}},
			}, mustParseExperimental(t, `m`), s, lowerCtx{rootFrame: tc.rootFrame, domain: &domainFit{frameShift: shift}})
			emitted, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			query, qargs := chsql.NewQuery().
				Select(chsql.Call(string(chplan.FnToUnixNanos), chsql.Col(s.TimestampColumn)), chsql.Call("toTypeName", chsql.Col(s.TimestampColumn))).
				From(chsql.Subquery(chsql.PreRenderedSQL{SQL: emitted, Args: args})).
				Build()
			var (
				nanos int64
				typ   string
			)
			if err := db.QueryRow(query, qargs...).Scan(&nanos, &typ); err != nil {
				t.Fatal(err)
			}
			if typ != "DateTime64(9)" {
				t.Fatalf("timestamp type %s, want DateTime64(9)", typ)
			}
			if nanos != tc.actual.UnixNano() {
				t.Fatalf("timestamp %s, want %s", time.Unix(0, nanos).UTC(), tc.actual)
			}
		})
	}
}
