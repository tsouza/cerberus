//go:build chdb

// chDB-backed proof of the one assumption the counter-reset mask's pair
// lambda rests on once its bucket ladders arrive as ARGUMENTS
// (expHistogramPairBucketLadderArgs, cerberus issue #3239): that the
// permutation `arraySort((rp, rt) -> rt, arrayEnumerate(<ts>), <ts>)`
// yields is the permutation `arraySort((row, key) -> key, <list>, <ts>)`
// applies to its own first argument.
//
// # Why this needs pinning now
//
// Before #3239 every one of a pair's fields was read through the SAME
// position permutation, so a pair could not mix two rows: whatever order
// the positions came out in, `_hq_scales[rb]` and `_hq_pos_buckets[rb]`
// named one row. Now the scalars still come through the positions while
// the two bucket ladders come through a direct sort, and the pairing is
// only coherent if the two spellings agree element for element. They do,
// because ClickHouse derives the order from the comparator's values alone
// — that one ts array in both spellings — and the sort of a permutation
// by those values does not consult the payload. That is an assertion
// about the SUBSTRATE, so it is asserted against the substrate rather
// than argued in a comment.
//
// # Why the ties matter
//
// Equal keys are the only input on which an unstable sort could order two
// elements differently, so every case below carries duplicate timestamps
// — including one whose timestamps are ALL equal, where the comparator
// can distinguish nothing at all and only the payload could break a tie.
// A stored series is not supposed to hold two samples at one timestamp,
// but the window stage's `HAVING uniqExact(TimeUnix) >= 2` guard admits
// groups that do, so the mask must not depend on it.
package promql_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/promql"
)

// pairLadderPositionSpelling and pairLadderSortSpelling are the two
// orderings under test, as the emitter renders them. They are asserted
// against the SQL a real `resets()` lowering emits (see
// TestExpHistogramPairLadder_ChDB_SortAgreesWithPositionPermutation), so
// a rename of either lambda's parameters cannot leave this test
// comparing two spellings production no longer uses.
const (
	pairLadderPositionSpelling = "arraySort((rp, rt) -> rt, arrayEnumerate("
	pairLadderSortSpelling     = "arraySort((row, key) -> key, "
)

// pairLadderTieCases are (timestamp, ladder) inputs whose timestamps all
// repeat. The ladders are Array(Array(...)), the stored bucket columns'
// own shape, and every element is distinct so a mispermutation shows up
// as an inequality rather than cancelling.
var pairLadderTieCases = map[string]struct {
	seconds []int
	ladders []string
}{
	"one_repeated_key": {
		seconds: []int{3, 1, 2, 1, 2},
		ladders: []string{"[11,12]", "[21]", "[31,32,33]", "[41,42]", "[51]"},
	},
	"all_keys_equal": {
		seconds: []int{7, 7, 7, 7},
		ladders: []string{"[11]", "[22,23]", "[34,35,36]", "[47]"},
	},
	"descending_with_ties": {
		seconds: []int{9, 9, 4, 4, 1, 1},
		ladders: []string{"[1]", "[2,2]", "[3]", "[4,4]", "[5]", "[6,6]"},
	},
	"empty_ladders_among_ties": {
		seconds: []int{5, 2, 5, 2},
		ladders: []string{"[]", "[9,9]", "[7]", "[]"},
	},
}

// TestExpHistogramPairLadder_ChDB_SortAgreesWithPositionPermutation runs
// both spellings over every tie case and fails unless gathering the
// ladder through the position permutation gives exactly the directly
// sorted ladder — and unless a real lowering still emits both spellings.
func TestExpHistogramPairLadder_ChDB_SortAgreesWithPositionPermutation(t *testing.T) {
	fixture := newChDBFixture(t, "")

	for name, tc := range pairLadderTieCases {
		t.Run(name, func(t *testing.T) {
			stamps := make([]string, len(tc.seconds))
			for i, s := range tc.seconds {
				stamps[i] = fmt.Sprintf("toDateTime64('2026-01-01 00:00:%02d', 9)", s)
			}
			// The position permutation, and one list gathered through it
			// against the same list sorted directly — both spelled the way
			// the emitter spells them.
			positions := pairLadderPositionSpelling + "ts), ts)"
			gathered := func(list string) string {
				return "arrayMap(p -> " + list + "[p], " + positions + ")"
			}
			sorted := func(list string) string {
				return pairLadderSortSpelling + list + ", ts)"
			}
			// The control gathers through the REVERSED permutation. Its
			// job is to fail the equality, which is what makes the two
			// assertions above mean something: without it a case whose
			// arrays compared equal for some reason unrelated to the
			// permutation — an empty array, say — would pass while
			// testing nothing.
			control := "arrayMap(p -> ladder[p], arrayReverse(" + positions + "))"
			query := fmt.Sprintf(
				"WITH [%s] AS ts, [%s] AS ladder SELECT %s = %s AS ladders_agree, %s = %s AS keys_agree, %s = %s AS control_agrees",
				strings.Join(stamps, ", "), strings.Join(tc.ladders, ", "),
				gathered("ladder"), sorted("ladder"),
				gathered("ts"), sorted("ts"),
				control, sorted("ladder"),
			)
			// ClickHouse answers a comparison as UInt8, which the chdb
			// driver hands back as a number rather than a bool.
			var laddersAgree, keysAgree, controlAgrees uint8
			if err := fixture.db.QueryRow(query).Scan(&laddersAgree, &keysAgree, &controlAgrees); err != nil {
				t.Fatalf("query %s: %v", query, err)
			}
			if keysAgree != 1 {
				t.Fatalf("%s: the two spellings order the KEY array differently", name)
			}
			if laddersAgree != 1 {
				t.Fatalf("%s: gathering the ladder by the position permutation differs from sorting it directly — "+
					"the mask would pair one row's scalars with another row's buckets", name)
			}
			if controlAgrees != 0 {
				t.Fatalf("%s: the REVERSED permutation also compares equal, so this case's assertions cannot fail — "+
					"pick ladder elements that distinguish an order", name)
			}
		})
	}

	t.Run("both spellings are what production emits", func(t *testing.T) {
		emitted, _ := resetMaskLower(t, fmt.Sprintf("resets(%s[5m])", resetMaskMetric), promql.LowerOpts{}, true)
		for _, spelling := range []string{pairLadderPositionSpelling, pairLadderSortSpelling} {
			if !strings.Contains(emitted, spelling) {
				t.Fatalf("emitted reset mask no longer carries %q — this test compares two spellings the emitter has stopped using:\n%s",
					spelling, emitted)
			}
		}
	})
}
