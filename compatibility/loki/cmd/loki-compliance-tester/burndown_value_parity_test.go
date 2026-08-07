package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/grafana/loki/v3/pkg/logproto"

	bench "github.com/tsouza/cerberus/compatibility/loki/upstream/loki-bench"
)

// The burndown pass is the only value-level coverage the wrong-rejection
// operators have: the vendored corpus exercises none of them. Its case
// list is therefore load-bearing in a way a query list usually is not —
// a selector picked non-deterministically, a window that drifts off the
// seed grid, or a metric case that lost its step all turn a real
// operator divergence into a spurious diff (or, worse, into a pass).

// burndownMetadata builds the minimal dataset metadata burndownCases
// reads: a web-server selector for the ip()/pattern arms and a logfmt
// selector carrying an unwrappable `duration` for the first/last arms.
func burndownMetadata() *bench.DatasetMetadata {
	return &bench.DatasetMetadata{
		ByServiceName: map[string][]string{
			"web-server": {`{service_name="web-server"}`, `{service_name="web-server", env="b"}`},
		},
		ByFormat: map[bench.LogFormat][]string{
			bench.LogFormatLogfmt: {`{service_name="zeta"}`, `{service_name="alpha"}`, `{service_name="unrelated"}`},
		},
		ByUnwrappableField: map[string][]string{
			"duration": {`{service_name="alpha"}`, `{service_name="zeta"}`},
		},
		TimeRange: bench.TimeRange{
			Start: time.Unix(1700000000, 0).UTC(),
			End:   time.Unix(1700086400, 0).UTC(),
		},
	}
}

// TestIntersectSorted pins the deterministic selector pick. The metadata
// maps carry generator-ordered slices, so an unsorted intersection would
// make the burndown query set depend on the fixture's serialisation
// order — the diff would move between runs.
func TestIntersectSorted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b []string
		want []string
	}{
		{"overlap-is-sorted", []string{"c", "a", "b"}, []string{"b", "c"}, []string{"b", "c"}},
		{"disjoint", []string{"a"}, []string{"b"}, nil},
		{"empty-left", nil, []string{"a"}, nil},
		{"empty-right", []string{"a"}, nil, nil},
		{"duplicates-in-a-are-kept", []string{"a", "a"}, []string{"a"}, []string{"a", "a"}},
		{"b-order-does-not-leak", []string{"a", "b"}, []string{"b", "a"}, []string{"a", "b"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := intersectSorted(tc.a, tc.b)
			if len(got) != len(tc.want) {
				t.Fatalf("intersectSorted(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("entry %d = %q, want %q (full: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// TestBurndownCases_Shape pins the case list's structure: the selector
// picks, the metric/log window split, the direction and the step. Each
// of these is a precondition for the value diff meaning what it claims.
func TestBurndownCases_Shape(t *testing.T) {
	t.Parallel()
	md := burndownMetadata()
	cases, err := burndownCases(md)
	if err != nil {
		t.Fatalf("burndownCases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("burndownCases returned no cases")
	}

	start := md.TimeRange.Start
	var metrics, logs int
	for _, tc := range cases {
		if tc.Source != burndownSource {
			t.Fatalf("case %q source = %q, want %q", tc.QueryDesc, tc.Source, burndownSource)
		}
		if len(tc.Tags) != 1 || tc.Tags[0] != "wrong-rejection-burndown" {
			t.Fatalf("case %q tags = %v", tc.QueryDesc, tc.Tags)
		}
		if tc.QueryDesc == "" {
			t.Fatalf("case with query %q carries no description", tc.Query)
		}
		switch tc.Kind() {
		case "metric":
			metrics++
			if tc.Step != burndownStep {
				t.Fatalf("metric case %q step = %v, want %v", tc.QueryDesc, tc.Step, burndownStep)
			}
			if !tc.Start.Equal(start) || !tc.End.Equal(start.Add(burndownLength)) {
				t.Fatalf("metric case %q window = [%v, %v], want the anchor-aligned [start, start+%v]", tc.QueryDesc, tc.Start, tc.End, burndownLength)
			}
			if tc.Direction != logproto.FORWARD {
				t.Fatalf("metric case %q direction = %v, want FORWARD", tc.QueryDesc, tc.Direction)
			}
		case "log":
			logs++
			if tc.Step != 0 {
				t.Fatalf("log case %q carries step %v; a log query has no step grid", tc.QueryDesc, tc.Step)
			}
			// The log window sits OFF the per-minute seed grid on both
			// edges so reference Loki's exclusive `end` cannot turn into
			// a spurious one-entry diff.
			const logWindowOffset = 30 * time.Second
			if !tc.Start.Equal(start.Add(logWindowOffset)) || !tc.End.Equal(start.Add(burndownLength).Add(logWindowOffset)) {
				t.Fatalf("log case %q window = [%v, %v], want both edges shifted %v off the seed grid", tc.QueryDesc, tc.Start, tc.End, logWindowOffset)
			}
			if tc.Direction != logproto.BACKWARD {
				t.Fatalf("log case %q direction = %v, want BACKWARD", tc.QueryDesc, tc.Direction)
			}
		default:
			t.Fatalf("case %q has kind %q; the query does not parse: %s", tc.QueryDesc, tc.Kind(), tc.Query)
		}
	}
	if metrics == 0 || logs == 0 {
		t.Fatalf("case kinds = (metric=%d, log=%d); the pass must carry both shapes", metrics, logs)
	}
}

// TestBurndownCases_CoversEveryBurnedDownOperator is the closure check:
// each operator family the pass exists for must appear in some query.
// Without it a refactor could silently drop an operator's only
// value-level coverage and the pass would still look healthy.
func TestBurndownCases_CoversEveryBurnedDownOperator(t *testing.T) {
	t.Parallel()
	cases, err := burndownCases(burndownMetadata())
	if err != nil {
		t.Fatalf("burndownCases: %v", err)
	}
	var joined strings.Builder
	for _, tc := range cases {
		joined.WriteString(tc.Query)
		joined.WriteString("\n")
	}
	all := joined.String()
	for _, op := range []string{
		") and ", ") or ", ") unless ", "+ bool ",
		"first_over_time", "last_over_time", "absent_over_time",
		"topk(", "bottomk(", "sort(", "sort_desc(",
		`|= ip(`, `!= ip(`, `= ip(`, `|> "`, `!> "`,
	} {
		if !strings.Contains(all, op) {
			t.Fatalf("no burndown case exercises %q; the operator has no value-level coverage", op)
		}
	}
}

// TestBurndownCases_UsesTheSortedDurationSelector — the logfmt/duration
// selector is chosen through intersectSorted, so it must be the
// lexicographically-first member of the intersection rather than the
// first entry of either input slice.
func TestBurndownCases_UsesTheSortedDurationSelector(t *testing.T) {
	t.Parallel()
	cases, err := burndownCases(burndownMetadata())
	if err != nil {
		t.Fatalf("burndownCases: %v", err)
	}
	var found bool
	for _, tc := range cases {
		if !strings.Contains(tc.Query, "first_over_time") {
			continue
		}
		found = true
		if !strings.Contains(tc.Query, `{service_name="alpha"}`) {
			t.Fatalf("first_over_time case uses %q; want the sorted intersection's first selector {service_name=\"alpha\"}", tc.Query)
		}
	}
	if !found {
		t.Fatal("no first_over_time case in the list")
	}
}

// TestBurndownCases_MissingPreconditions — a dataset that cannot support
// the pass fails loudly and names what is missing, rather than quietly
// emitting fewer cases and shrinking the score denominator.
func TestBurndownCases_MissingPreconditions(t *testing.T) {
	t.Parallel()

	noWeb := burndownMetadata()
	noWeb.ByServiceName = map[string][]string{"api": {`{service_name="api"}`}}
	_, err := burndownCases(noWeb)
	if err == nil {
		t.Fatal("metadata without a web-server selector produced no error")
	}
	if !strings.Contains(err.Error(), "web-server") {
		t.Fatalf("error %q should name the missing service", err.Error())
	}

	noDuration := burndownMetadata()
	noDuration.ByUnwrappableField = map[string][]string{"bytes": {`{service_name="alpha"}`}}
	_, err = burndownCases(noDuration)
	if err == nil {
		t.Fatal("metadata without an unwrappable duration produced no error")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Fatalf("error %q should name the missing field", err.Error())
	}
}

// TestCompareBurndownValueParity_ReportsBrokenPreconditions — the pass
// surfaces a construction failure as one UnexpectedFailure result. That
// keeps the broken precondition visible instead of letting the pass
// contribute zero rows to a denominator nobody re-checks.
func TestCompareBurndownValueParity_ReportsBrokenPreconditions(t *testing.T) {
	t.Parallel()
	results := compareBurndownValueParity(&http.Client{Timeout: time.Second}, flags{}, &bench.DatasetMetadata{})
	if len(results) != 1 {
		t.Fatalf("results = %d, want exactly 1 construction-failure row", len(results))
	}
	if results[0].UnexpectedFailure == "" {
		t.Fatalf("construction failure produced a passing row: %+v", results[0])
	}
	if results[0].TestCase.Source != burndownSource {
		t.Fatalf("source = %q, want %q so the row lands in this pass", results[0].TestCase.Source, burndownSource)
	}
}
