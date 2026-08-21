// Deliberately NOT `//go:build chdb`. Everything here reads/writes/ranks
// []profile.Record — plain JSON in, a markdown/text table out — and never
// opens a chDB session. Keeping it untagged is what lets `-merge` (folding N
// shards' JSON outputs into one combined report) run in the DEFAULT build,
// with no libchdb.so and no `chdb` tag: the perf-profile.yml aggregator job
// that combines the profile-shard matrix's outputs needs none of the chDB
// toolchain main_chdb.go's actual profiling requires. main_chdb.go (single-
// shard profiling) and main_nochdb.go (the merge-capable stub) both call into
// this file.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tsouza/cerberus/test/perf/profile"
)

// cliFlags is the flag set shared verbatim by main_chdb.go and
// main_nochdb.go, so `-h` output and flag semantics are identical whichever
// build tag produced the binary.
type cliFlags struct {
	specDir      string
	outPath      string
	mdPath       string
	top          int
	failOver     float64
	mergeGlob    string
	expectShards int
}

// parseFlags registers and parses the flags both entrypoints accept.
func parseFlags() cliFlags {
	var f cliFlags
	flag.StringVar(&f.specDir, "spec", "test/spec", "root directory of the TXTAR corpus")
	flag.StringVar(&f.outPath, "out", "", "path to write the JSON profile array (default stdout)")
	flag.StringVar(&f.mdPath, "md", "", "path to append a markdown top-fan_factor table (for GITHUB_STEP_SUMMARY)")
	flag.IntVar(&f.top, "top", 25, "print the top-N fan_factor fixtures to stderr (0 disables)")
	flag.Float64Var(&f.failOver, "fail-over", 0, "exit non-zero if any fixture fan_factor exceeds this (0 = never)")
	flag.StringVar(&f.mergeGlob, "merge", "",
		"glob of shard JSON files to MERGE into one combined report, instead of profiling. "+
			"Needs no chDB — works in the tag-free build too.")
	flag.IntVar(&f.expectShards, "expect-shards", 0,
		"(-merge only) fail unless exactly this many files matched -merge (0 = don't check)")
	flag.Parse()
	return f
}

// runMerge implements `-merge`: read every file -merge's glob matches as a
// []profile.Record JSON array, concatenate, and run the same report pipeline
// -spec's direct profiling run would (writeJSON / printTopTable / writeMarkdown
// / -fail-over). Returns the process exit code.
func runMerge(f cliFlags) int {
	recs, err := mergeRecords(f.mergeGlob, f.expectShards)
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf-profile: merge: %v\n", err)
		return 1
	}
	return emitReport(recs, f.outPath, f.mdPath, f.top, f.failOver)
}

// mergeRecords globs pattern, decodes each match as a []profile.Record JSON
// array, and returns every record concatenated and re-sorted by fan factor.
//
// A fixture ID appearing in more than one input file is a HARD error rather
// than a silently-deduplicated or silently-doubled record: the corpus
// partition (test/perf/profile/shard.go) guarantees a disjoint cover, so a
// duplicate means the shard count the matrix ran with does not match the
// count the aggregator was told to expect, or a shard re-ran and both
// artifacts got merged — either way the combined report is not trustworthy
// until that is fixed at the source.
func mergeRecords(pattern string, expectShards int) ([]profile.Record, error) {
	if pattern == "" {
		return nil, fmt.Errorf("empty -merge pattern")
	}
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob %q: %w", pattern, err)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("-merge %q matched no files", pattern)
	}
	if expectShards > 0 && len(paths) != expectShards {
		return nil, fmt.Errorf("-merge %q matched %d file(s), expected exactly %d (-expect-shards) — "+
			"a shard's artifact may be missing, or the matrix's shard count drifted from what the "+
			"aggregator was told to expect", pattern, len(paths), expectShards)
	}

	owner := make(map[string]string) // fixture ID -> the file it first appeared in
	var merged []profile.Record
	for _, path := range paths {
		data, rerr := os.ReadFile(path) //nolint:gosec // CI artifact path, not user input
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", path, rerr)
		}
		var recs []profile.Record
		if uerr := json.Unmarshal(data, &recs); uerr != nil {
			return nil, fmt.Errorf("decode %s: %w", path, uerr)
		}
		for _, r := range recs {
			if prior, dup := owner[r.Fixture]; dup {
				return nil, fmt.Errorf("fixture %q appears in both %s and %s — shard partitions must be disjoint",
					r.Fixture, prior, path)
			}
			owner[r.Fixture] = path
		}
		merged = append(merged, recs...)
	}

	profile.SortByFanFactor(merged)
	return merged, nil
}

// emitReport is the report pipeline shared by a direct profiling run and a
// merge run: write the JSON array, print the top-N table, append the
// markdown step summary, print the one-line corpus summary, and apply
// -fail-over. Returns the process exit code.
func emitReport(recs []profile.Record, outPath, mdPath string, top int, failOver float64) int {
	if err := writeJSON(outPath, recs); err != nil {
		fmt.Fprintf(os.Stderr, "perf-profile: write output: %v\n", err)
		return 1
	}

	if top > 0 {
		printTopTable(os.Stderr, recs, top)
	}

	nErr, nUnmeasured, maxFan := summarize(recs)

	if mdPath != "" {
		n := top
		if n <= 0 {
			n = 25
		}
		if err := writeMarkdown(mdPath, recs, n, len(recs), nErr, nUnmeasured, maxFan); err != nil {
			fmt.Fprintf(os.Stderr, "perf-profile: write markdown summary: %v\n", err)
			return 1
		}
	}
	fmt.Fprintf(os.Stderr, "perf-profile: profiled %d fixtures (%d with errors, %d unmeasured), max fan_factor = %.2f\n",
		len(recs), nErr, nUnmeasured, maxFan)

	if failOver > 0 && maxFan > failOver {
		fmt.Fprintf(os.Stderr, "perf-profile: FAIL — max fan_factor %.2f exceeds threshold %.2f\n", maxFan, failOver)
		return 2
	}
	return 0
}

// summarize computes the three headline numbers the report's summary line
// and markdown table share: how many fixtures errored, how many are
// unmeasured (nil FanFactor — see [profile.Record.FanFactor]), and the
// highest FanFactor among the measured ones.
func summarize(recs []profile.Record) (nErr, nUnmeasured int, maxFan float64) {
	for _, r := range recs {
		if r.Err != "" {
			nErr++
		}
		if r.FanFactor == nil {
			nUnmeasured++
			continue
		}
		if *r.FanFactor > maxFan {
			maxFan = *r.FanFactor
		}
	}
	return nErr, nUnmeasured, maxFan
}

// writeJSON marshals recs as an indented JSON array to outPath, or to
// stdout when outPath is empty.
func writeJSON(outPath string, recs []profile.Record) error {
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if outPath == "" {
		_, werr := os.Stdout.Write(data)
		return werr
	}
	return os.WriteFile(outPath, data, 0o644) //nolint:gosec // profile artifact, not a secret
}

// writeMarkdown appends a GitHub-flavoured markdown table of the top-n
// fan_factor fixtures plus a one-line corpus summary to mdPath (opened
// in append mode so it can target $GITHUB_STEP_SUMMARY directly).
func writeMarkdown(mdPath string, recs []profile.Record, n, total, nErr, nUnmeasured int, maxFan float64) error {
	if n > len(recs) {
		n = len(recs)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## perf-profile (corpus fan-out)\n\n")
	fmt.Fprintf(&b, "Profiled **%d** executable fixtures (%d with errors, %d unmeasured). Max fan_factor: **%.2f**.\n\n",
		total, nErr, nUnmeasured, maxFan)
	fmt.Fprintf(&b, "### top %d fixtures by fan_factor\n\n", n)
	b.WriteString("| fixture | fan_factor | scan_rows | peak_intermediate | result_rows | cross_join | array_join | recursive_cte |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | :---: | :---: | :---: |\n")
	for i := 0; i < n; i++ {
		r := recs[i]
		fmt.Fprintf(&b, "| `%s` | %s | %d | %d | %d | %s | %s | %s |\n",
			r.Fixture, fanFactorLabel(r.FanFactor), r.ScanRows, r.PeakIntermediate, r.ResultRows,
			mdYesNo(r.HasCrossJoin), mdYesNo(r.HasArrayJoin), mdYesNo(r.HasRecursiveCTE))
	}
	b.WriteString("\n_fan_factor = peak intermediate cardinality / leaf scan rows. " +
		"Fixtures are small golden seeds, so absolute counts are tiny; the ratio is the fan-out signal. " +
		"`unmeasured` means the profiler could not see through every pipeline stage (a CTE reference or a " +
		"recursive CTE step) — see [profile.Record.FanFactor]._\n\n")

	f, err := os.OpenFile(mdPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // step-summary path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, werr := f.WriteString(b.String())
	return werr
}

// fanFactorLabel renders a nullable fan_factor for the human-facing
// summary tables — nil (unmeasured) is spelled out rather than
// collapsed to a fabricated number.
func fanFactorLabel(f *float64) string {
	if f == nil {
		return "unmeasured"
	}
	return fmt.Sprintf("%.2f", *f)
}

func mdYesNo(v bool) string {
	if v {
		return "✓"
	}
	return ""
}

// printTopTable renders the top-n records (already sorted descending by
// fan factor) as a fixed-width table to w. Used for the nightly
// step-summary preview and local runs.
func printTopTable(w *os.File, recs []profile.Record, n int) {
	if n > len(recs) {
		n = len(recs)
	}
	fmt.Fprintf(w, "\n=== top %d fixtures by fan_factor ===\n", n)
	fmt.Fprintf(w, "%-48s %10s %12s %12s %6s %6s %6s\n",
		"fixture", "fan_factor", "scan_rows", "peak_inter", "xjoin", "ajoin", "rcte")
	for i := 0; i < n; i++ {
		r := recs[i]
		fmt.Fprintf(w, "%-48s %10s %12d %12d %6s %6s %6s\n",
			truncate(r.Fixture, 48), fanFactorLabel(r.FanFactor), r.ScanRows, r.PeakIntermediate,
			yesno(r.HasCrossJoin), yesno(r.HasArrayJoin), yesno(r.HasRecursiveCTE))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}
