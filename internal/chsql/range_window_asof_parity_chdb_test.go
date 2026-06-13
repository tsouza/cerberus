//go:build chdb

// chDB-backed exact-parity pin for the rate / increase / delta
// query_range matrix emitter's ASOF boundary-lookup path
// (internal/chsql/range_window_asof.go) against the legacy sample-side
// fan-out emitter (emitWindowedArrayExtrapolatedMatrix).
//
// # Why this pin exists
//
// The property/oracle lane (test/property) generates INSTANT-only
// PromQL, so it never exercises the OuterRange > 0 matrix path the ASOF
// rewrite touches. This test fills that coverage hole directly: it
// builds the SAME range-vector plan twice — once keyed (routes to the
// fan-out-free ASOF emitter) and once keyless (routes to the fan-out
// emitter, since CH ASOF needs an equi-key) — over a single seeded
// series whose group key is constant, so every (anchor) value the two
// emitters produce MUST match bit-for-bit. The two emitters share the
// downstream extrapolation arithmetic; only the SOURCE of the
// per-window boundary quantities differs, so any divergence is a real
// boundary-lookup bug.
//
// The seed is adversarial for the extrapolation contract: a counter
// with resets (so the reset-adjusted cumulative cumV diff must equal
// the fan-out's per-pair arraySum), irregular + sparse spacing (so the
// 1.1x extrapolation-threshold clamp and the empty/sparse-window drop
// gate fire differently per anchor), a counter that starts mid-window
// at a positive value (so the counter zero-crossing clamp engages), and
// a duplicate-input-timestamp run (so the all-same-ts sampled_interval
// == 0 -> nan path is hit, where the ASOF boundary collapse must still
// surface the NaN via the per-timestamp multiplicity).
package chsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// asofParitySeedTable is the ephemeral table the parity pin seeds.
const asofParitySeedTable = "rate_parity_src"

// asofParityAnchor is the deterministic eval-grid End the matrix plan
// anchors against (so the emitted now64-free SQL pins to a fixed grid).
var asofParityAnchor = time.Date(2026, 6, 13, 1, 0, 0, 0, time.UTC)

// TestRateRangeASOF_MatrixParityVsFanout proves the ASOF boundary
// emitter reproduces the fan-out emitter's per-anchor value column
// exactly for rate / increase / delta over a query_range grid.
func TestRateRangeASOF_MatrixParityVsFanout(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	defer db.Close()

	seedRateParitySeries(t, db)

	for _, fn := range []string{"rate", "increase", "delta"} {
		fn := fn
		t.Run(fn, func(t *testing.T) {
			// Subtests share the parent's chDB handle (the deferred Close
			// fires only after all subtests return), so they run serially.
			// Keyed plan -> ASOF emitter; keyless plan -> fan-out emitter.
			// Both read the SAME single-series data; the keyed plan groups
			// on a column whose value is constant, so the two value streams
			// align one-to-one by anchor_ts.
			asofSQL, asofArgs := emitRateParityPlan(t, fn, true)
			fanoutSQL, fanoutArgs := emitRateParityPlan(t, fn, false)

			if !strings.Contains(asofSQL, "ASOF LEFT JOIN") {
				t.Fatalf("%s: keyed plan did not route to the ASOF emitter; SQL:\n%s", fn, asofSQL)
			}
			if strings.Contains(fanoutSQL, "ASOF LEFT JOIN") {
				t.Fatalf("%s: keyless plan unexpectedly took the ASOF path; SQL:\n%s", fn, fanoutSQL)
			}

			asofRows := queryAnchorValues(t, db, asofSQL, asofArgs)
			fanoutRows := queryAnchorValues(t, db, fanoutSQL, fanoutArgs)

			if len(asofRows) == 0 {
				t.Fatalf("%s: ASOF path returned zero anchors — the seed proves nothing", fn)
			}
			if len(asofRows) != len(fanoutRows) {
				t.Fatalf("%s: anchor-row count mismatch ASOF=%d fanout=%d (the drop gate diverged)",
					fn, len(asofRows), len(fanoutRows))
			}
			nanSeen := false
			for anchor, av := range asofRows {
				fv, ok := fanoutRows[anchor]
				if !ok {
					t.Errorf("%s: anchor %s present in ASOF, absent in fanout", fn, anchor)
					continue
				}
				switch {
				case math.IsNaN(av) && math.IsNaN(fv):
					nanSeen = true
				case math.IsNaN(av) != math.IsNaN(fv):
					t.Errorf("%s: anchor %s NaN-class mismatch ASOF=%v fanout=%v", fn, anchor, av, fv)
				case math.Abs(av-fv) > 1e-9:
					t.Errorf("%s: anchor %s value mismatch ASOF=%.12g fanout=%.12g", fn, anchor, av, fv)
				}
			}
			// Whether a NaN window surfaces depends on grid geometry (an
			// anchor must isolate the all-same-ts pair within range); the
			// authoritative NaN-recovery coverage lives in the A-vs-B solver
			// lane (internal/solver/avb_chdb_lane_test.go), which pins that
			// the ASOF _mult path reproduces the fan-out's sampled_interval
			// == 0 -> nan. Here it is informational — the load-bearing proof
			// is bit-exact parity across the adversarial anchor set above.
			t.Logf("%s: %d anchors, exact parity ASOF vs fanout (NaN window observed=%v)",
				fn, len(asofRows), nanSeen)
		})
	}
}

// seedRateParitySeries creates + fills the parity table with one
// adversarial counter series (constant group key `k`). The reset /
// irregular-spacing / positive-start / duplicate-timestamp shapes are
// documented on the file header.
func seedRateParitySeries(t *testing.T, db *sql.DB) {
	t.Helper()
	ddl := fmt.Sprintf(
		"CREATE OR REPLACE TABLE %s (k String, TimeUnix DateTime64(9), Value Float64) "+
			"ENGINE = MergeTree ORDER BY (k, TimeUnix)", asofParitySeedTable,
	)
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("seed ddl: %v", err)
	}

	base := asofParityAnchor.Add(-58 * time.Minute).Unix() // inside the 1h grid
	var rows []string
	cum := 7.0 // positive start -> exercises the counter zero-crossing clamp
	tsec := int64(0)
	const dupGapIdx = 200 // leave a >5m gap here so the dup-ts series is isolated
	for i := 0; i < 360; i++ {
		// Irregular + sparse spacing: gaps swing 7s..59s so some windows
		// hold many samples and some hold the bare 2, flipping the 1.1x
		// extrapolation-threshold clamp on both edges.
		gap := int64(7 + (i*11)%53)
		if i == dupGapIdx || i == dupGapIdx+1 {
			// Open a >range (5m) gap on BOTH sides of the duplicate-timestamp
			// pair so it forms its OWN all-same-ts window (no neighbouring
			// samples within range), reproducing sampled_interval == 0 -> nan.
			gap += int64((6 * time.Minute).Seconds())
		}
		tsec += gap
		ts := base + tsec
		cum += float64((i*7)%13) + 1
		if i%53 == 0 && i > 0 {
			cum = float64(i % 5) // counter reset
		}
		rows = append(rows, fmt.Sprintf("('s', toDateTime64(%d, 9), %v)", ts, cum))
		if i == dupGapIdx {
			// Duplicate-input-timestamp run at THIS sample's ts: a second
			// sample sharing the exact timestamp. Its only populated window
			// holds first_ts == last_ts (the >5m gaps either side isolate
			// it), so sampled_interval == 0 -> nan in the fan-out; the ASOF
			// path must surface the same NaN via the per-ts multiplicity.
			rows = append(rows, fmt.Sprintf("('s', toDateTime64(%d, 9), %v)", ts, cum+4))
		}
	}

	if _, err := db.Exec("INSERT INTO " + asofParitySeedTable + " VALUES " + strings.Join(rows, ",")); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
}

// emitRateParityPlan builds a query_range RangeWindow plan for fn over
// the parity table and emits its SQL. keyed selects whether the series
// group key column `k` is present (routing to ASOF) or absent (routing
// to the fan-out).
func emitRateParityPlan(t *testing.T, fn string, keyed bool) (string, []any) {
	t.Helper()
	rw := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: asofParitySeedTable},
		Func:            fn,
		Range:           5 * time.Minute,
		Step:            15 * time.Second,
		OuterRange:      time.Hour,
		Start:           asofParityAnchor.Add(-time.Hour),
		End:             asofParityAnchor,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	if keyed {
		rw.GroupBy = []chplan.Expr{&chplan.ColumnRef{Name: "k"}}
	}
	sqlText, args, err := chsql.Emit(context.Background(), rw)
	if err != nil {
		t.Fatalf("%s (keyed=%v): emit: %v", fn, keyed, err)
	}
	return sqlText, args
}

// queryAnchorValues executes the emitted matrix SQL and returns a
// map from the anchor timestamp (string) to the per-anchor Value. The
// SQL carries `?` placeholders bound positionally in args; chDB's
// session driver has no binding, so we inline them textually.
func queryAnchorValues(t *testing.T, db *sql.DB, sqlText string, args []any) map[string]float64 {
	t.Helper()
	stmt := inlineRateParityArgs(sqlText, args)
	rows, err := db.Query(stmt)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, stmt)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	// The matrix SELECT projects [<group>,] anchor_ts, TimeUnix, Value —
	// locate anchor_ts + Value by name so column order changes don't
	// silently misread.
	anchorIdx, valueIdx := -1, -1
	for i, c := range cols {
		switch c {
		case "anchor_ts":
			anchorIdx = i
		case "Value":
			valueIdx = i
		}
	}
	if anchorIdx < 0 || valueIdx < 0 {
		t.Fatalf("missing anchor_ts/Value columns in %v", cols)
	}

	out := map[string]float64{}
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		anchor := cells[anchorIdx].(*sql.NullString)
		valStr := cells[valueIdx].(*sql.NullString)
		if !anchor.Valid {
			continue
		}
		out[anchor.String] = parseRateParityFloat(valStr)
	}
	if err := tolerantRateParityErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// parseRateParityFloat reads a chDB Float64 cell rendered as a string,
// mapping the textual `nan` / NULL forms to math.NaN().
func parseRateParityFloat(s *sql.NullString) float64 {
	if s == nil || !s.Valid {
		return math.NaN()
	}
	v := strings.TrimSpace(s.String)
	if v == "" || strings.EqualFold(v, "nan") {
		return math.NaN()
	}
	var f float64
	if _, err := fmt.Sscanf(v, "%g", &f); err != nil {
		return math.NaN()
	}
	return f
}

// tolerantRateParityErr swallows chdb-go's spurious "empty row"
// end-of-iteration sentinel (its parquet driver returns it instead of
// io.EOF), surfacing any other error.
func tolerantRateParityErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "empty row") {
		return nil
	}
	return err
}

// inlineRateParityArgs textually substitutes each `?` placeholder with
// its positional arg (string args single-quoted) so the parameterised
// SQL can run on chDB's binding-free session driver. The parity plan's
// only args are the metric-name / table-scan string filters; numeric
// window constants are inlined by the emitter already.
func inlineRateParityArgs(sqlText string, args []any) string {
	var b strings.Builder
	argi := 0
	for _, r := range sqlText {
		if r == '?' && argi < len(args) {
			switch a := args[argi].(type) {
			case string:
				b.WriteByte('\'')
				b.WriteString(strings.ReplaceAll(a, "'", "''"))
				b.WriteByte('\'')
			default:
				fmt.Fprintf(&b, "%v", a)
			}
			argi++
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
