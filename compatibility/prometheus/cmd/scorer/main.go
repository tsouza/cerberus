// Command prometheus-compat-scorer reads a promql-compliance-tester
// JSON report and emits a shields.io endpoint-badge "compat-score
// JSON" alongside it.
//
// The upstream `promql-compliance-tester` (built from the vendored
// `compatibility/prometheus/upstream` submodule of
// prometheus/compliance) writes its own report JSON, but it does not
// emit the shields.io endpoint-badge envelope cerberus's downstream
// dashboard + badge need. Forking the upstream tester to add that
// emit would mean carrying a patch on top of the submodule, which
// the upstream-forks playbook tries to avoid for unpatched
// dependencies. Instead, the shell harness invokes the upstream
// tester to produce report.json AND this in-tree post-processor to
// derive compat-score.json from it.
//
// Per task #68 ("compat is informational"), the wrapping shell
// script swallows the tester's non-zero exit on parity diffs and
// always exits 0 in the parity-drift path. Only hard infrastructure
// failures (build failure, compose-up failure, scorer can't parse
// report.json) escalate. This binary is one step in that pipeline:
// it reads the tester's output and writes the shields envelope; the
// shell script then ignores any per-case diff exit from the tester.
//
// Score semantics (mirrors compatibility/loki/cmd/loki-compliance-
// tester's scoreCounts):
//
//   - total counts every result row in the report.
//   - passed counts rows where both `diff` and `unexpectedFailure`
//     are empty AND `unexpectedSuccess` is false. These are the
//     "fully agreed with the reference" cases.
//   - per-case diffs, unexpected failures (including unsupported
//     queries), and unexpected successes contribute to total but
//     not to passed.
//   - the upstream tester does not surface "skipped" cases in
//     report.json (skip entries are pre-filtered at config-load
//     time), so there's no skip-exclusion branch like in the loki
//     scorer.
//
// Exit codes:
//
//   - 0: report.json parsed and score JSON written.
//   - 1: cannot read or parse report.json, or cannot write score JSON.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/tsouza/cerberus/compatibility/internal/score"
)

// reportEnvelope is the subset of promql-compliance-tester's JSON
// output we need. The upstream shape (see
// `compatibility/prometheus/upstream/promql/output/json.go`) carries
// more fields — `totalResults`, `includePassing`, optional query
// tweaks — but our scoring only depends on the per-result flag
// quartet. Decoding the minimal subset keeps this binary
// independent of upstream schema additions.
type reportEnvelope struct {
	Results []reportResult `json:"results"`
}

// reportResult mirrors the four flag fields the upstream tester
// emits per query. Empty strings + false booleans mean "passed";
// any non-zero value means the case diverged.
type reportResult struct {
	// Diff is non-empty when the structural diff between the two
	// backends reported a mismatch.
	Diff string `json:"diff"`

	// UnexpectedFailure is non-empty when the test backend returned
	// an error that the suite did not expect.
	UnexpectedFailure string `json:"unexpectedFailure"`

	// UnexpectedSuccess is true when the case was tagged should_fail
	// but actually succeeded.
	UnexpectedSuccess bool `json:"unexpectedSuccess"`
}

// passed reports whether this case contributes to the "passed"
// numerator. Mirrors loki's `Result.success()` and tempo's
// "no HardError && Diff.Equal && no Assertions" predicate.
func (r reportResult) passed() bool {
	return r.Diff == "" && r.UnexpectedFailure == "" && !r.UnexpectedSuccess
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("prometheus-compat-scorer: %v", err)
	}
}

func run() error {
	var (
		reportPath = flag.String("report", "", "promql-compliance-tester report JSON (required)")
		scorePath  = flag.String("score", "", "shields.io endpoint-badge score JSON output (required)")
		label      = flag.String("label", "prometheus compat", "badge label text")
	)
	flag.Parse()

	if *reportPath == "" || *scorePath == "" {
		return errors.New("both -report and -score are required")
	}

	data, err := os.ReadFile(*reportPath) //nolint:gosec // CLI-supplied trusted path
	if err != nil {
		return fmt.Errorf("read report %s: %w", *reportPath, err)
	}

	var env reportEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("parse report %s: %w", *reportPath, err)
	}

	passed, total := tally(env.Results)
	s := score.Compute(*label, passed, total)
	if err := score.Write(*scorePath, s); err != nil {
		return fmt.Errorf("write score: %w", err)
	}

	fmt.Fprintf(os.Stderr,
		"==> score: passed=%d total=%d percent=%.2f color=%s -> %s\n",
		passed, total, s.Percent, s.Color, *scorePath)
	return nil
}

// tally derives (passed, total) from the report's per-result rows.
// Every row counts toward total; rows that pass on all four flag
// dimensions contribute to passed.
func tally(results []reportResult) (passed, total int) {
	for _, r := range results {
		total++
		if r.passed() {
			passed++
		}
	}
	return passed, total
}
