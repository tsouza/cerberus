package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	bench "github.com/tsouza/cerberus/compatibility/loki/upstream/loki-bench"
)

// TestIsExpectedEmptyCase pins the corpus-tag contract that powers
// `compareOne`'s empty-result branch. A drift in the upstream YAML tag
// vocabulary (e.g. renaming the tag) would silently flip the failing
// `fast/basic-selectors.yaml#Log query with impossible filter` case
// back into a `baseline returned empty` row, so we anchor the predicate
// against the tag literal here.
func TestIsExpectedEmptyCase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tags []string
		want bool
	}{
		{"tagged-empty-result", []string{"line-filter", "empty-result", "cache-test"}, true},
		{"tag-set-without-empty", []string{"line-filter", "regex"}, false},
		{"nil-tags", nil, false},
		{"empty-tags", []string{}, false},
		{"only-empty-result", []string{"empty-result"}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isExpectedEmptyCase(bench.TestCase{Tags: tc.tags})
			if got != tc.want {
				t.Fatalf("isExpectedEmptyCase(tags=%v) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

// TestScoreCounts asserts the score denominator includes every case
// the driver attempted, with the numerator being only the cases that
// passed. There is no allow-list / overlay exclusion: the harness
// carries no `should_skip` consumer code.
func TestScoreCounts(t *testing.T) {
	t.Parallel()
	results := []Result{
		{},                        // pass
		{},                        // pass
		{Diff: "x"},               // diff
		{UnexpectedFailure: "y"},  // unexpected failure
		{UnexpectedSuccess: true}, // unexpected success
	}
	passed, total := scoreCounts(results)
	if total != 5 {
		t.Fatalf("total = %d, want 5 (every attempted case counts)", total)
	}
	if passed != 2 {
		t.Fatalf("passed = %d, want 2", passed)
	}
}

func TestScoreCounts_AllPassing(t *testing.T) {
	t.Parallel()
	results := []Result{{}, {}, {}}
	passed, total := scoreCounts(results)
	if passed != 3 || total != 3 {
		t.Fatalf("(passed, total) = (%d, %d), want (3, 3)", passed, total)
	}
}

func TestScoreCounts_Empty(t *testing.T) {
	t.Parallel()
	passed, total := scoreCounts(nil)
	if passed != 0 || total != 0 {
		t.Fatalf("(passed, total) = (%d, %d), want (0, 0)", passed, total)
	}
}

// TestBaselineKey pins the wire-format key the baseline file + the
// overlay both index on. Drift here would silently break the sanity
// rail's name-to-corpus mapping.
func TestBaselineKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		def  bench.QueryDefinition
		want string
	}{
		{
			name: "strips-line-suffix",
			def:  bench.QueryDefinition{Source: "fast/basic-selectors.yaml:12", Description: "Basic label selector"},
			want: "fast/basic-selectors.yaml#Basic label selector",
		},
		{
			name: "no-line-suffix",
			def:  bench.QueryDefinition{Source: "regression/metric-queries.yaml", Description: "HTTP status code distribution"},
			want: "regression/metric-queries.yaml#HTTP status code distribution",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := baselineKey(tc.def); got != tc.want {
				t.Fatalf("baselineKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReadSkipBaseline_IgnoresCommentsAndBlanks pins the parser
// contract — comment + blank lines stay out of the resulting set so
// the file can carry rationale prose.
func TestReadSkipBaseline_IgnoresCommentsAndBlanks(t *testing.T) {
	t.Parallel()
	body := "# leading comment\n" +
		"# another comment\n" +
		"\n" +
		"fast/foo.yaml#alpha\n" +
		"\n" +
		"   # indented comment\n" +
		"regression/bar.yaml#beta\n" +
		"exhaustive/baz.yaml#gamma\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	got, err := readSkipBaseline(path)
	if err != nil {
		t.Fatalf("readSkipBaseline: %v", err)
	}
	want := []string{
		"exhaustive/baz.yaml#gamma",
		"fast/foo.yaml#alpha",
		"regression/bar.yaml#beta",
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestCheckSkipBaseline_MatchPasses confirms an exact set match returns
// nil — the no-drift path.
func TestCheckSkipBaseline_MatchPasses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	body := "fast/a.yaml#one\nregression/b.yaml#two\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	defs := []bench.QueryDefinition{
		{Source: "fast/a.yaml:3", Description: "one"},
		{Source: "regression/b.yaml:7", Description: "two"},
	}
	if err := checkSkipBaseline(path, defs); err != nil {
		t.Fatalf("checkSkipBaseline returned err: %v", err)
	}
}

// TestCheckSkipBaseline_DetectsAddition pins the trip-wire: a new
// upstream entry not in the baseline fails with the new key named.
func TestCheckSkipBaseline_DetectsAddition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	body := "fast/a.yaml#one\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	defs := []bench.QueryDefinition{
		{Source: "fast/a.yaml:3", Description: "one"},
		{Source: "regression/new.yaml:11", Description: "fresh entry"},
	}
	err := checkSkipBaseline(path, defs)
	if err == nil {
		t.Fatalf("checkSkipBaseline = nil, want drift error")
	}
	if !strings.Contains(err.Error(), "regression/new.yaml#fresh entry") {
		t.Fatalf("error %q missing added key", err.Error())
	}
}

// TestCheckSkipBaseline_DetectsRemoval mirrors the addition case for
// the other direction — an upstream re-enable surfaces.
func TestCheckSkipBaseline_DetectsRemoval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	body := "fast/a.yaml#one\nregression/gone.yaml#two\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	defs := []bench.QueryDefinition{
		{Source: "fast/a.yaml:3", Description: "one"},
	}
	err := checkSkipBaseline(path, defs)
	if err == nil {
		t.Fatalf("checkSkipBaseline = nil, want drift error")
	}
	if !strings.Contains(err.Error(), "regression/gone.yaml#two") {
		t.Fatalf("error %q missing removed key", err.Error())
	}
}

// TestWriteSkipBaseline_RoundTripsThroughRead confirms the writer
// emits a file the reader parses back to the same sorted set, so
// -regen-baseline + the diff path are self-consistent.
func TestWriteSkipBaseline_RoundTripsThroughRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	defs := []bench.QueryDefinition{
		{Source: "regression/b.yaml:7", Description: "two"},
		{Source: "fast/a.yaml:3", Description: "one"},
		{Source: "exhaustive/c.yaml:99", Description: "three"},
	}
	if err := writeSkipBaseline(path, defs); err != nil {
		t.Fatalf("writeSkipBaseline: %v", err)
	}
	got, err := readSkipBaseline(path)
	if err != nil {
		t.Fatalf("readSkipBaseline: %v", err)
	}
	want := []string{
		"exhaustive/c.yaml#three",
		"fast/a.yaml#one",
		"regression/b.yaml#two",
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}
