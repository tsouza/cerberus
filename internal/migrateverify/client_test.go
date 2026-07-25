package migrateverify

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"now", now},
		{"-1h", now.Add(-time.Hour)},
		{"now-15m", now.Add(-15 * time.Minute)},
		{"+30m", now.Add(30 * time.Minute)},
		{"2026-07-22T11:00:00Z", time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC)},
		{"1700000000", time.Unix(1_700_000_000, 0).UTC()},
	}
	for _, c := range cases {
		got, err := ParseTime(c.in, now)
		if err != nil {
			t.Errorf("ParseTime(%q): %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("ParseTime(%q) = %s, want %s", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"", "yesterday", "-1potato"} {
		if _, err := ParseTime(bad, now); err == nil {
			t.Errorf("ParseTime(%q) should error", bad)
		}
	}
}

func TestBuildParams(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	p, err := BuildParams("-1h", "now", "60s", 0.5, now)
	if err != nil {
		t.Fatalf("BuildParams: %v", err)
	}
	if !p.Start.Equal(now.Add(-time.Hour)) || !p.End.Equal(now) {
		t.Errorf("window = [%s, %s], want [-1h, now]", p.Start, p.End)
	}
	if p.Step != 60*time.Second || p.Tolerance != 0.5 {
		t.Errorf("step/tol = %s / %v, want 60s / 0.5", p.Step, p.Tolerance)
	}

	// end must be after start.
	if _, err := BuildParams("now", "-1h", "60s", 0, now); err == nil {
		t.Error("BuildParams should reject end before start")
	}
	// step must be positive and parseable.
	if _, err := BuildParams("-1h", "now", "0s", 0, now); err == nil {
		t.Error("BuildParams should reject a zero step")
	}
	if _, err := BuildParams("-1h", "now", "banana", 0, now); err == nil {
		t.Error("BuildParams should reject an unparseable step")
	}
}

// TestLoadCorpus writes a v1 corpus holding one entry of each shape the router
// must distinguish — a PromQL rule, a LogQL log-stream panel, a LogQL metric
// panel, a TraceQL metrics panel, a TraceQL search panel — plus a harvest-time
// skip, and pins that each lands in exactly one bucket with the right head, that
// corpus order survives, and that the buckets account for every input entry.
func TestLoadCorpus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.json")
	const corpusEntries = 5
	const body = `{
  "version": 1,
  "queries": [
    {"expr": "up", "source": "rule:a", "kind": "record", "lang": "promql"},
    {"expr": "{app=\"x\"}", "source": "panel:logs", "kind": "panel", "lang": "logql"},
    {"expr": "sum(rate({app=\"x\"}[5m]))", "source": "panel:lograte", "kind": "panel", "lang": "logql"},
    {"expr": "{} | rate()", "source": "panel:spanrate", "kind": "panel", "lang": "traceql"},
    {"expr": "{ span.a = \"b\" }", "source": "panel:search", "kind": "panel", "lang": "traceql"}
  ],
  "skipped": [
    {"source": "rule:broken.yml", "reason": "rule has an empty expr"}
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCorpus(path)
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}

	wantQueries := []Query{
		{Expr: "up", Source: "rule:a", Head: HeadProm, Lang: "promql"},
		{Expr: `sum(rate({app="x"}[5m]))`, Source: "panel:lograte", Head: HeadLoki, Lang: "logql"},
		{Expr: "{} | rate()", Source: "panel:spanrate", Head: HeadTempo, Lang: "traceql"},
	}
	if len(c.Queries) != len(wantQueries) {
		t.Fatalf("Queries = %+v, want the 3 metric-lane queries", c.Queries)
	}
	for i, want := range wantQueries {
		if c.Queries[i] != want {
			t.Errorf("Queries[%d] = %+v, want %+v (corpus order + head tag must survive)", i, c.Queries[i], want)
		}
	}

	if len(c.OutOfScope) != 2 {
		t.Fatalf("OutOfScope = %+v, want the log-stream panel and the trace search", c.OutOfScope)
	}
	wantOOS := []struct {
		source string
		head   string
		kind   string
	}{
		{"panel:logs", HeadLoki, KindLogStream},
		{"panel:search", HeadTempo, KindTraceSearch},
	}
	for i, want := range wantOOS {
		got := c.OutOfScope[i]
		if got.Source != want.source || got.Head != want.head || got.Kind != want.kind {
			t.Errorf("OutOfScope[%d] = %+v, want source=%s head=%s kind=%s", i, got, want.source, want.head, want.kind)
		}
		if got.Reason == "" {
			t.Errorf("OutOfScope[%d] (%s) carries an empty Reason: the operator must be told WHY it was not judged", i, got.Source)
		}
		if got.Expr == "" {
			t.Errorf("OutOfScope[%d] (%s) carries an empty Expr", i, got.Source)
		}
	}

	// The accounting invariant: every corpus entry landed in exactly one bucket.
	if got := len(c.Queries) + len(c.OutOfScope); got != corpusEntries {
		t.Errorf("replayable + out-of-scope = %d, want %d: an entry was dropped", got, corpusEntries)
	}

	if len(c.HarvestSkipped) != 1 || c.HarvestSkipped[0].Source != "rule:broken.yml" ||
		c.HarvestSkipped[0].Reason != "rule has an empty expr" {
		t.Errorf("HarvestSkipped = %+v, want the one broken-rule skip", c.HarvestSkipped)
	}
}

// TestFormatStep pins that the step is sent with sub-second precision instead of
// being truncated to whole seconds — a 1500ms step must replay as "1.5", not "1".
func TestFormatStep(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{60 * time.Second, "60"},
		{1500 * time.Millisecond, "1.5"},
		{500 * time.Millisecond, "0.5"},
		{90 * time.Second, "90"},
	}
	for _, c := range cases {
		if got := formatStep(c.in); got != c.want {
			t.Errorf("formatStep(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLoadCorpus_Errors(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadCorpus(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("LoadCorpus should error on a missing file")
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCorpus(bad); err == nil {
		t.Error("LoadCorpus should error on malformed JSON")
	}

	wrongVer := filepath.Join(dir, "v99.json")
	if err := os.WriteFile(wrongVer, []byte(`{"version":99,"queries":[],"skipped":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCorpus(wrongVer); err == nil {
		t.Error("LoadCorpus should reject an unknown corpus version")
	}
}

// TestCanonicalLabels pins that label-map order does not affect the match key.
func TestCanonicalLabels(t *testing.T) {
	a := canonicalLabels(map[string]string{"b": "2", "a": "1"})
	b := canonicalLabels(map[string]string{"a": "1", "b": "2"})
	if a != b {
		t.Errorf("canonical labels must be order-independent: %q vs %q", a, b)
	}
	if a != `{a="1",b="2"}` {
		t.Errorf("canonical form = %q, want {a=\"1\",b=\"2\"}", a)
	}
}
