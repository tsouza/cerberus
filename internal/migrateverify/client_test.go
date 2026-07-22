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

// TestLoadCorpus writes a v1 corpus with a PromQL query and a LogQL panel, then
// checks that verify picks up the PromQL query and carries the LogQL one through
// as out-of-scope (never silently dropped).
func TestLoadCorpus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.json")
	const body = `{
  "version": 1,
  "queries": [
    {"expr": "up", "source": "rule:a", "kind": "record", "lang": "promql"},
    {"expr": "{app=\"x\"}", "source": "panel:logs", "kind": "panel", "lang": "logql"}
  ],
  "skipped": []
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCorpus(path)
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	if len(c.PromQL) != 1 || c.PromQL[0].Expr != "up" {
		t.Errorf("PromQL = %+v, want the single `up` query", c.PromQL)
	}
	if len(c.OutOfScope) != 1 || c.OutOfScope[0].Lang != "logql" {
		t.Errorf("OutOfScope = %+v, want the one logql panel", c.OutOfScope)
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
