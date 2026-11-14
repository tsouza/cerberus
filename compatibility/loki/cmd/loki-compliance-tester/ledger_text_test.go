package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// committedLedgers are the files a Loki compatibility run's results are
// pinned in — the per-case parity roster buckets and the upstream-skip
// baseline — mapped to their text: a bucket's case IDs, decoded, one per
// line, and the baseline as written.
func committedLedgers(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "..", "parity-baseline", "loki", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no committed loki parity-baseline bucket found")
	}
	paths = append(paths, filepath.Join("..", "..", "upstream-skip-baseline.txt"))
	ledgers := make(map[string]string, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p) //nolint:gosec // committed ledger path
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(p) != ".json" {
			ledgers[p] = string(raw)
			continue
		}
		var bucket struct {
			Cases []string `json:"cases"`
		}
		if err := json.Unmarshal(raw, &bucket); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		ledgers[p] = strings.Join(bucket.Cases, "\n")
	}
	return ledgers
}

// TestCommittedLedgers_CarryNoUpstreamText — no committed ledger carries
// a description or a resolved query of the upstream corpus. Upstream cases
// are keyed by opaque ID (caseSet, baselineKey), so the only way upstream
// text reaches a ledger is a key built from the readable identity. A
// definition whose whole query is `${SELECTOR}` resolves to a bare
// selector from the cerberus-authored dataset metadata, which is not
// upstream text, so its resolved query is not searched for.
func TestCommittedLedgers_CarryNoUpstreamText(t *testing.T) {
	t.Parallel()
	all := fullCorpusFlags()
	all.includeSkip = true

	defs, err := loadUpstreamCorpus(all)
	if err != nil {
		t.Fatalf("loadUpstreamCorpus: %v", err)
	}
	needles := map[string]string{}
	for _, def := range defs {
		if def.Description != "" {
			needles[def.Description] = def.Source + " description"
		}
	}
	for _, instant := range []bool{false, true} {
		cases, err := loadCases(all, instant)
		if err != nil {
			t.Fatalf("loadCases(instant=%v): %v", instant, err)
		}
		for _, lc := range cases {
			if lc.upstream && lc.expandErr == nil && strings.TrimSpace(lc.def.Query) != "${SELECTOR}" {
				needles[lc.tc.Query] = lc.def.Source + " resolved query"
			}
		}
	}
	if len(needles) == 0 {
		t.Fatal("the upstream corpus yielded no text to search for")
	}

	for path, body := range committedLedgers(t) {
		for needle, what := range needles {
			if strings.Contains(body, needle) {
				t.Errorf("%s carries the upstream %s %q", path, what, needle)
			}
		}
	}
}
