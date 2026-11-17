package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The committed bound file, relative to this package.
const testBaseline = "../../nightly-baseline.json"

// writeRunners writes one rounds file per runner, each measuring sentinel on
// ClickHouse 26.9 with the given rounds, and returns their paths.
func writeRunners(t *testing.T, sentinel string, perRunner ...[]uint64) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, 0, len(perRunner))
	for i, rounds := range perRunner {
		buf, err := json.Marshal(map[string]any{
			"clickhouse": "26.9",
			"runner":     fmt.Sprintf("runner-%d", i),
			"sentinels":  []map[string]any{{"name": sentinel, "round_max_bytes": rounds}},
		})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, fmt.Sprintf("rounds-%d.json", i))
		if err := os.WriteFile(path, buf, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

// TestRun_ExitStatusAndFileTellTheOutcomesApart pins the contract
// perf-nightly-aggregate.mjs reads: exitCombined only for a file written with
// no refusal; exitFailed with the file written for a refusal, and with no
// file for a combine that could not write one.
func TestRun_ExitStatusAndFileTellTheOutcomesApart(t *testing.T) {
	const sentinel = "label_values_matcher_stock_key_renamed"
	settled := []uint64{29_000_000, 29_100_000, 29_200_000, 29_300_000, 29_400_000}
	// The sentinel's 1.40x of 29.4M leaves an 11.76M margin; 10M spans 19.4M.
	low := []uint64{10_000_000, 29_100_000, 29_200_000, 29_300_000, 29_400_000}
	for _, tc := range []struct {
		name        string
		args        []string
		wantStatus  int
		wantWritten bool
	}{
		{"accepted", writeRunners(t, sentinel, settled, settled, settled, settled), exitCombined, true},
		{"refused", writeRunners(t, sentinel, settled, settled, settled, low), exitFailed, true},
		{"unknown sentinel", writeRunners(t, "no_such_sentinel", settled, settled, settled, settled), exitFailed, false},
		{"no rounds files", nil, exitFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "nightly-baseline.json")
			args := append([]string{"-baseline", testBaseline, "-out", out}, tc.args...)
			if got := run(args, io.Discard, io.Discard); got != tc.wantStatus {
				t.Errorf("exit %d, want %d", got, tc.wantStatus)
			}
			_, err := os.Stat(out)
			if written := err == nil; written != tc.wantWritten {
				t.Errorf("written %v, want %v (stat: %v)", written, tc.wantWritten, err)
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}
