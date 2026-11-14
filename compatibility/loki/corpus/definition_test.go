package corpus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCorpus lays out a corpus root: files maps `<suite>/<name>` to its
// YAML. Every suite directory is created even when it holds no file.
func writeCorpus(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for _, s := range AllSuites {
		if err := os.MkdirAll(filepath.Join(root, string(s)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLoad_OrdersBySuiteThenFileThenPosition(t *testing.T) {
	t.Parallel()
	root := writeCorpus(t, map[string]string{
		"exhaustive/a.yaml": "queries:\n  - description: e1\n    query: '{a=\"1\"}'\n    time_range: {length: 1h}\n",
		"fast/b.yml":        "queries:\n  - description: f3\n    query: '{a=\"1\"}'\n    time_range: {length: 1h}\n",
		"fast/a.yaml": "# leading comment\nqueries:\n" +
			"  - description: f1\n    query: '{a=\"1\"}'\n    time_range: {length: 1h}\n" +
			"  - description: f2\n    query: '{a=\"1\"}'\n    time_range: {length: 1h}\n",
		"fast/notes.txt": "not a query file",
	})
	defs, err := Load(root, AllSuites...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var got []string
	for _, d := range defs {
		got = append(got, d.Description+"@"+d.Source)
	}
	want := []string{"f1@fast/a.yaml:3", "f2@fast/a.yaml:6", "f3@fast/b.yml:2", "e1@exhaustive/a.yaml:2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Load order = %v, want %v", got, want)
	}
	if defs[0].File() != "fast/a.yaml" {
		t.Fatalf("File() = %q, want fast/a.yaml", defs[0].File())
	}
}

func TestLoad_SuiteSubsetAndMissingSuite(t *testing.T) {
	t.Parallel()
	root := writeCorpus(t, map[string]string{
		"fast/a.yaml":       "queries:\n  - description: f\n    query: '{a=\"1\"}'\n    time_range: {length: 1h}\n",
		"regression/a.yaml": "queries:\n  - description: r\n    query: '{a=\"1\"}'\n    time_range: {length: 1h}\n",
	})
	defs, err := Load(root, SuiteRegression)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(defs) != 1 || defs[0].Description != "r" {
		t.Fatalf("Load(regression) = %+v, want only the regression definition", defs)
	}
	if _, err := Load(filepath.Join(root, "missing"), SuiteFast); err == nil {
		t.Fatal("Load over a missing suite directory returned no error")
	}
}

func TestLoad_EmptyFileIsZeroDefinitions(t *testing.T) {
	t.Parallel()
	root := writeCorpus(t, map[string]string{"fast/empty.yaml": "", "fast/comment.yaml": "# nothing yet\n"})
	defs, err := Load(root, AllSuites...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(defs) != 0 {
		t.Fatalf("Load = %d definitions, want 0", len(defs))
	}
}

func TestLoad_Defaults(t *testing.T) {
	t.Parallel()
	root := writeCorpus(t, map[string]string{"fast/a.yaml": `queries:
  - description: inferred metric
    query: 'sum(count_over_time(${SELECTOR}[${RANGE}]))'
    time_range: {length: 1h, step: 1m}
  - description: inferred log
    query: '${SELECTOR} |= "x"'
    time_range: {length: 1h}
  - description: declared log, one direction
    query: '${SELECTOR}'
    kind: log
    directions: backward
    time_range: {length: 1h}
  - description: declared metric keeps no directions
    query: 'count_over_time(${SELECTOR}[5m])'
    kind: metric
    time_range: {length: 1h}
  - description: unparseable falls back to log
    query: 'not logql'
    time_range: {length: 1h}
`})
	defs, err := Load(root, AllSuites...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []struct {
		kind string
		dirs Directions
	}{
		{KindMetric, ""},
		{KindLog, DirectionsBoth},
		{KindLog, DirectionsBackward},
		{KindMetric, ""},
		{KindLog, DirectionsBoth},
	}
	for i, w := range want {
		if defs[i].Kind != w.kind || defs[i].Directions != w.dirs {
			t.Fatalf("%q: kind=%q directions=%q, want kind=%q directions=%q",
				defs[i].Description, defs[i].Kind, defs[i].Directions, w.kind, w.dirs)
		}
	}
}

func TestLoad_Rejects(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ body, want string }{
		"unknown key":        {"queries:\n  - query: x\n    direction: both\n    time_range: {length: 1h}\n", "direction"},
		"unknown kind":       {"queries:\n  - query: x\n    kind: logs\n    time_range: {length: 1h}\n", "kind"},
		"unknown directions": {"queries:\n  - query: x\n    directions: sideways\n    time_range: {length: 1h}\n", "directions"},
		"missing length":     {"queries:\n  - query: x\n    time_range: {step: 1m}\n", "time_range.length is required"},
		"bad length":         {"queries:\n  - query: x\n    time_range: {length: soon}\n", "time_range.length"},
		"bad step":           {"queries:\n  - query: x\n    time_range: {length: 1h, step: often}\n", "time_range.step"},
		"malformed yaml":     {"queries: [\n", "fast/a.yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := writeCorpus(t, map[string]string{"fast/a.yaml": tc.body})
			_, err := Load(root, AllSuites...)
			if err == nil {
				t.Fatalf("Load accepted %q", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestPartition_KeepsOrder(t *testing.T) {
	t.Parallel()
	defs := []Definition{{Description: "a"}, {Description: "b", Skip: true}, {Description: "c"}, {Description: "d", Skip: true}}
	run, skip := Partition(defs)
	if len(run) != 2 || run[0].Description != "a" || run[1].Description != "c" {
		t.Fatalf("runnable = %+v", run)
	}
	if len(skip) != 2 || skip[0].Description != "b" || skip[1].Description != "d" {
		t.Fatalf("skipped = %+v", skip)
	}
}
