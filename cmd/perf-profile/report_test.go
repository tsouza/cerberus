package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/perf/profile"
)

func ptr(f float64) *float64 { return &f }

func writeRecordsFile(t *testing.T, path string, recs []profile.Record) {
	t.Helper()
	data, err := json.Marshal(recs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestMergeRecords_EmptyPattern(t *testing.T) {
	if _, err := mergeRecords("", 0); err == nil {
		t.Fatal("expected an error for an empty pattern")
	}
}

func TestMergeRecords_NoMatches(t *testing.T) {
	dir := t.TempDir()
	if _, err := mergeRecords(filepath.Join(dir, "*.json"), 0); err == nil {
		t.Fatal("expected an error when the glob matches nothing")
	}
}

func TestMergeRecords_ExpectShardsMismatch(t *testing.T) {
	dir := t.TempDir()
	writeRecordsFile(t, filepath.Join(dir, "shard-1.json"), []profile.Record{{Fixture: "promql/a"}})
	if _, err := mergeRecords(filepath.Join(dir, "*.json"), 2); err == nil {
		t.Fatal("expected an error when the matched file count does not equal -expect-shards")
	}
}

func TestMergeRecords_DuplicateFixtureAcrossShards(t *testing.T) {
	dir := t.TempDir()
	writeRecordsFile(t, filepath.Join(dir, "shard-1.json"), []profile.Record{{Fixture: "promql/a"}})
	writeRecordsFile(t, filepath.Join(dir, "shard-2.json"), []profile.Record{{Fixture: "promql/a"}})
	_, err := mergeRecords(filepath.Join(dir, "*.json"), 0)
	if err == nil {
		t.Fatal("expected an error when the same fixture appears in two shard files")
	}
	if !strings.Contains(err.Error(), "promql/a") {
		t.Errorf("error should name the duplicate fixture, got: %v", err)
	}
}

func TestMergeRecords_ConcatenatesAndSortsByFanFactor(t *testing.T) {
	dir := t.TempDir()
	writeRecordsFile(t, filepath.Join(dir, "shard-1.json"), []profile.Record{
		{Fixture: "promql/low", FanFactor: ptr(1.5)},
	})
	writeRecordsFile(t, filepath.Join(dir, "shard-2.json"), []profile.Record{
		{Fixture: "promql/high", FanFactor: ptr(9.0)},
		{Fixture: "promql/unmeasured", FanFactor: nil},
	})

	recs, err := mergeRecords(filepath.Join(dir, "*.json"), 2)
	if err != nil {
		t.Fatalf("mergeRecords: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("expected 3 merged records, got %d", len(recs))
	}
	// SortByFanFactor puts unmeasured (nil FanFactor) records FIRST, then
	// measured ones descending by fan factor — see its own comparator.
	if recs[0].Fixture != "promql/unmeasured" || recs[1].Fixture != "promql/high" || recs[2].Fixture != "promql/low" {
		t.Errorf("merged records not sorted by fan factor: %v", recs)
	}
}

func TestSummarize(t *testing.T) {
	recs := []profile.Record{
		{Fixture: "a", FanFactor: ptr(2.0)},
		{Fixture: "b", FanFactor: ptr(5.0)},
		{Fixture: "c", FanFactor: nil},
		{Fixture: "d", FanFactor: ptr(1.0), Err: "boom"},
	}
	nErr, nUnmeasured, maxFan := summarize(recs)
	if nErr != 1 {
		t.Errorf("nErr = %d, want 1", nErr)
	}
	if nUnmeasured != 1 {
		t.Errorf("nUnmeasured = %d, want 1", nUnmeasured)
	}
	if maxFan != 5.0 {
		t.Errorf("maxFan = %v, want 5.0", maxFan)
	}
}

func TestWriteJSON_ToFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.json")
	recs := []profile.Record{{Fixture: "promql/a", FanFactor: ptr(1.0)}}
	if err := writeJSON(out, recs); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got []profile.Record
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal written JSON: %v", err)
	}
	if len(got) != 1 || got[0].Fixture != "promql/a" {
		t.Errorf("round-tripped records = %v, want one record for promql/a", got)
	}
}

func TestWriteMarkdown(t *testing.T) {
	dir := t.TempDir()
	md := filepath.Join(dir, "summary.md")
	recs := []profile.Record{
		{Fixture: "promql/a", FanFactor: ptr(3.5), ScanRows: 10, PeakIntermediate: 35, HasCrossJoin: true},
		{Fixture: "promql/b", FanFactor: nil, ScanRows: 4, PeakIntermediate: 4},
	}
	if err := writeMarkdown(md, recs, 2, len(recs), 0, 1, 3.5); err != nil {
		t.Fatalf("writeMarkdown: %v", err)
	}
	data, err := os.ReadFile(md)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(data)
	for _, want := range []string{"promql/a", "3.50", "unmeasured", "✓"} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown output missing %q:\n%s", want, got)
		}
	}
}

func TestWriteMarkdown_AppendsRatherThanTruncates(t *testing.T) {
	dir := t.TempDir()
	md := filepath.Join(dir, "summary.md")
	if err := os.WriteFile(md, []byte("existing content\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := writeMarkdown(md, nil, 5, 0, 0, 0, 0); err != nil {
		t.Fatalf("writeMarkdown: %v", err)
	}
	data, err := os.ReadFile(md)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.HasPrefix(string(data), "existing content\n") {
		t.Errorf("writeMarkdown must append, not truncate; got:\n%s", data)
	}
}

func TestFanFactorLabel(t *testing.T) {
	if got := fanFactorLabel(nil); got != "unmeasured" {
		t.Errorf("fanFactorLabel(nil) = %q, want %q", got, "unmeasured")
	}
	if got := fanFactorLabel(ptr(2.5)); got != "2.50" {
		t.Errorf("fanFactorLabel(2.5) = %q, want %q", got, "2.50")
	}
}

func TestMdYesNo(t *testing.T) {
	if got := mdYesNo(true); got != "✓" {
		t.Errorf("mdYesNo(true) = %q, want a checkmark", got)
	}
	if got := mdYesNo(false); got != "" {
		t.Errorf("mdYesNo(false) = %q, want empty", got)
	}
}

func TestYesno(t *testing.T) {
	if got := yesno(true); got != "yes" {
		t.Errorf("yesno(true) = %q, want %q", got, "yes")
	}
	if got := yesno(false); got != "-" {
		t.Errorf("yesno(false) = %q, want %q", got, "-")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 48); got != "short" {
		t.Errorf("truncate(short) = %q, want unchanged", got)
	}
	long := strings.Repeat("x", 50)
	got := truncate(long, 10)
	// n-1 ASCII bytes plus one multi-byte "…" rune — byte length is
	// n-1+len("…"), not n.
	wantPrefix := strings.Repeat("x", 9)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("truncate(%d) = %q, want prefix %q", 10, got, wantPrefix)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated result must end with an ellipsis, got %q", got)
	}
}

func TestEmitReport_FailOverThreshold(t *testing.T) {
	dir := t.TempDir()
	recs := []profile.Record{{Fixture: "promql/a", FanFactor: ptr(10.0)}}

	code := emitReport(recs, filepath.Join(dir, "out.json"), "", 0, 5.0)
	if code != 2 {
		t.Errorf("emitReport exit code = %d, want 2 (max fan_factor exceeds -fail-over)", code)
	}

	code = emitReport(recs, filepath.Join(dir, "out2.json"), "", 0, 20.0)
	if code != 0 {
		t.Errorf("emitReport exit code = %d, want 0 (max fan_factor under -fail-over)", code)
	}

	code = emitReport(recs, filepath.Join(dir, "out3.json"), "", 0, 0)
	if code != 0 {
		t.Errorf("emitReport exit code = %d, want 0 (-fail-over=0 never gates)", code)
	}
}

func TestRunMerge_PropagatesMergeError(t *testing.T) {
	if code := runMerge(cliFlags{mergeGlob: ""}); code != 1 {
		t.Errorf("runMerge with an empty -merge glob: exit code = %d, want 1", code)
	}
}

func TestRunMerge_Succeeds(t *testing.T) {
	dir := t.TempDir()
	writeRecordsFile(t, filepath.Join(dir, "shard-1.json"), []profile.Record{{Fixture: "promql/a", FanFactor: ptr(1.0)}})
	out := filepath.Join(dir, "merged.json")
	code := runMerge(cliFlags{mergeGlob: filepath.Join(dir, "*.json"), outPath: out})
	if code != 0 {
		t.Fatalf("runMerge exit code = %d, want 0", code)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("runMerge did not write the merged report: %v", err)
	}
}
