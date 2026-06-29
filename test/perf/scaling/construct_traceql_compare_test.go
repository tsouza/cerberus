//go:build chdb

// Construct: traceql_compare — TraceQL span-attribute conjunction, K
// attrs/span.
//
// A NEW registered shape the Phase-1 audit flagged but which had no
// standalone guard: a span filter comparing K span attributes,
// `{ span.a0 = v && span.a1 = v && ... && span.aK = v }`. THE REAL
// MULTIPLIER is K = attrs-compared-per-span. The audit's concern: a
// compare/conjunction path that materialised an intermediate per compared
// attribute (a per-attribute JOIN or arrayJoin fan-out) would explode in K
// even at constant scanned rows.
//
// On current main the conjunction lowers to a SINGLE flat WHERE — K map
// lookups on one scan, no fan-out — so the intermediate equals the scan
// rows (a Filter only reduces) and the wall stays sub-linear in K (K cheap
// map probes per row). Param = K, swept 2 -> 8 -> 32. The bound pins that
// the path STAYS scan-bound: peak intermediate <= ~1x scan_rows,
// independent of K.
package scaling

import (
	"context"
	"fmt"
	"strings"
	"testing"

	traceqlast "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

func init() {
	register(Construct{
		Name:        "traceql_compare",
		Param:       "attrs/span K",
		Why:         "TraceQL span-attribute conjunction per-attribute fan-out (rows x K)",
		ScanRowsSQL: "SELECT count() FROM otel_traces",
		// A flat conjunctive Filter only REDUCES rows; the peak intermediate
		// is the scan itself. Bound at 1.1x scan_rows: a per-attribute JOIN /
		// arrayJoin regression (rows x K) blows straight past, while the
		// scan-bound shape sits at <=1x.
		CardinalityBound: 1.1,
		SubLinearSlack:   0.9,
		Seed: func() string {
			ddl := `DROP TABLE IF EXISTS otel_traces;
			CREATE TABLE otel_traces (
			    TraceId String, SpanId String, ParentSpanId String,
			    SpanName String DEFAULT '',
			    Timestamp UInt64 DEFAULT 0,
			    SpanAttributes Map(String, String) DEFAULT map(),
			    ResourceAttributes Map(String, String) DEFAULT map()
			) ENGINE = MergeTree() ORDER BY (TraceId, SpanId);`
			// 120k spans, each carrying 32 span attributes a0..a31 (so a
			// K-up-to-32 conjunction always has every key present). ~half the
			// spans set a0..a31 = 'x' (match), the rest 'y' (no match), so
			// the conjunction selects a real, non-trivial subset.
			ins := `INSERT INTO otel_traces (TraceId, SpanId, SpanAttributes) SELECT
			  lower(hex(toUInt128(intDiv(number, 4) + 1))),
			  lower(hex(reinterpretAsFixedString(toUInt64(number + 1)))),
			  mapFromArrays(
			    arrayMap(i -> concat('a', toString(i)), range(32)),
			    arrayMap(i -> if(number % 2 = 0, 'x', 'y'), range(32))
			  )
			FROM numbers(120000);`
			return ddl + ins
		},
		Points: func(t *testing.T) []Point {
			ks := []int64{2, 8, 32}
			pts := make([]Point, 0, len(ks))
			for _, k := range ks {
				sqlText, args := emitTraceQLCompareSQL(t, int(k))
				// Precondition: the conjunction must lower to a flat single
				// scan (no JOIN / arrayJoin / WITH RECURSIVE) — that is the
				// scan-bound shape the bound assumes. A regression that fanned
				// per attribute would introduce one of these.
				upper := strings.ToUpper(sqlText)
				for _, banned := range []string{" JOIN ", "ARRAYJOIN", "WITH RECURSIVE"} {
					if strings.Contains(upper, banned) {
						t.Fatalf("traceql_compare K=%d: emitted SQL contains %q — the conjunction is no longer a "+
							"flat scan-bound filter; a per-attribute fan-out regressed in:\n%s", k, banned, sqlText)
					}
				}
				pts = append(pts, Point{
					Param: k,
					SQL:   sqlText,
					Args:  args,
					// The whole filtered scan IS the intermediate — its row
					// count is what a per-attribute fan-out would inflate.
					LevelSQLs: []string{sqlText},
				})
			}
			return pts
		},
	})
}

// emitTraceQLCompareSQL lowers `{ span.a0 = "x" && ... && span.a{K-1} = "x" }`
// through the real cerberus parse -> lower -> emit chain.
func emitTraceQLCompareSQL(t *testing.T, k int) (string, []any) {
	t.Helper()
	preds := make([]string, k)
	for i := 0; i < k; i++ {
		preds[i] = fmt.Sprintf(`span.a%d = "x"`, i)
	}
	q := "{ " + strings.Join(preds, " && ") + " }"
	expr, err := traceqlast.Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	plan, err := traceql.Lower(context.Background(), expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("Lower(%q): %v", q, err)
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", q, err)
	}
	return sqlText, args
}
