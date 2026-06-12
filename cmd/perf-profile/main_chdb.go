//go:build chdb

// Command perf-profile is the corpus-wide perf profiler entrypoint
// (Component B of the perf-assessment framework). It walks every
// executable TXTAR fixture under test/spec/** and emits one structured
// fan-out profile per fixture.
//
// It is built with the `chdb` tag (requires libchdb.so — `just
// chdb-install`) because the profiling work runs in-process against an
// embedded chDB engine. Without the tag, the build falls back to the
// stub in main_nochdb.go which prints an instruction and exits non-zero.
//
// Usage:
//
//	perf-profile -spec test/spec -out profile.json [-top 25]
//
// Flags:
//
//	-spec   root directory of the TXTAR corpus (default "test/spec")
//	-out    path to write the JSON profile array (default stdout)
//	-top    print the top-N fan_factor fixtures to stderr as a table
//	        (default 25; 0 disables)
//	-fail-over  exit non-zero if any fixture's fan_factor exceeds this
//	            threshold (default 0 = never fail; nightly lane reports,
//	            it does not gate)
//
// The JSON output is an array of profile.Record. The nightly
// perf-profile.yml lane uploads it as an artifact and renders the
// top-fan_factor table into the job step summary.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/tsouza/cerberus/test/perf/profile"
)

func main() {
	specDir := flag.String("spec", "test/spec", "root directory of the TXTAR corpus")
	outPath := flag.String("out", "", "path to write the JSON profile array (default stdout)")
	mdPath := flag.String("md", "", "path to append a markdown top-fan_factor table (for GITHUB_STEP_SUMMARY)")
	top := flag.Int("top", 25, "print the top-N fan_factor fixtures to stderr (0 disables)")
	failOver := flag.Float64("fail-over", 0, "exit non-zero if any fixture fan_factor exceeds this (0 = never)")
	flag.Parse()

	recs, err := profile.ProfileCorpus(*specDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf-profile: %v\n", err)
		os.Exit(1)
	}

	if err := writeJSON(*outPath, recs); err != nil {
		fmt.Fprintf(os.Stderr, "perf-profile: write output: %v\n", err)
		os.Exit(1)
	}

	if *top > 0 {
		printTopTable(os.Stderr, recs, *top)
	}

	// Summary line: total fixtures, how many had errors.
	var nErr int
	var maxFan float64
	for _, r := range recs {
		if r.Err != "" {
			nErr++
		}
		if r.FanFactor > maxFan {
			maxFan = r.FanFactor
		}
	}

	if *mdPath != "" {
		n := *top
		if n <= 0 {
			n = 25
		}
		if err := writeMarkdown(*mdPath, recs, n, len(recs), nErr, maxFan); err != nil {
			fmt.Fprintf(os.Stderr, "perf-profile: write markdown summary: %v\n", err)
			os.Exit(1)
		}
	}
	fmt.Fprintf(os.Stderr, "perf-profile: profiled %d fixtures (%d with errors), max fan_factor = %.2f\n",
		len(recs), nErr, maxFan)

	if *failOver > 0 && maxFan > *failOver {
		fmt.Fprintf(os.Stderr, "perf-profile: FAIL — max fan_factor %.2f exceeds threshold %.2f\n", maxFan, *failOver)
		os.Exit(2)
	}
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
func writeMarkdown(mdPath string, recs []profile.Record, n, total, nErr int, maxFan float64) error {
	if n > len(recs) {
		n = len(recs)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## perf-profile (corpus fan-out)\n\n")
	fmt.Fprintf(&b, "Profiled **%d** executable fixtures (%d with errors). Max fan_factor: **%.2f**.\n\n",
		total, nErr, maxFan)
	fmt.Fprintf(&b, "### top %d fixtures by fan_factor\n\n", n)
	b.WriteString("| fixture | fan_factor | scan_rows | peak_intermediate | result_rows | cross_join | array_join | recursive_cte |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | :---: | :---: | :---: |\n")
	for i := 0; i < n; i++ {
		r := recs[i]
		fmt.Fprintf(&b, "| `%s` | %.2f | %d | %d | %d | %s | %s | %s |\n",
			r.Fixture, r.FanFactor, r.ScanRows, r.PeakIntermediate, r.ResultRows,
			mdYesNo(r.HasCrossJoin), mdYesNo(r.HasArrayJoin), mdYesNo(r.HasRecursiveCTE))
	}
	b.WriteString("\n_fan_factor = peak intermediate cardinality / leaf scan rows. " +
		"Fixtures are small golden seeds, so absolute counts are tiny; the ratio is the fan-out signal._\n\n")

	f, err := os.OpenFile(mdPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // step-summary path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, werr := f.WriteString(b.String())
	return werr
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
		fmt.Fprintf(w, "%-48s %10.2f %12d %12d %6s %6s %6s\n",
			truncate(r.Fixture, 48), r.FanFactor, r.ScanRows, r.PeakIntermediate,
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
