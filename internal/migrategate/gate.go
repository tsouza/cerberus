// Package migrategate folds the machine-readable artifacts the other migration
// blocks emit — verify, classify, inventory, rulegraph — into a single
// cutover go/no-go decision. It is a pure, offline aggregator: it reads the
// JSON each block writes with its `--json` flag, applies a small set of
// conservative rules, and reports a per-stage checklist plus one overall
// PASS/FAIL verdict. It opens no network connection and runs no query.
//
// The gate REFUSES (a blocking stage → overall FAIL, non-zero exit), it never
// merely warns, on any blocking input. The rules (v1, deliberately
// conservative) are:
//
//   - verify   — BLOCK if any query diverged or errored (the parity gate found
//     cerberus returning different numbers, or a backend failing).
//   - classify — BLOCK if any corpus query is unsupported (no cerberus
//     equivalent). A supported-but-risky query WARNs but does not block.
//   - inventory — WARN (never block) when the source carries high-cardinality
//     metrics or labels: an OOM candidate to review, not a correctness stop.
//   - rulegraph — BLOCK if any recorded series is consumed: a dashboard or
//     alert depends on it, so it MUST keep being materialized after cutover;
//     dropping its materializer leaves a blank panel. Orphan recorded series
//     (nobody reads them) are safe and never block.
//
// A required artifact that was not supplied is itself a BLOCK: the gate cannot
// prove safety for a stage it never saw. verify, classify, and rulegraph are
// required; inventory is advisory (its worst outcome is a WARN), so a missing
// inventory artifact is reported but does not block.
package migrategate

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/migrateinventory"
	"github.com/tsouza/cerberus/internal/migrateverify"
)

// Stage names, stable across text and JSON output.
const (
	StageVerify    = "verify"
	StageClassify  = "classify"
	StageInventory = "inventory"
	StageRuleGraph = "rulegraph"
)

// Verdicts a stage can carry. FAIL and a missing REQUIRED artifact block the
// cutover; WARN and PASS do not; MISSING marks an artifact that was not
// supplied (blocking only when the stage is required).
const (
	VerdictPass    = "PASS"
	VerdictFail    = "FAIL"
	VerdictWarn    = "WARN"
	VerdictMissing = "MISSING"
)

// Overall decision strings.
const (
	OverallPass = "PASS"
	OverallFail = "FAIL"
)

// DefaultHighCardSeries is the head series count at or above which a single
// metric name is flagged as a high-cardinality OOM candidate (a WARN, never a
// block). A metric with this many active series drives a large fan-out.
const DefaultHighCardSeries int64 = 100_000

// DefaultHighCardLabelValues is the distinct-value count at or above which a
// single label name is flagged as high-cardinality: wide labels drive group-by
// and join fan-out.
const DefaultHighCardLabelValues int64 = 50_000

// jsonIndent matches the two-space indent the other migration blocks emit, so
// the gate's JSON reads the same as its inputs.
const jsonIndent = "  "

// Inputs holds the paths to the four artifact files. Every path is optional;
// an empty path means that artifact was not supplied, which the gate reports
// (and, for a required stage, blocks on).
type Inputs struct {
	Verify    string
	Classify  string
	Inventory string
	RuleGraph string
}

// Options tunes the advisory thresholds. Zero values fall back to the package
// defaults, so a caller can leave the struct empty for the standard gate.
type Options struct {
	HighCardSeries      int64
	HighCardLabelValues int64
}

func (o Options) highCardSeries() int64 {
	if o.HighCardSeries > 0 {
		return o.HighCardSeries
	}
	return DefaultHighCardSeries
}

func (o Options) highCardLabelValues() int64 {
	if o.HighCardLabelValues > 0 {
		return o.HighCardLabelValues
	}
	return DefaultHighCardLabelValues
}

// StageResult is one stage's line in the go/no-go checklist. Present reports
// whether the artifact was supplied; Blocking reports whether this stage
// prevents the cutover (a FAIL, or a required artifact that is missing).
// Reasons lists every rule that fired, in a deterministic order.
type StageResult struct {
	Stage    string   `json:"stage"`
	Present  bool     `json:"present"`
	Required bool     `json:"required"`
	Verdict  string   `json:"verdict"`
	Blocking bool     `json:"blocking"`
	Reasons  []string `json:"reasons"`
}

// Decision is the full go/no-go result: one entry per stage, the names of the
// artifacts that were missing, and the overall verdict. Pass is true — and the
// CLI exits 0 — only when no stage blocks.
type Decision struct {
	Pass    bool          `json:"pass"`
	Overall string        `json:"overall"`
	Stages  []StageResult `json:"stages"`
	Missing []string      `json:"missing"`
}

// Evaluate reads whichever artifacts were supplied, applies the gate rules, and
// returns the decision. A supplied-but-unreadable or unparseable artifact is a
// hard error (not a silent skip and not a gate FAIL): the operator handed the
// gate a file it cannot trust, which is a misconfiguration to surface, not a
// safety verdict to invent.
func Evaluate(in Inputs, opts Options) (Decision, error) {
	verify, err := evalVerify(in.Verify)
	if err != nil {
		return Decision{}, err
	}
	classify, err := evalClassify(in.Classify)
	if err != nil {
		return Decision{}, err
	}
	inventory, err := evalInventory(in.Inventory, opts)
	if err != nil {
		return Decision{}, err
	}
	rulegraph, err := evalRuleGraph(in.RuleGraph)
	if err != nil {
		return Decision{}, err
	}

	dec := Decision{Stages: []StageResult{verify, classify, inventory, rulegraph}}
	dec.Pass = true
	for _, s := range dec.Stages {
		if s.Blocking {
			dec.Pass = false
		}
		if !s.Present {
			dec.Missing = append(dec.Missing, s.Stage)
		}
	}
	if dec.Pass {
		dec.Overall = OverallPass
	} else {
		dec.Overall = OverallFail
	}
	return dec, nil
}

// evalVerify blocks when the parity gate found any divergence or backend error.
// verify is a required artifact: a cutover cannot be proven safe without it.
func evalVerify(path string) (StageResult, error) {
	res := StageResult{Stage: StageVerify, Required: true}
	if path == "" {
		return missingRequired(res, "--verify"), nil
	}
	var rep migrateverify.Report
	if err := readArtifact(path, &rep); err != nil {
		return StageResult{}, err
	}
	res.Present = true
	if d, e := rep.Summary.Diverge, rep.Summary.Error; d > 0 || e > 0 {
		res.Verdict = VerdictFail
		res.Blocking = true
		res.Reasons = append(res.Reasons,
			fmt.Sprintf("parity gate failed: %d diverge, %d error (of %d queries)", d, e, rep.Summary.Total))
		return res, nil
	}
	res.Verdict = VerdictPass
	return res, nil
}

// evalClassify blocks when any corpus query is unsupported (no cerberus
// equivalent). A supported-but-risky query WARNs but does not block. classify
// is a required artifact.
func evalClassify(path string) (StageResult, error) {
	res := StageResult{Stage: StageClassify, Required: true}
	if path == "" {
		return missingRequired(res, "--classify"), nil
	}
	var cl migrate.Classification
	if err := readArtifact(path, &cl); err != nil {
		return StageResult{}, err
	}
	res.Present = true
	if cl.Counts.Unsupported > 0 {
		res.Verdict = VerdictFail
		res.Blocking = true
		res.Reasons = append(res.Reasons,
			fmt.Sprintf("%d unsupported quer%s (no cerberus equivalent)",
				cl.Counts.Unsupported, plural(cl.Counts.Unsupported, "y", "ies")))
		for _, name := range unsupportedConstructs(cl) {
			res.Reasons = append(res.Reasons, "  unsupported: "+name)
		}
		return res, nil
	}
	if cl.Counts.Risky > 0 {
		res.Verdict = VerdictWarn
		res.Reasons = append(res.Reasons,
			fmt.Sprintf("%d supported quer%s carry an offline fan-out risk flag",
				cl.Counts.Risky, plural(cl.Counts.Risky, "y", "ies")))
		return res, nil
	}
	res.Verdict = VerdictPass
	return res, nil
}

// evalInventory WARNs (never blocks) when the source carries a high-cardinality
// metric or label. inventory is advisory: its worst outcome is a WARN, so a
// missing inventory artifact is reported but does not block the cutover.
func evalInventory(path string, opts Options) (StageResult, error) {
	res := StageResult{Stage: StageInventory, Required: false}
	if path == "" {
		res.Verdict = VerdictMissing
		res.Reasons = append(res.Reasons,
			"artifact not provided (--inventory); source cardinality risk unchecked")
		return res, nil
	}
	var inv migrateinventory.Inventory
	if err := readArtifact(path, &inv); err != nil {
		return StageResult{}, err
	}
	res.Present = true

	seriesLimit := opts.highCardSeries()
	for _, m := range inv.TopMetricsBySeries {
		if m.Value >= seriesLimit {
			res.Reasons = append(res.Reasons,
				fmt.Sprintf("high-cardinality metric %q: %d series (>= %d)", m.Name, m.Value, seriesLimit))
		}
	}
	labelLimit := opts.highCardLabelValues()
	for _, l := range inv.TopLabelsByValues {
		if l.Value >= labelLimit {
			res.Reasons = append(res.Reasons,
				fmt.Sprintf("high-cardinality label %q: %d values (>= %d)", l.Name, l.Value, labelLimit))
		}
	}

	if len(res.Reasons) > 0 {
		res.Verdict = VerdictWarn
		return res, nil
	}
	res.Verdict = VerdictPass
	return res, nil
}

// evalRuleGraph blocks when any recorded series is consumed: a dashboard or
// alert reads it, so it MUST keep being materialized after cutover — dropping
// its recording rule leaves a blank panel. Orphan recorded series (read by
// nobody) are safe to drop and never block. rulegraph is a required artifact.
func evalRuleGraph(path string) (StageResult, error) {
	res := StageResult{Stage: StageRuleGraph, Required: true}
	if path == "" {
		return missingRequired(res, "--rulegraph"), nil
	}
	var g migrate.RuleGraph
	if err := readArtifact(path, &g); err != nil {
		return StageResult{}, err
	}
	res.Present = true

	var consumed []migrate.RecordedNode
	for _, n := range g.Recorded {
		if n.Status == migrate.StatusConsumed {
			consumed = append(consumed, n)
		}
	}
	if len(consumed) > 0 {
		res.Verdict = VerdictFail
		res.Blocking = true
		res.Reasons = append(res.Reasons,
			fmt.Sprintf("%d consumed recorded series must stay materialized after cutover",
				len(consumed)))
		for _, n := range consumed {
			res.Reasons = append(res.Reasons,
				fmt.Sprintf("  %s <- %d consumer%s", n.Name, len(n.Consumers), plural(len(n.Consumers), "", "s")))
		}
		return res, nil
	}
	res.Verdict = VerdictPass
	return res, nil
}

// missingRequired marks a required stage whose artifact was not supplied as a
// blocking MISSING: the gate cannot prove that stage safe without seeing it.
func missingRequired(res StageResult, flag string) StageResult {
	res.Verdict = VerdictMissing
	res.Blocking = true
	res.Reasons = append(res.Reasons,
		fmt.Sprintf("required artifact not provided (%s); cannot prove safety", flag))
	return res
}

// unsupportedConstructs returns the sorted, deduplicated set of rejection
// messages across the unsupported queries, so the gate names WHAT is
// unsupported rather than only counting.
func unsupportedConstructs(cl migrate.Classification) []string {
	seen := map[string]struct{}{}
	for _, q := range cl.Queries {
		if q.Bucket == migrate.BucketUnsupported && q.Construct != "" {
			seen[q.Construct] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// plural picks the singular or plural suffix for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// readArtifact reads and JSON-decodes one artifact file into v. A read or
// decode failure is wrapped with the path so the operator can tell which
// artifact is at fault.
func readArtifact(path string, v any) error {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied artifact path; offline CLI input.
	if err != nil {
		return fmt.Errorf("read artifact %q: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse artifact %q: %w", path, err)
	}
	return nil
}
