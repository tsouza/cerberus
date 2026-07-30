package promql_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

// The deferred-label-shaping hoist is only allowed to change the SHAPE of the
// emitted SQL, never the rows it returns. Two fixtures pin that: one lowers
// rate() with the hoist on, its twin lowers the same query over the same seed
// with the hoist off, and both record the rows chDB actually produced. The
// claim "the hoist preserves semantics" is exactly the claim that those two
// `expected_rows` sections agree.
//
// Nothing else compares them. Each fixture is matched only against its own
// golden, and `just update-golden` rewrites both halves in one pass — so a
// hoist that computes a subtly different number regenerates both sides into
// fresh, mutually-consistent agreement and stays green. This test is the
// missing comparison: it runs in the default (non-chDB) lane, reads the two
// files off disk, and fails on any divergence between them.
const (
	recollapseHoistedFixture  = "native_rate_recollapse_collision.txtar"
	recollapseTwoLevelFixture = "native_rate_recollapse_collision_two_level.txtar"

	// recollapseDirectiveSection is the fixture section that turns the hoist
	// on — present in the hoisted half, absent in the twin. Without that
	// asymmetry the pair would compare one lowering against itself.
	recollapseDirectiveSection = "experimental_ts_grid_recollapse"

	expectedRowsSection = "expected_rows"
)

// TestNativeRateRecollapseFixturePairPreservesRows asserts the hoisted and
// un-hoisted halves of the non-injective-shaping pair describe the same
// experiment (same query, same step, same seed, differing only in the hoist
// directive) and agree cell-for-cell on the rows it produces.
//
// A divergence here means the hoist is wrong. Do NOT reconcile it by
// regenerating whichever golden moved.
func TestNativeRateRecollapseFixturePairPreservesRows(t *testing.T) {
	t.Parallel()

	hoisted := loadRecollapsePairHalf(t, recollapseHoistedFixture)
	twoLevel := loadRecollapsePairHalf(t, recollapseTwoLevelFixture)

	// The pair only proves anything if the two halves differ in the hoist and
	// nothing else.
	for _, sect := range []string{"query.promql", "range_step", "seed"} {
		if got, want := section(t, twoLevel, sect), section(t, hoisted, sect); got != want {
			t.Fatalf("fixture pair disagrees on section %q — the two halves must describe ONE experiment\n"+
				"--- %s ---\n%s\n--- %s ---\n%s",
				sect, recollapseHoistedFixture, want, recollapseTwoLevelFixture, got)
		}
	}
	if _, on := hoisted.Section(recollapseDirectiveSection); !on {
		t.Fatalf("%s has no %q section — the hoisted half of the pair no longer exercises the hoist",
			recollapseHoistedFixture, recollapseDirectiveSection)
	}
	if _, on := twoLevel.Section(recollapseDirectiveSection); on {
		t.Fatalf("%s declares %q — the un-hoisted half of the pair must lower WITHOUT the hoist, "+
			"otherwise the row comparison compares the hoist against itself",
			recollapseTwoLevelFixture, recollapseDirectiveSection)
	}

	if got, want := section(t, twoLevel, expectedRowsSection), section(t, hoisted, expectedRowsSection); got != want {
		t.Fatalf("the hoist changed the rows rate() returns — it is WRONG, not the goldens\n"+
			"--- %s (hoist ON) ---\n%s\n--- %s (hoist OFF) ---\n%s",
			recollapseHoistedFixture, want, recollapseTwoLevelFixture, got)
	}
}

// loadRecollapsePairHalf loads one half of the pair from test/spec/promql/. A
// renamed or deleted fixture fails here rather than quietly disarming the
// comparison.
func loadRecollapsePairHalf(t *testing.T, name string) *spec.Case {
	t.Helper()
	path := filepath.Join(fixtureDir, name)
	c, err := spec.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v — this fixture is half of the hoist's semantics proof; "+
			"if it moved, retarget this test rather than dropping it", path, err)
	}
	return c
}

// section returns c's named section, failing when it is absent or blank — an
// empty body would make every comparison in this test trivially true.
func section(t *testing.T, c *spec.Case, name string) string {
	t.Helper()
	body, ok := c.Section(name)
	if !ok {
		t.Fatalf("fixture %s has no %q section", c.Path, name)
	}
	if strings.TrimSpace(body) == "" {
		t.Fatalf("fixture %s has an empty %q section", c.Path, name)
	}
	return body
}
