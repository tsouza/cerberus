package promql

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// Default-lane pins for cerberus issue #2733: where the mixed float/histogram
// subquery composition stops being serveable, and what a query past that point
// is told.
//
// The composition's emitted SQL grows ~4x per bracket level, because each
// Mixed FOLD-family level partitions its input through
// [splitMixedRelByDiscriminator] and so names the relation beneath it twice.
// Measured over `((m_exp_hist) or (m_gauge))` at an instant eval: 26KB at one
// level, 140KB at two, 588KB at three, 1.8MB at four. Levels one and two are
// what cerberus issues #2726 and #2728 answer, and both are chDB-proven in
// histogram_native_subquery_call_subquery_chdb_test.go /
// …_outer_fn_chdb_test.go. Level three is past ClickHouse's own
// max_query_size, so the server refuses to parse it — and before this bound
// existed that refusal reached the user as a raw driver `Code: 62 … failed at
// position 262145`.
//
// These tests pin both halves of the fix at once, and deliberately in one
// file: the SEPARATION is the whole claim. A bound that admitted level three
// would not fix anything, and one that refused level two would break a
// shipping, chDB-proven shape — the far worse of the two failures.

const (
	// emitSizeInnerRange / emitSizeMidRange / emitSizeOuterRange / emitSizeTopRange
	// are the four brackets the queries below stack, innermost first.
	emitSizeInnerRange = "[2m:1m]"
	emitSizeMidRange   = "[3m:1m]"
	emitSizeOuterRange = "[4m:1m]"
	emitSizeTopRange   = "[5m:1m]"
)

// emitSizeLevel1 … emitSizeLevel4 are the four bracket depths issue #2733
// measured, over the same mixed `or` the #2728 default-lane tests use.
func emitSizeLevel1() string {
	return "rate(" + outerFn2TestMixedInner + emitSizeMidRange + ")"
}

func emitSizeLevel2() string {
	// Identical to outerFn2TestQuery("sum_over_time", …) — spelled through it
	// so the shape this admits stays the shape #2728's own tests exercise.
	return outerFn2TestQuery("sum_over_time", outerFn2TestMixedInner, "")
}

func emitSizeLevel3() string {
	return "sum_over_time(rate(rate(" + outerFn2TestMixedInner + emitSizeInnerRange + ")" +
		emitSizeMidRange + ")" + emitSizeOuterRange + ")"
}

func emitSizeLevel4() string {
	return "rate(" + emitSizeLevel3() + emitSizeTopRange + ")"
}

// emitSizeLowerAndEmit lowers q at a fixed anchor — instant, or over a
// query_range grid — and emits it, returning whatever emit produced. Lowering
// itself is asserted to succeed in every case: the bound this file pins sits at
// the EMIT chokepoint, so a query it refuses must still lower cleanly, and a
// lowering-time failure here would mean the test is measuring something else.
func emitSizeLowerAndEmit(t *testing.T, q string, rangeMode bool) (string, error) {
	t.Helper()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	expr := mustParseExperimental(t, q)

	var plan chplan.Node
	var err error
	if rangeMode {
		plan, err = LowerAtRange(context.Background(), expr, s, at.Add(-10*time.Minute), at, time.Minute)
	} else {
		plan, err = LowerAt(context.Background(), expr, s, at, at)
	}
	if err != nil {
		t.Fatalf("lower %s: %v", q, err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	return sql, err
}

// TestMixedSubqueryComposition_LevelsOneAndTwoStillEmit is the regression half
// of issue #2733's fix, and the more important half: the two composition
// depths cerberus already answers must keep emitting. Level two under
// query_range renders 209,182 bytes — 80% of the ceiling — so this is not a
// theoretical margin, and a bound placed on any estimate that OVER-counts the
// wire size (metadata.go's deliberately conservative boundQueryBytes, say)
// would refuse it outright.
func TestMixedSubqueryComposition_LevelsOneAndTwoStillEmit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		q    string
	}{
		{"level 1", emitSizeLevel1()},
		{"level 2", emitSizeLevel2()},
	} {
		for _, rangeMode := range []bool{false, true} {
			name := tc.name + "/instant"
			if rangeMode {
				name = tc.name + "/query_range"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				sql, err := emitSizeLowerAndEmit(t, tc.q, rangeMode)
				if err != nil {
					t.Fatalf("%s no longer emits: %v\n\nThis shape is chDB-proven "+
						"(cerberus issues #2726 / #2728) and shipping. If the "+
						"emitted-SQL size bound refused it, the bound is wrong; if the "+
						"composition grew past ClickHouse's max_query_size, the "+
						"emitter regressed and the query is now unserveable.", tc.q, err)
				}
				t.Logf("%s: %d bytes of SQL", tc.q, len(sql))
			})
		}
	}
}

// emitSizeReportedBytes pulls the rejection message's own two figures — the
// bytes the statement rendered to, and the ceiling it passed — out of it, so
// the assertions below compare the guard against its own reported bound rather
// than against a second copy of ClickHouse's max_query_size default.
var emitSizeReportedBytes = regexp.MustCompile(`renders at least (\d+) bytes, past the (\d+)-byte ceiling`)

// TestMixedSubqueryComposition_LevelThreeAndDeeperRejectedCleanly pins the
// other half: the depths ClickHouse will not parse are refused by cerberus, by
// name, before any statement reaches a querier.
func TestMixedSubqueryComposition_LevelThreeAndDeeperRejectedCleanly(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		q          string
		wantLevels string
	}{
		{"level 3", emitSizeLevel3(), "a plan stacking 4 range-vector levels over a mixed float/histogram relation"},
		{"level 4", emitSizeLevel4(), "a plan stacking 5 range-vector levels over a mixed float/histogram relation"},
	} {
		for _, rangeMode := range []bool{false, true} {
			name := tc.name + "/instant"
			if rangeMode {
				name = tc.name + "/query_range"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				sql, err := emitSizeLowerAndEmit(t, tc.q, rangeMode)
				if err == nil {
					t.Fatalf("%s emitted %d bytes of SQL with no error — ClickHouse would "+
						"refuse to parse it (max_query_size), so the user gets a raw driver "+
						"code 62 instead of a cerberus rejection naming the shape", tc.q, len(sql))
				}
				if !errors.Is(err, chsql.ErrEmittedSQLTooLarge) {
					t.Fatalf("%s was refused by something other than the emitted-SQL size bound: %v", tc.q, err)
				}
				if sql != "" {
					t.Errorf("a refused emit returned %d bytes of SQL; it must return none", len(sql))
				}
				msg := err.Error()
				if !strings.Contains(msg, tc.wantLevels) {
					t.Errorf("rejection does not name the composition.\n got: %s\nwant it to contain: %s", msg, tc.wantLevels)
				}
			})
		}
	}
}

// TestMixedSubqueryComposition_RejectionStopsAtTheFirstOversizeSubStatement
// pins the emitter's per-sub-statement check (renderNode), which is what keeps
// a rejection CHEAP. The four-level composition's finished statement is ~1.8MB
// (~3.5MB under query_range) and each further bracket multiplies that by ~4, so
// a guard that only measured the finished statement would build the whole thing
// before refusing it — ~45MB at six levels, ~700MB at eight, all from a query
// short enough to type.
//
// The rejection's own byte figure is the observable: it reports the sub-
// statement that crossed the ceiling, which for this shape is ~290KB — barely
// over the bound, and an order of magnitude below the finished statement the
// late-only guard would have had to render first.
func TestMixedSubqueryComposition_RejectionStopsAtTheFirstOversizeSubStatement(t *testing.T) {
	t.Parallel()

	_, err := emitSizeLowerAndEmit(t, emitSizeLevel4(), true)
	if err == nil {
		t.Fatal("the four-level composition emitted with no error")
	}
	m := emitSizeReportedBytes.FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("rejection carries no byte figure to read: %v", err)
	}
	reported, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		t.Fatalf("byte figure %q is not a number: %v", m[1], convErr)
	}
	ceiling, convErr := strconv.Atoi(m[2])
	if convErr != nil {
		t.Fatalf("ceiling figure %q is not a number: %v", m[2], convErr)
	}

	// The statement that tripped the bound must genuinely be over it, or the
	// figure is measuring something other than what was refused.
	if reported <= ceiling {
		t.Errorf("the rejection reports %d bytes, which is within its own %d-byte ceiling", reported, ceiling)
	}
	// The finished query_range statement for this shape is ~3.5MB — more than
	// four times the ceiling. Anything near that means the emitter rendered the
	// whole composition before refusing it, i.e. the per-sub-statement check no
	// longer fires. The observed figure is ~290KB, barely over the ceiling.
	const stoppedEarlyCeilingMultiple = 4
	if limit := ceiling * stoppedEarlyCeilingMultiple; reported >= limit {
		t.Errorf("the rejection reports %d bytes, at or past %dx the %d-byte ceiling — the emitter "+
			"rendered the whole composition before refusing it instead of stopping at the first "+
			"over-bound sub-statement", reported, stoppedEarlyCeilingMultiple, ceiling)
	}
}
