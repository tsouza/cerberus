//go:build chdb

// Package profile is the corpus-wide perf profiler (Component B of the
// perf-assessment framework).
//
// # Why this exists
//
// The Phase-1 perf audit (wf_a7e317b9) found that every perf bug
// cerberus shipped was COMPUTE FAN-OUT: the table scan read a normal
// number of rows, but an intermediate pipeline stage (a CROSS JOIN, an
// ARRAY JOIN, a range-window cross product, a recursive-CTE closure)
// exploded the row count between the scan and the final result, blowing
// up peak memory. The existing reactive guards under test/perf/ each pin
// ONE construct a human already found broken (range_lwr_scaling,
// histogram_range_scaling, setop_chain_scaling, …). They share a style
// but they only cover constructs someone remembered to register — a
// fan-out in an unregistered construct sails through.
//
// This profiler closes that gap with corpus-wide BREADTH. It walks every
// committed TXTAR fixture that is executable (declares `seed:` +
// `expected_rows:` + `sql:`), and for each one measures the fan-out
// signal directly in chDB:
//
//   - EXPLAIN PLAN actions=1 — detect the fan-out OPERATORS (CROSS JOIN,
//     ARRAY JOIN, recursive CTE) structurally, before they run.
//   - a per-subquery-level count() decomposition — peak INTERMEDIATE
//     cardinality vs the leaf SCAN row count. fan_factor =
//     peak_intermediate / scan_rows is the headline fan-out number.
//   - the chDB native per-query stats (bytes_read as the peak-memory
//     proxy; chDB's embedded engine does not expose query_log /
//     peak_memory_usage, so bytes_read is the closest available signal).
//
// It emits one [Record] per fixture. The nightly perf-profile.yml lane
// runs the whole corpus, uploads the JSON, and surfaces the highest
// fan_factor fixtures in the step summary — so a newly-introduced fan-out
// shows up as an outlier even in a construct no per-construct guard
// covers.
//
// # chDB seam reuse
//
// Seeding + SQL rewriting go through the SAME pipeline the round-trip
// assertion uses (test/spec.PrepareRoundTrip), so the SQL profiled here
// is byte-identical to the SQL CI executes. The profiler does not invent
// its own seed or its own now64/Map rewrites.
package profile

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tsouza/cerberus/internal/testsql"

	chdb "github.com/chdb-io/chdb-go/chdb"

	"github.com/tsouza/cerberus/test/spec"
)

// Record is the structured per-fixture profile emitted by [ProfileFixture].
// One JSON object per executable fixture; the nightly lane collects them
// into an array artifact and ranks by FanFactor.
type Record struct {
	// Fixture is the fixture identity as "<head>/<name>" (e.g.
	// "promql/lwr_range_rate"), derived from the path relative to
	// test/spec/.
	Fixture string `json:"fixture"`

	// ScanRows is the row count of the deepest (leaf) FROM-source
	// subquery level — the rows actually read off the seeded tables
	// before any fan-out stage. The fan-out denominator.
	ScanRows int64 `json:"scan_rows"`

	// PeakIntermediate is the maximum count() over every non-aggregating
	// FROM-source subquery level, including the outer query. The fan-out
	// numerator: the widest the row set gets anywhere in the pipeline.
	PeakIntermediate int64 `json:"peak_intermediate"`

	// FanFactor is PeakIntermediate / ScanRows (1.0 when nothing fans
	// out; >1 when an intermediate stage widened the row set; 0 when
	// ScanRows is 0 — an empty-seed or all-filtered fixture, where every
	// level WAS measured and they all counted zero rows, a degenerate
	// ratio rather than an opaque one). The headline number the nightly
	// lane ranks on. nil (JSON null) — NEVER a fabricated 1.00 —
	// whenever [UncountableLevels] is nonzero: at least one pipeline
	// stage was opaque to the per-level count() decomposition (a CTE
	// reference, a pre-rendered subquery splice, or a RECURSIVE CTE
	// step), so PeakIntermediate does not actually reflect the widest
	// the row set gets anywhere in the pipeline. A 1.00 in that state
	// reads as "no fan-out" on exactly the shapes most likely to hide
	// one — see issue #1519, where emitSetOperation's `&&` CTE arms
	// measured fan_factor=1.00 while costing 4 leaf reads.
	FanFactor *float64 `json:"fan_factor"`

	// UncountableLevels is the number of pipeline stages the profiler
	// could not see through: a WITH-prefixed subquery (CTE reference /
	// pre-rendered subquery splice) the leftmost-descent decomposition
	// refused to enter because its body's names are only in scope at its
	// own level, a RECURSIVE CTE step (detected structurally via EXPLAIN
	// PLAN, wherever in the plan it sits), or a level whose isolated
	// count() outright failed. Nonzero forces [FanFactor] to nil — see
	// that field's doc. Zero means every level the decomposition visited
	// was measured, so FanFactor (when ScanRows > 0) is trustworthy.
	UncountableLevels int `json:"uncountable_levels"`

	// UncountableReasons is one human-readable line per event counted in
	// UncountableLevels, naming the depth (where applicable) and why it
	// was opaque. Kept for debugging an unmeasured fixture — parallel to
	// Levels but for the stages excluded from it.
	UncountableReasons []string `json:"uncountable_reasons,omitempty"`

	// ResultRows is count() of the full outer query — the rows the
	// fixture's SQL ultimately returns.
	ResultRows int64 `json:"result_rows"`

	// HasCrossJoin / HasArrayJoin / HasRecursiveCTE are structural flags
	// read off EXPLAIN PLAN actions=1. A CROSS JOIN or ARRAY JOIN over a
	// non-trivial scan is the classic fan-out shape; a recursive CTE is
	// the structural-recursion shape the cycle guards pin.
	HasCrossJoin    bool `json:"has_cross_join"`
	HasArrayJoin    bool `json:"has_array_join"`
	HasRecursiveCTE bool `json:"has_recursive_cte"`

	// MaxRecursionDepth is a lower-bound estimate of the recursive-CTE
	// expansion: when the plan carries a recursive CTE, it is the
	// result-row count of the recursive level (the closure size). 0 when
	// the query has no recursive CTE.
	MaxRecursionDepth int64 `json:"max_recursion_depth"`

	// PeakBytesRead is the chDB native bytes_read stat for the full
	// query — the peak-memory proxy. chDB's embedded engine exposes
	// neither system.query_log nor peak_memory_usage through any driver
	// surface, so bytes_read (the volume the engine pulled through the
	// pipeline) stands in for peak memory. Larger = more memory pressure.
	PeakBytesRead uint64 `json:"peak_bytes_read"`

	// Levels is the per-subquery-level count() decomposition, deepest
	// first. Kept for debugging an outlier: it shows exactly which
	// pipeline stage widened the row set.
	Levels []LevelCount `json:"levels,omitempty"`

	// Err, when non-empty, records why the fixture could not be fully
	// profiled (seed failure, unrunnable SQL). The fixture still emits a
	// Record so the nightly lane can surface profiling gaps rather than
	// silently dropping them.
	Err string `json:"err,omitempty"`
}

// LevelCount is the count() of one FROM-source subquery level. Depth 0 is
// the outermost query; higher depth is deeper nesting (closer to the
// leaf scan).
type LevelCount struct {
	Depth int   `json:"depth"`
	Count int64 `json:"count"`
}

// Profiler holds a chDB session shared across fixtures. The session is a
// process-global singleton in chdb-go, so the profiler seeds each
// fixture with CREATE OR REPLACE TABLE (via spec.PromoteCreateTable) to
// stay idempotent across fixtures that reuse table names.
type Profiler struct {
	sess *chdb.Session
}

// experimentalTSGridSetting is the canonical ClickHouse setting that
// gates the experimental timeSeries*ToGrid aggregate family. The profiler
// EXPLAINs + count()s every executable fixture's SQL, including the
// native-rate fixture whose SQL emits timeSeriesRateToGrid — without the
// gate enabled on the session, that fixture's EXPLAIN errors with Code 63
// (experimental-and-disabled), so the profiler must enable it session-wide
// exactly as the round-trip lane does (test/spec/runner_chdb.go). It is
// harmless for every other fixture: it gates only those aggregates, which
// no other fixture emits. Kept in lock-step with the canonical spelling in
// chclient.SettingExperimentalTSGridAggregate (ClickHouse PR #80590 renamed
// the gate from `..._ts_to_grid_aggregate_function` before the v25.6
// release; the old name survives only as an alias).
const experimentalTSGridSetting = "allow_experimental_time_series_aggregate_functions"

// NewProfiler opens a fresh ephemeral chDB session. Caller must Close.
func NewProfiler() (*Profiler, error) {
	sess, err := chdb.NewSession("")
	if err != nil {
		return nil, fmt.Errorf("open chdb session: %w", err)
	}
	p := &Profiler{sess: sess}
	// Enable the experimental timeSeries*ToGrid gate so the native-rate
	// fixture's SQL EXPLAINs + counts instead of erroring (Code 63). The
	// chDB substrate is 25.8, well past the v25.6 floor where the family
	// (and the canonical setting) landed.
	if err := p.exec("SET " + experimentalTSGridSetting + " = 1"); err != nil {
		p.Close()
		return nil, fmt.Errorf("enable experimental ts-grid aggregate: %w", err)
	}
	return p, nil
}

// Close tears down the session and its temp dir.
func (p *Profiler) Close() {
	if p.sess != nil {
		p.sess.Cleanup()
		p.sess = nil
	}
}

// exec runs a statement and surfaces any chDB-side error. CSV output is
// used (cheapest) since exec callers ignore the result body.
func (p *Profiler) exec(stmt string) error {
	res, err := p.sess.Query(stmt, "CSV")
	if err != nil {
		return err
	}
	if res != nil {
		if e := res.Error(); e != nil {
			return e
		}
	}
	return nil
}

// scalarCount runs `SELECT count() FROM (<inner>)` and returns the count
// plus the native bytes_read stat for that execution.
func (p *Profiler) scalarCount(inner string) (count int64, bytesRead uint64, err error) {
	q := "SELECT count() FROM (" + inner + ")"
	res, err := p.sess.Query(q, "JSON")
	if err != nil {
		return 0, 0, err
	}
	if e := res.Error(); e != nil {
		return 0, 0, e
	}
	n, perr := parseSingleCount(res.String())
	if perr != nil {
		return 0, 0, perr
	}
	return n, res.BytesRead(), nil
}

// explainPlan returns the EXPLAIN PLAN actions=1 text for query.
func (p *Profiler) explainPlan(query string) (string, error) {
	res, err := p.sess.Query("EXPLAIN PLAN actions=1 "+query, "CSV")
	if err != nil {
		return "", err
	}
	if e := res.Error(); e != nil {
		return "", e
	}
	return res.String(), nil
}

// ProfileFixture seeds the fixture's tables and measures its fan-out
// signal. It always returns a Record: on a seed/exec error the Record
// carries Err set and whatever partial signal was collected, so the
// nightly lane can report profiling gaps instead of dropping fixtures.
//
// fixtureID is the "<head>/<name>" identity used in Record.Fixture.
func (p *Profiler) ProfileFixture(fixtureID string, prep *spec.PreparedRoundTrip) Record {
	rec := Record{Fixture: fixtureID}

	// Seed: split + promote-to-OR-REPLACE exactly as applySeed does, so
	// re-seeding across fixtures that share a table name is idempotent.
	for _, stmt := range testsql.SplitStatements(prep.Seed) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		stmt = testsql.PromoteCreateTable(stmt)
		if err := p.exec(stmt); err != nil {
			rec.Err = fmt.Sprintf("seed: %v", err)
			return rec
		}
	}

	// The fixture SQL carries `?` placeholders bound positionally in
	// prep.Args. chDB's session API has no placeholder binding, so we
	// inline the args into the SQL textually (literal substitution) for
	// profiling. The substituted SQL is semantically identical for the
	// purpose of plan shape + row counts.
	query := inlineArgs(prep.Query, prep.Args)

	// EXPLAIN PLAN actions=1 — structural fan-out operators.
	if plan, err := p.explainPlan(query); err == nil {
		rec.HasCrossJoin = planHasCrossJoin(plan)
		rec.HasArrayJoin = planHasArrayJoin(plan)
		rec.HasRecursiveCTE = planHasRecursiveCTE(plan)
	} else {
		rec.Err = fmt.Sprintf("explain: %v", err)
		// Continue: the count decomposition may still run.
	}

	// Per-level count() decomposition over FROM-source subqueries.
	// levelsWithReasons additionally reports every point the descent
	// stopped on a non-recursive WITH-prefixed subquery it could not see
	// through — see that function's doc for why a RECURSIVE CTE is
	// handled separately, below, from the structural EXPLAIN flag
	// instead.
	levels, descentReasons := levelsWithReasons(query)
	rec.UncountableReasons = append(rec.UncountableReasons, descentReasons...)
	if rec.HasRecursiveCTE {
		rec.UncountableReasons = append(rec.UncountableReasons, recursiveCTEUncountableReason)
	}

	rec.Levels = make([]LevelCount, 0, len(levels))
	var peak int64
	var peakBytes uint64
	for depth, lvl := range levels {
		c, br, err := p.scalarCount(lvl)
		if err != nil {
			// A level that can't be counted in isolation (e.g. it
			// references a name only in scope at an outer level) is
			// excluded from Levels AND recorded as uncountable — the
			// outer-query count at depth 0 still anchors the result, and
			// the leaf scan still anchors scan_rows, but this pipeline
			// stage's contribution to PeakIntermediate is unknown rather
			// than silently treated as zero.
			rec.UncountableReasons = append(rec.UncountableReasons,
				fmt.Sprintf("depth %d: count() failed in isolation: %v", depth, err))
			continue
		}
		rec.Levels = append(rec.Levels, LevelCount{Depth: depth, Count: c})
		if c > peak {
			peak = c
		}
		if br > peakBytes {
			peakBytes = br
		}
	}
	rec.PeakIntermediate = peak
	rec.PeakBytesRead = peakBytes

	// scan_rows is the deepest level we could count (the leaf FROM
	// source). result_rows is the outermost (depth 0).
	if len(rec.Levels) > 0 {
		rec.ScanRows = rec.Levels[len(rec.Levels)-1].Count
		rec.ResultRows = rec.Levels[0].Count
	}

	rec.UncountableLevels = len(rec.UncountableReasons)
	// FanFactor stays nil whenever any level was opaque: a fabricated
	// 1.00 computed from a partial decomposition is worse than an
	// admitted unknown. See the field doc + issue #1519 part 2. This is
	// orthogonal to the ScanRows == 0 case (an empty-result fixture, e.g.
	// an `absent_over_time` guard with no matching series): that ratio is
	// mathematically degenerate rather than opaque — every level WAS
	// measured, they just all counted zero rows — so it is still reported
	// as a measured 0, matching the pre-#1519p2 convention.
	if rec.UncountableLevels == 0 {
		var ff float64
		if rec.ScanRows > 0 {
			ff = float64(rec.PeakIntermediate) / float64(rec.ScanRows)
		}
		rec.FanFactor = &ff
	}

	// max_recursion_depth: for a recursive CTE the closure size is the
	// best available expansion signal (result_rows of the recursion).
	if rec.HasRecursiveCTE {
		rec.MaxRecursionDepth = rec.ResultRows
	}

	return rec
}

// parseSingleCount pulls the single count() value out of a chDB JSON
// result body shaped `{"data":[{"count()":N}], ...}`.
func parseSingleCount(jsonBody string) (int64, error) {
	var parsed struct {
		Data []map[string]json.Number `json:"data"`
	}
	dec := json.NewDecoder(strings.NewReader(jsonBody))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return 0, fmt.Errorf("decode count json: %w", err)
	}
	if len(parsed.Data) == 0 {
		return 0, nil
	}
	for _, v := range parsed.Data[0] {
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("count not an int: %w", err)
		}
		return n, nil
	}
	return 0, nil
}

// SortByFanFactor orders records descending by FanFactor (then by
// PeakIntermediate, then fixture name) so the nightly step-summary lists
// the worst fan-outs first. A nil FanFactor (unmeasured — see that
// field's doc) sorts before every measured value: an unknown fan-out is
// at least as much of a risk signal as a known-bad one, so it must not
// silently sink to the bottom of the list.
func SortByFanFactor(recs []Record) {
	sort.Slice(recs, func(i, j int) bool {
		fi, fj := recs[i].FanFactor, recs[j].FanFactor
		if (fi == nil) != (fj == nil) {
			return fi == nil
		}
		if fi != nil && fj != nil && *fi != *fj {
			return *fi > *fj
		}
		if recs[i].PeakIntermediate != recs[j].PeakIntermediate {
			return recs[i].PeakIntermediate > recs[j].PeakIntermediate
		}
		return recs[i].Fixture < recs[j].Fixture
	})
}
