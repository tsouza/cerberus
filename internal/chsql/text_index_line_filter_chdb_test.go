//go:build chdb

// Real-ClickHouse (chDB) proof that chplan.LineContent.TextIndexPrefilter
// (cerberus issue #2773, chopt text_index_line_filter) is:
//
//  1. Result-equivalent: the rewritten (prefiltered) predicate returns the
//     EXACT SAME row set as the unrewritten one, over a real text index, for
//     a plain multi-word |= literal, a pure-literal regex, and a literal
//     containing LIKE metacharacters (%/_) that must be escaped correctly.
//  2. Actually index-accelerated: EXPLAIN indexes=1 against a table
//     carrying the idx_lower_body text index (the exact shape
//     internal/schema/ddl.renderLogsTable emits when TextIndexEnabled=true)
//     shows the prefiltered form pruning granules the unprefiltered form
//     does not.
//  3. Measurably faster end-to-end on production-shaped synthetic data — see
//     TestTextIndexLineFilterPrefilter_Latency_ChDB, which logs the
//     before/after wall-clock numbers this PR's body reports.
//
// The seed corpus is SYNTHETIC (no real production log-Body sample exists
// under test/perf/ — its committed LFS corpus is metrics-shaped only), the
// same honestly-labeled posture cerberus issue #2764/#2767's own chDB perf
// proofs took on their new floors.
package chsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// tilfTotalRows / tilfNeedleEvery size the synthetic logs corpus: a body
// column of realistic multi-word log lines, with the target literal
// ("connection reset by peer") appended to every tilfNeedleEvery-th row —
// selective enough (0.05%) that a real skip index has something to prune.
const (
	// 5M rows (~450MB compressed body text): the smallest scale at which
	// the granule-pruning win reliably shows up as a real wall-clock
	// improvement in this test's own measurements — see
	// TestTextIndexLineFilterPrefilter_Latency_ChDB's doc comment.
	// Measured directly: at 500K rows the LIKE-conjunct evaluation
	// overhead roughly offsets the pruning win (no clear improvement);
	// scaling to 5M is what makes not-reading-the-pruned-granules'
	// compressed bytes actually outweigh evaluating a few extra LIKE
	// predicates per row.
	tilfTotalRows   = 5_000_000
	tilfNeedleEvery = 2_000
)

// tilfTableDDL is a TRIMMED production-shaped logs table: only the
// Timestamp/Body columns internal/schema/ddl.renderLogsTable's Logs
// template carries, plus the EXACT idx_lower_body index shape that
// function emits when TextIndexEnabled=true (TYPE text(tokenizer =
// 'splitByNonAlpha') over lower(Body), no explicit GRANULARITY — see
// bodyTextIndexGranularity's doc comment in internal/schema/ddl for why
// that constant reproduces ClickHouse's own implicit default rather than
// this DDL needing one).
const tilfTableDDL = `CREATE OR REPLACE TABLE tilf_logs (
    Timestamp DateTime64(9),
    Body String,
    INDEX idx_lower_body lower(Body) TYPE text(tokenizer = 'splitByNonAlpha')
) ENGINE = MergeTree()
ORDER BY Timestamp
SETTINGS index_granularity = 256;`

// tilfVocabWords is the fixed vocabulary synthetic log lines are drawn
// from — deliberately log-shaped (service/infra terms), not English prose.
var tilfVocabWords = []string{
	"starting", "service", "handler", "request", "completed", "timeout",
	"retry", "upstream", "cache", "miss", "hit", "auth", "token", "expired",
	"refresh", "database", "query", "slow", "index", "scan", "user",
	"session", "closed", "gc", "pause", "thread", "pool", "exhausted",
	"queue", "depth",
}

// tilfNeedle is the multi-word |= literal the correctness + pruning +
// latency tests all filter on. "by" (2 runes) falls below
// textIndexLikeMinTokenLength and is dropped from the prefilter — the
// query still runs correctly on the other three ANDed conjuncts.
const tilfNeedle = "connection reset by peer"

// tilfSeedInsert seeds tilfTotalRows rows: each a pseudo-random slice of
// tilfVocabWords (via a deterministic arrayMap/rand seeded by `number`),
// with tilfNeedle appended to every tilfNeedleEvery-th row.
func tilfSeedInsert() string {
	vocab := "["
	for i, w := range tilfVocabWords {
		if i > 0 {
			vocab += ", "
		}
		vocab += "'" + w + "'"
	}
	vocab += "]"
	return fmt.Sprintf(`INSERT INTO tilf_logs
SELECT
    toDateTime64('2026-01-01 00:00:00', 9) + INTERVAL number MICROSECOND AS Timestamp,
    arrayStringConcat(
        arrayMap(i -> %s[1 + (cityHash64(number, i) %% %d)], range(6 + (number %% 8))),
        ' '
    ) || if(number %% %d = 0, ' %s', '') AS Body
FROM numbers(%d);`, vocab, len(tilfVocabWords), tilfNeedleEvery, tilfNeedle, tilfTotalRows)
}

// tilfSeed opens an isolated chDB session (chsqltest.OpenIsolatedChDB — a
// direct connection open would inherit whatever database/tables a
// PREVIOUS test in this process left behind, since chdb-go caches one
// session per process; see test/regression/chsql_chdb_isolation_test.go,
// issue #2074) and seeds tilf_logs. Returns the *sql.DB and the exact
// count of needle-bearing rows seeded, computed independently via `LIKE`
// over the raw generated Body (not via the code under test), so the
// correctness tests below have a ground truth that does not share any
// code path with what they verify.
func tilfSeed(t *testing.T) (*sql.DB, int) {
	t.Helper()
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(tilfTableDDL); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(tilfSeedInsert()); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	var ground int
	if err := db.QueryRow(`SELECT count() FROM tilf_logs WHERE position(Body, ?) > 0`, tilfNeedle).Scan(&ground); err != nil {
		t.Fatalf("ground-truth count: %v", err)
	}
	if ground == 0 {
		t.Fatalf("seed produced 0 needle rows — seed is broken")
	}
	return db, ground
}

// lineContentSQL emits the SQL + args chsql.Emit renders for a
// chplan.Filter{Predicate: *chplan.LineContent} over tilf_logs — the SAME
// production emitter path a LogQL |= / |~ query goes through, not a
// hand-written query, so this test exercises the real rewrite.
func lineContentSQL(t *testing.T, lc *chplan.LineContent) (string, []any) {
	t.Helper()
	plan := &chplan.Filter{
		Input:     &chplan.Scan{Table: "tilf_logs"},
		Predicate: lc,
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}
	return sqlStr, args
}

// tilfRowIDs runs sqlStr (a SELECT * shape) and returns the sorted set of
// Timestamp values matched, as a comparable fingerprint of the result set.
func tilfRowIDs(t *testing.T, db *sql.DB, sqlStr string, args []any) []string {
	t.Helper()
	rows, err := db.Query(sqlStr, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, sqlStr)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	tsIdx := -1
	for i, c := range cols {
		if c == "Timestamp" {
			tsIdx = i
		}
	}
	if tsIdx < 0 {
		t.Fatalf("result has no Timestamp column: %v", cols)
	}
	var out []string
	for rows.Next() {
		dest := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, fmt.Sprintf("%v", dest[tsIdx]))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(out)
	return out
}

// TestTextIndexLineFilterPrefilter_ResultEquivalence_ChDB is the "real
// EXPLAIN/execution test proving the rewritten form still returns identical
// result sets to the original" the issue explicitly calls for — not just
// that the rewrite "looks right". Three shapes: a plain multi-word |=
// literal, a pure-literal regex (|~ with no RE2 metacharacters), and a
// literal containing % / _ (the dedicated LIKE-escaping case).
func TestTextIndexLineFilterPrefilter_ResultEquivalence_ChDB(t *testing.T) {
	db, ground := tilfSeed(t)
	t.Logf("seeded %d rows, %d ground-truth needle matches", tilfTotalRows, ground)

	cases := []struct {
		name    string
		pattern string
		isRegex bool
	}{
		{"literal", tilfNeedle, false},
		{"pure_literal_regex", tilfNeedle, true},
		{"like_metachar_escaping", "user_id=50 at 92% done", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			before := &chplan.LineContent{
				Source: &chplan.ColumnRef{Name: "Body"}, Pattern: tt.pattern, IsRegex: tt.isRegex,
			}
			after := &chplan.LineContent{
				Source: &chplan.ColumnRef{Name: "Body"}, Pattern: tt.pattern, IsRegex: tt.isRegex,
				TextIndexPrefilter: true,
			}
			beforeSQL, beforeArgs := lineContentSQL(t, before)
			afterSQL, afterArgs := lineContentSQL(t, after)
			if beforeSQL == afterSQL {
				t.Fatalf("expected the prefiltered SQL to differ from the unprefiltered form; both rendered:\n%s", beforeSQL)
			}
			beforeRows := tilfRowIDs(t, db, beforeSQL, beforeArgs)
			afterRows := tilfRowIDs(t, db, afterSQL, afterArgs)
			if len(beforeRows) != len(afterRows) {
				t.Fatalf("%s: row count mismatch: before=%d after=%d", tt.name, len(beforeRows), len(afterRows))
			}
			for i := range beforeRows {
				if beforeRows[i] != afterRows[i] {
					t.Fatalf("%s: result sets diverge at index %d: before=%s after=%s", tt.name, i, beforeRows[i], afterRows[i])
				}
			}
		})
	}
}

// TestTextIndexLineFilterPrefilter_PrunesGranules_ChDB proves the
// prefiltered form actually prunes MergeTree granules via EXPLAIN
// indexes=1, over the exact idx_lower_body text-index shape
// internal/schema/ddl.renderLogsTable emits when TextIndexEnabled=true —
// the DDL half of cerberus issue #2773 — not a synthetic index this test
// invented for itself.
func TestTextIndexLineFilterPrefilter_PrunesGranules_ChDB(t *testing.T) {
	db, _ := tilfSeed(t)

	explainGranules := func(sqlStr string, args []any) (total, read int) {
		t.Helper()
		rows, err := db.Query("EXPLAIN indexes=1 "+sqlStr, args...)
		if err != nil {
			t.Fatalf("EXPLAIN: %v\nSQL: %s", err, sqlStr)
		}
		defer rows.Close()
		// EXPLAIN indexes=1 prints one "Granules: n/m" line per index stage
		// (PrimaryKey first, then Skip when a skip index applies) — take the
		// LAST one, which is the Skip stage's when a skip index engages, and
		// the (only) PrimaryKey stage's when none does. Taking the FIRST
		// match instead would always read the PrimaryKey line and silently
		// miss whether the skip index pruned anything.
		found := false
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan explain line: %v", err)
			}
			var r, tot int
			if n, err := fmt.Sscanf(line, "            Granules: %d/%d", &r, &tot); err == nil && n == 2 {
				read, total = r, tot
				found = true
			}
		}
		if !found {
			t.Fatalf("EXPLAIN output carried no 'Granules: n/m' line:\nSQL: %s", sqlStr)
		}
		return total, read
	}

	before := &chplan.LineContent{Source: &chplan.ColumnRef{Name: "Body"}, Pattern: tilfNeedle}
	after := &chplan.LineContent{Source: &chplan.ColumnRef{Name: "Body"}, Pattern: tilfNeedle, TextIndexPrefilter: true}
	beforeSQL, beforeArgs := lineContentSQL(t, before)
	afterSQL, afterArgs := lineContentSQL(t, after)

	totalBefore, readBefore := explainGranules(beforeSQL, beforeArgs)
	totalAfter, readAfter := explainGranules(afterSQL, afterArgs)

	t.Logf("before (no prefilter): %d/%d granules read", readBefore, totalBefore)
	t.Logf("after  (prefiltered):  %d/%d granules read", readAfter, totalAfter)

	if readBefore != totalBefore {
		t.Errorf("expected the UNPREFILTERED position()-only form to read every granule (no skip index it can use), got %d/%d", readBefore, totalBefore)
	}
	if readAfter >= readBefore {
		t.Errorf("expected the prefiltered form to read FEWER granules than the unprefiltered form: before=%d after=%d", readBefore, readAfter)
	}
}

// TestTextIndexLineFilterPrefilter_Latency_ChDB measures real wall-clock
// latency before vs after over the synthetic corpus, logging the numbers
// this PR's description reports. Not a hard pass/fail gate on absolute
// timing (chDB's runtime characteristics vary too much across the CI fleet
// for a numeric threshold to be reliable — see docs/test-strategy.md's
// chDB-vs-real-CH guidance elsewhere in this repo) —
// TestTextIndexLineFilterPrefilter_PrunesGranules_ChDB above is the
// structural (version-independent) regression guard; this test's job is to
// produce the honest measured numbers.
//
// use_query_condition_cache=0 is REQUIRED on every measured query here, not
// cosmetic: ClickHouse's query condition cache (chopt condition_cache,
// already auto-enabled by cerberus at server >= 25.3 — see
// chopt.FeatureConditionCache) remembers, per granule, whether a
// semantically-identical WHERE condition matched on a PREVIOUS execution,
// and skips re-reading a granule it already knows cannot match. That cache
// is completely independent of this feature's own text index — verified
// live: with the condition cache warm, `enable_full_text_index=0` (the
// text index switched OFF entirely) still read the exact same reduced row
// count as with it on, and the FIRST-ever execution of a novel predicate
// always full-scans even with the text index present. Left enabled, a
// naive best-of-N loop over the SAME predicate would measure the SECOND
// run's cache-warm speed for BOTH the before and after form, drowning out
// this feature's own contribution entirely (a real methodological trap
// this test fell into during development — the first attempt showed NO
// improvement, or a slight regression, purely because both forms were
// hitting a warm condition cache). Disabling it isolates the ONE
// mechanism this test exists to measure: the text-index-driven granule
// prefilter, on the realistic "first time a user searches for this exact
// literal" cold-cache path ad-hoc LogQL search overwhelmingly is.
func TestTextIndexLineFilterPrefilter_Latency_ChDB(t *testing.T) {
	db, ground := tilfSeed(t)

	before := &chplan.LineContent{Source: &chplan.ColumnRef{Name: "Body"}, Pattern: tilfNeedle}
	after := &chplan.LineContent{Source: &chplan.ColumnRef{Name: "Body"}, Pattern: tilfNeedle, TextIndexPrefilter: true}
	beforeSQL, beforeArgs := lineContentSQL(t, before)
	afterSQL, afterArgs := lineContentSQL(t, after)
	const disableConditionCache = " SETTINGS use_query_condition_cache = 0"
	beforeSQL += disableConditionCache
	afterSQL += disableConditionCache

	const runs = 5
	timeIt := func(sqlStr string, args []any) time.Duration {
		best := time.Duration(1<<63 - 1)
		for i := 0; i < runs; i++ {
			start := time.Now()
			rows, err := db.Query(sqlStr, args...)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			n := 0
			for rows.Next() {
				n++
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			rows.Close()
			if n != ground {
				t.Fatalf("row count drifted mid-benchmark: got %d, want %d", n, ground)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}

	// Interleave rather than running all "before" repeats then all "after"
	// repeats, so a shared confound drifting over the test's lifetime (GC,
	// thermal throttling, a noisy CI neighbor) hits both forms evenly
	// instead of biasing whichever form runs second.
	var beforeBest, afterBest time.Duration
	for i := 0; i < 3; i++ {
		if b := timeIt(beforeSQL, beforeArgs); beforeBest == 0 || b < beforeBest {
			beforeBest = b
		}
		if a := timeIt(afterSQL, afterArgs); afterBest == 0 || a < afterBest {
			afterBest = a
		}
	}

	t.Logf("TextIndexLineFilter latency (condition cache disabled), %d rows, best-of-%d: before=%s after=%s (%.2fx)",
		tilfTotalRows, runs*3, beforeBest, afterBest, float64(beforeBest)/float64(afterBest))
}
