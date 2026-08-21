//go:build chdb

// The package doc comment lives in doc.go, which carries no build tag so it
// stays visible to `go doc` even when this file — the chDB-querying half of
// the package — is excluded from the build.
package profile

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tsouza/cerberus/internal/testsql"

	chdb "github.com/chdb-io/chdb-go/chdb"

	"github.com/tsouza/cerberus/test/spec"
)

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
	decomposition := decomposeQuery(query, 0)
	rec.UncountableReasons = append(rec.UncountableReasons, decomposition.reasons...)
	if rec.HasRecursiveCTE {
		rec.UncountableReasons = append(rec.UncountableReasons, recursiveCTEUncountableReason)
	}

	rec.Levels = make([]LevelCount, 0, len(decomposition.levels))
	var peak int64
	var peakBytes uint64
	for _, level := range decomposition.levels {
		c, br, err := p.scalarCount(level.query)
		if err != nil {
			// A level that can't be counted in isolation (e.g. it
			// references a name only in scope at an outer level) is
			// excluded from Levels AND recorded as uncountable — the
			// outer-query count at depth 0 still anchors the result, and
			// the leaf scan still anchors scan_rows, but this pipeline
			// stage's contribution to PeakIntermediate is unknown rather
			// than silently treated as zero.
			rec.UncountableReasons = append(rec.UncountableReasons,
				fmt.Sprintf("depth %d: count() failed in isolation: %v", level.depth, err))
			continue
		}
		rec.Levels = append(rec.Levels, LevelCount{Depth: level.depth, Count: c})
		if level.depth == 0 {
			rec.ResultRows = c
		}
		if level.scanSource {
			rec.ScanRows += c
		}
		if c > peak {
			peak = c
		}
		if br > peakBytes {
			peakBytes = br
		}
	}
	rec.PeakIntermediate = peak
	rec.PeakBytesRead = peakBytes

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
