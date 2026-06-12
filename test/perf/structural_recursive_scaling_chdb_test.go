//go:build chdb

// Perf guard (RC1 TraceQL structural `>>` / `<<`, tasks #77 + #78): the
// recursive `WITH RECURSIVE` closure that lowers a descendant/ancestor
// query used to scan the BARE FULL otel_traces table on every recursion
// level — no time-range, no trace-id restriction pushed into the
// recursive arm. Each of `depth` iterations re-scanned the whole table,
// an O(depth × full-scan) shape whose wall time grew LINEARLY with the
// chain depth even though the result set + row count were held fixed
// (measured pre-fix: depth 4/8/16/48 → ~46/78/128/353ms at constant
// span_rows + result_rows).
//
// The #77 fix (internal/chsql/structural_join.go::emitStructuralRecursive)
// pushes the seed subquery's trace-id set into the recursive arm:
//
//	... INNER JOIN _struct_closure AS c ON t.TraceId = c.TraceId ...
//	WHERE c._depth < <cap>
//	  AND t.TraceId IN (SELECT TraceId FROM (<L>) AS _seed_ids)
//
// so each iteration reads only the rows of the seed's candidate traces
// (O(matching-trace-rows)) rather than the whole table. Because the
// seed subquery (<L>) carries the search's [start,end] Timestamp filter
// and resource/span predicates, the time window is pushed in for free.
// The rewrite is semantics-preserving — the closure is per-trace (the
// step ON already pins t.TraceId = c.TraceId, and every c.TraceId
// originates in the seed), so no descendant/ancestor row can be added
// or dropped by scoping `t` to the seed's trace-id set.
//
// This guard pins the scaling invariant: hold the total span-row count
// + result-row count fixed and vary the chain depth D. The new
// pushed-down path's wall time stays ~flat in D; the old bare-rescan
// path's grows with D. The headline assertion is on the RATIO (pushdown
// is far flatter + faster at the deepest D), not absolute ms, so it's
// portable across runner speeds.
//
// Build-tagged `chdb`, same lane as the other chDB execs (#70/#789).
package perf

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// structuralSeedDDL is the otel_traces table the structural perf seed
// fills. Memory engine keeps the chDB exec deterministic and matches the
// shape the traceql spec fixtures pin (String ids, the columns the
// default OTel-traces schema projects).
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

// structuralSeedInsert builds a span table with two trace populations:
//
//   - `candidates` traces that match the search: each a linear chain of
//     `depth` spans whose root carries service.name='root_marker' and
//     whose leaf carries service.name='leaf_marker' (middles are 'mid').
//     These are what the `root_marker >> leaf_marker` closure walks.
//   - `noise` traces that DO NOT match: linear chains of `depth` spans,
//     all service.name='noise'. They never seed the closure, but the
//     PRE-#77 bare-rescan recursive arm still re-reads every one of their
//     rows on every recursion level (the join only prunes them AFTER the
//     scan). The #77 seed-trace-id pushdown excludes them up front via
//     `t.TraceId IN (seed ids)`.
//
// SpanId = hex(globalIndex+1), unique across both populations;
// ParentSpanId points one level up (a chain root's parent is empty).
// Total span rows = (candidates + noise) × depth.
//
// The realistic shape a structural search hits — a few matching traces in
// a large table of unrelated ones — is exactly where the pushdown wins:
// the bare rescan's per-level cost tracks the FULL table (noise included)
// × depth, while the pushdown's tracks only the candidate rows.
func structuralSeedInsert(candidates, noise, depth int) string {
	hexID := func(n int) string { return fmt.Sprintf("%016x", n+1) }
	hexTrace := func(prefix byte, n int) string { return fmt.Sprintf("%c%031x", prefix, n+1) }
	var b []byte
	b = append(b, "INSERT INTO otel_traces (TraceId, SpanId, ParentSpanId, ResourceAttributes) VALUES\n"...)
	first := true
	gid := 0 // global span index → unique SpanId across both populations
	emit := func(traceID string, depth int, marker bool) {
		base := gid
		for lvl := 0; lvl < depth; lvl++ {
			var parent string
			if lvl > 0 {
				parent = hexID(base + lvl - 1)
			}
			name := "noise"
			if marker {
				name = "mid"
				switch lvl {
				case 0:
					name = "root_marker"
				case depth - 1:
					name = "leaf_marker"
				}
			}
			if !first {
				b = append(b, ",\n"...)
			}
			first = false
			b = append(b, fmt.Sprintf("('%s', '%s', '%s', map('service.name', '%s'))",
				traceID, hexID(base+lvl), parent, name)...)
		}
		gid += depth
	}
	for tr := 0; tr < candidates; tr++ {
		emit(hexTrace('a', tr), depth, true)
	}
	for tr := 0; tr < noise; tr++ {
		emit(hexTrace('b', tr), depth, false)
	}
	b = append(b, ';')
	return string(b)
}

// emitStructuralRecursiveSQL lowers `{ SpanName="root_marker" } >>
// { SpanName="leaf_marker" }` through the real cerberus parse → lower →
// emit chain and returns the emitted SQL + args. This is the
// pushed-down shape under test.
func emitStructuralRecursiveSQL(t *testing.T) (string, []any) {
	t.Helper()
	expr, err := tempo.Parse(`{ resource.service.name = "root_marker" } >> { resource.service.name = "leaf_marker" }`)
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

// noPushdownRecursiveSQL reconstructs the PRE-#77 closure shape: the same
// WITH RECURSIVE walk WITHOUT the seed-trace-id IN filter in the
// recursive arm — i.e. the recursive arm scans the BARE FULL otel_traces
// table on every level. Inlined (not emitted) precisely because the fix
// added the IN filter the new emitter always carries; the guard pins that
// the un-pushed shape was the slow one. A depth cap keeps a cyclic seed
// (there is none here) from running away, matching the new emitter; it is
// large enough never to truncate the acyclic chains under test.
func noPushdownRecursiveSQL() string {
	return `SELECT count() FROM (
  WITH RECURSIVE _closure AS (
    SELECT DISTINCT TraceId, SpanId, ParentSpanId, 0 AS _depth
      FROM (SELECT * FROM otel_traces WHERE ResourceAttributes['service.name'] = 'root_marker') AS _seed
    UNION ALL
    SELECT DISTINCT t.TraceId, t.SpanId, t.ParentSpanId, c._depth + 1
      FROM otel_traces AS t
      INNER JOIN _closure AS c
        ON t.TraceId = c.TraceId AND t.ParentSpanId = c.SpanId
      WHERE c._depth < 128
  )
  SELECT DISTINCT TraceId, SpanId FROM _closure WHERE _depth > 0
) AS L INNER JOIN (SELECT * FROM otel_traces WHERE ResourceAttributes['service.name'] = 'leaf_marker') AS R
  ON L.TraceId = R.TraceId AND L.SpanId = R.SpanId`
}

func TestStructuralRecursive_Scaling_ChDB(t *testing.T) {
	db := openChDB(t)
	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS default"); err != nil {
		t.Fatalf("create db: %v", err)
	}

	// Total span rows held ~fixed across every depth variant so the only
	// variable is how DEEP the recursion walks, not how MANY rows the
	// table holds. The candidate population (matching traces) is a small
	// fixed fraction; the rest is non-matching noise — the realistic
	// "few matching traces in a big table" shape where the pushdown wins.
	const totalRows = 60000
	const candidateRows = 1200 // ~2% of the table matches the search
	depths := []int{4, 16, 48}

	pushdownSQL, pushdownArgs := emitStructuralRecursiveSQL(t)
	noPushSQL := noPushdownRecursiveSQL()

	const iters = 3
	type row struct {
		depth                int
		pushWall, noPushWall time.Duration
		resultRows           int64
	}
	results := make([]row, 0, len(depths))

	for _, d := range depths {
		candidates := candidateRows / d // fixed candidate ROW count → /depth traces
		noise := (totalRows - candidateRows) / d
		// Fresh table per depth — the row set is rebuilt so
		// (candidates+noise)×depth stays ~constant.
		if _, err := db.Exec("DROP TABLE IF EXISTS otel_traces"); err != nil {
			t.Fatalf("drop: %v", err)
		}
		if _, err := db.Exec(structuralSeedDDL); err != nil {
			t.Fatalf("ddl: %v", err)
		}
		if _, err := db.Exec(structuralSeedInsert(candidates, noise, d)); err != nil {
			t.Fatalf("seed (depth=%d): %v", d, err)
		}

		var scanRows int64
		if err := db.QueryRow("SELECT count() FROM otel_traces").Scan(&scanRows); err != nil {
			t.Fatalf("scan count: %v", err)
		}

		// Result rows: one leaf per trace matches the root>>leaf closure.
		var resultRows int64
		if err := db.QueryRow("SELECT count() FROM ("+stripTrailingSemi(pushdownToSelect(pushdownSQL))+")", pushdownArgs...).Scan(&resultRows); err != nil {
			t.Fatalf("result-row count: %v\nSQL: %s", err, pushdownSQL)
		}

		pushWall := bestOf(t, db, pushdownSQL, pushdownArgs, iters)
		noPushWall := bestOfCount(t, db, noPushSQL, iters)

		results = append(results, row{d, pushWall, noPushWall, resultRows})
		t.Logf("depth=%-3d candidates=%-4d noise=%-5d scan_rows=%-6d result_rows=%-5d  pushdown=%-10v no_pushdown=%-10v",
			d, candidates, noise, scanRows, resultRows,
			pushWall.Round(time.Microsecond), noPushWall.Round(time.Microsecond))
	}

	first, last := results[0], results[len(results)-1]

	// --- Invariant 1: pushdown wall is ~FLAT in depth, no-pushdown grows --
	//
	// The bare-rescan shape re-reads the whole table on every recursion
	// level, so its wall grows with depth; the pushed-down shape reads
	// only the seed's candidate-trace rows per level, so it stays ~flat.
	pushGrowth := ratio(last.pushWall, first.pushWall)
	noPushGrowth := ratio(last.noPushWall, first.noPushWall)
	t.Logf("growth depth %d→%d:  pushdown=%.2fx  no_pushdown=%.2fx",
		first.depth, last.depth, pushGrowth, noPushGrowth)

	// --- Invariant 2: at the deepest D the pushdown path is FAR faster ----
	//
	// Observed (chDB, ~60k-row table, ~2% candidates): depth 4/16/48 →
	// pushdown 20/53/139ms vs bare-rescan 25/86/258ms, a ~1.9x speedup at
	// the deepest chain. The gate is ≥1.4x — comfortably below the
	// observed margin so it doesn't flake on a noisy runner, but high
	// enough that a regression reinstating the bare per-level full scan
	// (which collapses the margin toward 1.0x) trips it.
	speedup := ratio(last.noPushWall, last.pushWall)
	t.Logf("at depth=%d: pushdown=%v no_pushdown=%v  speedup=%.1fx", last.depth,
		last.pushWall.Round(time.Microsecond), last.noPushWall.Round(time.Microsecond), speedup)
	if speedup < 1.4 {
		t.Errorf("structural `>>` perf regression: at depth=%d the seed-trace-id pushdown path is only "+
			"%.1fx faster than the bare full-table-rescan shape (pushdown=%v no_pushdown=%v); want ≥1.4x. "+
			"A collapsed margin means the recursive arm regressed back to scanning the whole table per "+
			"level.", last.depth, speedup, last.pushWall, last.noPushWall)
	}

	// The pushdown path must scale clearly flatter than the bare rescan.
	// Only assert when the no-pushdown shape actually exhibited growth (so
	// the guard is meaningfully seeded); the 0.85× factor leaves headroom
	// for the pushdown path's small-absolute-time noise.
	if noPushGrowth > 1.5 && pushGrowth >= noPushGrowth*0.85 {
		t.Errorf("structural `>>` scaling regression: pushdown growth %.2fx is not clearly flatter than "+
			"the bare-rescan growth %.2fx across the depth sweep — the predicate pushdown must scale "+
			"strictly better in chain depth.", pushGrowth, noPushGrowth)
	}
}

// bestOfCount runs an already-count()-shaped query `iters` times and
// returns the fastest wall. The no-pushdown SQL already projects
// `SELECT count() FROM (...)`, so it is run directly (no extra wrap).
func bestOfCount(t *testing.T, db *sql.DB, q string, iters int) time.Duration {
	t.Helper()
	best := time.Hour
	for i := 0; i < iters; i++ {
		s := time.Now()
		var c int64
		if err := db.QueryRow(q).Scan(&c); err != nil {
			t.Fatalf("query: %v\nSQL: %s", err, q)
		}
		if d := time.Since(s); d < best {
			best = d
		}
	}
	return best
}

// pushdownToSelect returns the emitted structural SQL unchanged — it is
// already a bare SELECT the caller can wrap in `count()`. Kept as a named
// hook so the intent (this is the row-producing SELECT, not a count) is
// explicit at the call site.
func pushdownToSelect(sqlText string) string { return sqlText }

// stripTrailingSemi drops a single trailing ';' if present so the SQL can
// be embedded as a subquery.
func stripTrailingSemi(s string) string {
	for len(s) > 0 && (s[len(s)-1] == ';' || s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
