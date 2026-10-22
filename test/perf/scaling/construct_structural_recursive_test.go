//go:build chdb

// Construct: structural_recursive — TraceQL structural `>>` / `<<` closure.
//
// Folds in the standalone structural_recursive_scaling_chdb guard. The
// recursive `WITH RECURSIVE` closure that lowers a descendant/ancestor
// query used to scan the BARE FULL otel_traces table on every recursion
// level — no time-range, no trace-id restriction pushed into the recursive
// arm. Each of `depth` iterations re-scanned the whole table, an
// O(depth x full-scan) shape whose wall grew LINEARLY with chain depth even
// at constant row count. The #808 fix pushes the seed subquery's trace-id
// set into the recursive arm (`t.TraceId IN (SELECT TraceId FROM <seed>)`),
// so each iteration reads only the candidate-trace rows.
//
// THE REAL MULTIPLIER is the recursion DEPTH D. Param = D, swept 4 -> 16
// -> 48. The row set is REBUILT per point (Reseed) so total span rows stay
// ~constant (candidates/D traces of D spans each) — the only variable is
// how DEEP the recursion walks. The production lowering's wall stays
// sub-linear in D, and the closure's per-level intermediate (the recursive
// CTE's row count) stays bounded by the candidate-trace rows, NOT
// rows x depth.
package scaling

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	traceqlast "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

const structuralSeedDDL = `CREATE TABLE otel_traces (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String DEFAULT '',
    Duration UInt64 DEFAULT 0,
    Timestamp UInt64 DEFAULT 0,
    ResourceAttributes Map(String, String) DEFAULT map(),
    SpanAttributes Map(String, String) DEFAULT map(),
    StatusCode String DEFAULT '',
    StatusMessage String DEFAULT '',
    SpanKind String DEFAULT '',
    ScopeName String DEFAULT '',
    ScopeVersion String DEFAULT ''
) ENGINE = Memory;`

func init() {
	const totalRows = 60000
	const candidateRows = 1200 // ~2% of the table matches the search

	register(Construct{
		Name:        "structural_recursive",
		Param:       "recursion depth D",
		Why:         "structural `>>` recursive CTE bare full-table rescan per level (O(depth x full-scan))",
		ScanRowsSQL: "SELECT count() FROM otel_traces",
		// The recursive closure's per-level frontier is bounded by the
		// candidate-trace rows (~candidateRows), a small fraction of the
		// table. The pushed-down closure's total emitted rows stay well
		// under the scan; bound at 2x scan_rows (the closure projects at
		// most every candidate span once per its single ancestry path).
		CardinalityBound: 2.0,
		// This query is measured after the long perf corpus on shared CI
		// runners. Keep the gate decisively sub-linear while allowing the
		// occasional cold-cache tail to affect a single endpoint sample.
		SubLinearSlack: 1.25,
		Reseed: func(t *testing.T, db *sql.DB, param int64) {
			d := int(param)
			candidates := candidateRows / d // fixed candidate ROW count -> /D traces
			noise := (totalRows - candidateRows) / d
			execAll(t, db, "DROP TABLE IF EXISTS otel_traces", structuralSeedDDL,
				structuralSeedInsert(candidates, noise, d))
		},
		Points: func(t *testing.T) []Point {
			sqlText, args := emitStructuralRecursiveSQL(t)
			closureLevel := structuralClosureLevel(sqlText)
			depths := []int64{4, 16, 48}
			pts := make([]Point, 0, len(depths))
			for _, d := range depths {
				pts = append(pts, Point{
					Param:     d,
					SQL:       stripTrailingSemi(sqlText),
					Args:      args,
					LevelSQLs: []string{closureLevel},
				})
			}
			return pts
		},
	})
}

// structuralSeedInsert builds a span table with two trace populations:
// `candidates` matching traces (root=root_marker -> ... -> leaf=leaf_marker,
// each a linear chain of `depth` spans) and `noise` non-matching traces
// (all service.name='noise'). The pushed-down closure walks only the
// candidates; the pre-fix bare-rescan re-read every noise row per level.
// Total span rows = (candidates + noise) x depth.
//
// Row n of numbers() is span n of the table: trace n/depth, level n%depth.
// Candidate traces come first, so a trace ordinal below `candidates` is a
// candidate. Span ids are the 1-based span ordinal in hex, a span's parent is
// the span before it in its chain, and a trace id is a population prefix
// ('a' candidate, 'b' noise) followed by the 1-based ordinal within that
// population in hex.
//
// The rows are generated in-engine (INSERT ... SELECT FROM numbers()) rather
// than as a VALUES literal: a VALUES row carrying a map() expression is
// evaluated row by row, which took ~40s per 60k-row point, about three
// quarters of the whole harness's runtime. The generated set is the same:
// EXCEPT ALL against the former Go-built VALUES literal is empty in both
// directions at every swept depth.
func structuralSeedInsert(candidates, noise, depth int) string {
	return fmt.Sprintf(`INSERT INTO otel_traces (TraceId, SpanId, ParentSpanId, ResourceAttributes)
SELECT
  concat(if(tr < %[1]d, 'a', 'b'),
         leftPad(lower(hex(if(tr < %[1]d, toUInt64(tr), toUInt64(tr - %[1]d)) + 1)), %[4]d, '0')),
  leftPad(lower(hex(toUInt64(number + 1))), %[5]d, '0'),
  if(lvl = 0, '', leftPad(lower(hex(toUInt64(number))), %[5]d, '0')),
  map('service.name', multiIf(tr >= %[1]d, 'noise', lvl = 0, 'root_marker', lvl = %[3]d - 1, 'leaf_marker', 'mid'))
FROM (SELECT number, intDiv(number, %[3]d) AS tr, number %% %[3]d AS lvl FROM numbers(%[2]d));`,
		candidates, (candidates+noise)*depth, depth, structuralTraceOrdinalHexWidth, structuralSpanIDHexWidth)
}

// structuralSpanIDHexWidth is the hex-digit width of a span id: a 64-bit id
// rendered as 16 hex digits, the OTel span-id width.
const structuralSpanIDHexWidth = 16

// structuralTraceOrdinalHexWidth is the hex-digit width of a trace id's
// ordinal part: the OTel 32-hex-digit trace id minus its one-character
// population prefix.
const structuralTraceOrdinalHexWidth = 31

// emitStructuralRecursiveSQL lowers `root_marker >> leaf_marker` through
// the real cerberus parse -> lower -> emit chain — the pushed-down shape.
func emitStructuralRecursiveSQL(t *testing.T) (string, []any) {
	t.Helper()
	expr, err := traceqlast.Parse(`{ resource.service.name = "root_marker" } >> { resource.service.name = "leaf_marker" }`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := traceql.Lower(context.Background(), expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sqlText, args
}

// structuralClosureLevel reconstructs the recursive-closure frontier as a
// standalone countable level: the DISTINCT descendant (TraceId, SpanId)
// rows the seed-pushed-down WITH RECURSIVE produces. This is the
// intermediate stage the pushdown bounds (to candidate-trace rows) and the
// bare rescan would inflate. Kept as an inline mirror of the emitter's
// recursive arm with the seed-trace-id pushdown the fix always carries
// (a depth cap matches the new emitter and never truncates the acyclic
// chains under test).
func structuralClosureLevel(_ string) string {
	return `WITH RECURSIVE _closure AS (
	  SELECT DISTINCT TraceId, SpanId, ParentSpanId, 0 AS _depth
	    FROM (SELECT * FROM otel_traces WHERE ResourceAttributes['service.name'] = 'root_marker') AS _seed
	  UNION ALL
	  SELECT DISTINCT t.TraceId, t.SpanId, t.ParentSpanId, c._depth + 1
	    FROM otel_traces AS t
	    INNER JOIN _closure AS c
	      ON t.TraceId = c.TraceId AND t.ParentSpanId = c.SpanId
	    WHERE c._depth < 128
	      AND t.TraceId IN (SELECT TraceId FROM otel_traces WHERE ResourceAttributes['service.name'] = 'root_marker')
	)
	SELECT DISTINCT TraceId, SpanId FROM _closure WHERE _depth > 0`
}
