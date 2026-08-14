//go:build chdb

// chDB-backed proof that the probe rewrite is right in ClickHouse and not
// only in Go.
//
// Every other test of this feature reasons about `extractGroups` through
// Go's `regexp`, which is a model of ClickHouse's regex engine rather than
// ClickHouse itself. The two agree on re2 semantics, but the emitted form
// — `arrayFirst` over TWO arrays, whose predicate runs over the second and
// whose result comes from the first — is a ClickHouse function contract,
// not a Go one. If CH returned the element of the array the predicate ran
// over, every Go-side differential would still pass while the label on the
// wire carried the probe's text instead of the carrier's.
//
// So this runs the SQL the emitter really produces, against the values
// ClickHouse really computes, and compares to Go's `ExpandString` — the
// engine reference Prometheus and reference Loki both define
// `label_replace` in terms of.
package chsql

import (
	"regexp"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

func TestLabelReplaceProbedSelection_ChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)

	cases := []struct {
		name    string
		regex   string
		probed  string
		segment chplan.LabelReplaceSegment
		srcs    []string
	}{
		{
			// The first carrier's branch is mandatory but its own capture is
			// nullable. The sibling is non-empty, so its empty probe proves
			// that the first branch was selected.
			name:    "mandatory_alternation_negative_sibling_probe",
			regex:   `(?:(?P<dup>a?)|(?P<dup>b))(?P<dup>c)`,
			probed:  `(?:(?P<dup>a?)|(?P<cerberusprobe0>(?P<dup>b)))(?P<dup>c)`,
			segment: chplan.LabelReplaceSegment{Group: 1, Fallbacks: []int{3, 4}, NegativeProbes: [][]int{{2}, nil, nil}},
			srcs:    []string{"c", "ac", "bc"},
		},
		{
			// The witness issue #1956 was narrowed to. On "xb" the
			// optional branch takes part and captures nothing, so the
			// answer is "" — which a search over the carriers' own
			// captures would get wrong by answering "b".
			name:    "quest_body_probe",
			regex:   `(?:x(?P<dup>a?))?(?P<dup>b)`,
			probed:  `(?:(?P<cerberusprobe0>x(?P<dup>a?)))?(?P<dup>b)`,
			segment: chplan.LabelReplaceSegment{Group: 2, Fallbacks: []int{3}, Probes: []int{1, 3}},
			srcs:    []string{"xb", "b", "xab"},
		},
		{
			// Only the branch holding the carrier is probed, so the probe
			// must stay silent on matches that take the other branch.
			name:    "alternation_branch_probe",
			regex:   `(?:x(?P<dup>a?)|y(?P<dup>b))?(?P<dup>c)`,
			probed:  `(?:(?P<cerberusprobe0>x(?P<dup>a?))|y(?P<dup>b))?(?P<dup>c)`,
			segment: chplan.LabelReplaceSegment{Group: 2, Fallbacks: []int{3, 4}, Probes: []int{1, 3, 4}},
			srcs:    []string{"xc", "xac", "ybc", "c"},
		},
		{
			// Two probes in one pattern: each must answer for its own
			// carrier and not for the other's.
			name:    "two_probes",
			regex:   `(?:x(?P<dup>a?))?(?:y(?P<dup>b?))?(?P<dup>c)`,
			probed:  `(?:(?P<cerberusprobe0>x(?P<dup>a?)))?(?:(?P<cerberusprobe1>y(?P<dup>b?)))?(?P<dup>c)`,
			segment: chplan.LabelReplaceSegment{Group: 2, Fallbacks: []int{4, 5}, Probes: []int{1, 3, 5}},
			srcs:    []string{"xyc", "yc", "c", "xaybc", "xybc", "xc"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := renderExpr(t, &chplan.LabelReplace{
				Map:              attrsMap(),
				Dst:              "dst",
				Src:              "src",
				Regex:            tc.regex,
				ProbedRegex:      tc.probed,
				EmptyReplacement: "",
				Segments:         []chplan.LabelReplaceSegment{tc.segment},
			})
			// The emitted expression rewrites the whole Attributes map, so
			// the assertion reads the destination label back out of it.
			// mapFilter drops an empty value, and a missing key reads as
			// the empty string, which is the same observable.
			query := "SELECT (" + sql + ")[?] FROM (SELECT map(?, ?) AS Attributes)"

			oracle := regexp.MustCompile("^(?s:" + tc.regex + ")$")
			for _, src := range tc.srcs {
				match := oracle.FindStringSubmatchIndex(src)
				if match == nil {
					t.Fatalf("regex %q does not match %q — the oracle needs a match", tc.regex, src)
				}
				want := string(oracle.ExpandString(nil, "$dup", src, match))

				bound := append(append([]any{}, args...), "dst", "src", src)
				var got string
				if err := db.QueryRow(query, bound...).Scan(&got); err != nil {
					t.Fatalf("regex %q src %q: %v\n--- sql ---\n%s", tc.regex, src, err, query)
				}
				if got != want {
					t.Errorf("regex %q src %q: ClickHouse answered %q, Go's ExpandString gives %q",
						tc.regex, src, got, want)
				}
			}
		})
	}
}
