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
//     cerberus returning different numbers, or a backend failing), or if the run
//     verified ZERO queries (an empty harvest proves nothing and must not
//     green-light a cutover). WARN when the report carries UNCHECKED entries —
//     queries that emitted SQL but returned no comparable matrix live
//     (Unsupported), harvest-skipped entries, or out-of-scope (non-PromQL)
//     entries — so a green parity gate never hides an unexamined input.
//   - classify — BLOCK if any corpus query is unsupported (no cerberus
//     equivalent), or if ZERO queries were classified (an empty harvest cannot
//     prove support). A supported-but-risky query WARNs but does not block; a
//     harvest-skipped corpus entry (never classified) also WARNs.
//   - inventory — WARN (never block) when the source carries high-cardinality
//     metrics or labels: an OOM candidate to review, not a correctness stop.
//     Also WARNs on every honesty caveat the probe recorded (an optional
//     enrichment it could not fetch), so an enrichment failure never reads clean.
//   - rulegraph — BLOCK if any recorded series is consumed: a dashboard or
//     alert depends on it, so it MUST keep being materialized after cutover;
//     dropping its materializer leaves a blank panel. Orphan recorded series
//     (nobody reads them) are safe and never block — BUT any SKIPPED input (an
//     unparseable consumer expr, an unreadable rule file) also BLOCKS, because a
//     dropped consumer that referenced a series would have marked it consumed;
//     with a skip, "orphan ⇒ safe to drop" is unsound and the stage can no
//     longer prove a panel won't go blank.
//
// A required artifact that was not supplied is itself a BLOCK: the gate cannot
// prove safety for a stage it never saw. verify, classify, and rulegraph are
// required; inventory is advisory (its worst outcome is a WARN), so a missing
// inventory artifact is reported but does not block.
//
// Every artifact is decoded strictly (unknown fields rejected) and its stamped
// schema_version is checked against the version this gate build understands. A
// schema-drifted, wrong-type, or version-mismatched artifact is a hard error, not
// a struct that zero-fills its counts to a silent PASS.
//
// Every count each upstream block deliberately preserves — the skip/unchecked/
// caveat tallies — is threaded into a stage's Reasons here, never silently
// re-dropped at the aggregation layer.
package migrategate

import (
	"bytes"
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
	if err := requireSchemaVersion(path, rep.SchemaVersion, migrateverify.ReportVersion); err != nil {
		return StageResult{}, err
	}
	res.Present = true
	s := rep.Summary
	caveats := verifyCaveats(s)
	if s.Total == 0 {
		// A parity run that replayed zero queries proves nothing — an empty
		// harvest must never green-light a cutover with nothing verified.
		res.Verdict = VerdictFail
		res.Blocking = true
		res.Reasons = append(res.Reasons, "nothing verified: the parity run replayed 0 queries (an empty corpus cannot prove parity)")
		res.Reasons = append(res.Reasons, caveats...)
		return res, nil
	}
	if s.Diverge > 0 || s.Error > 0 {
		res.Verdict = VerdictFail
		res.Blocking = true
		res.Reasons = append(res.Reasons,
			fmt.Sprintf("parity gate failed: %d diverge, %d error (of %d queries)", s.Diverge, s.Error, s.Total))
		res.Reasons = append(res.Reasons, caveats...)
		return res, nil
	}
	if len(caveats) > 0 {
		res.Verdict = VerdictWarn
		res.Reasons = append(res.Reasons, caveats...)
		return res, nil
	}
	res.Verdict = VerdictPass
	return res, nil
}

// verifyCaveats surfaces the parity counts that Report.Failed() deliberately
// does NOT block on but that each leave a query UNCHECKED against a reference
// backend: a query cerberus emitted SQL for whose live response was not a
// comparable matrix (Unsupported — e.g. a non-200 or non-matrix body), a corpus
// entry that never became a replayable query (HarvestSkipped), and a non-PromQL
// entry with no Prometheus baseline (OutOfScope). Each is an unexamined input
// the operator must see before trusting a green parity gate — counted here,
// never silently dropped.
func verifyCaveats(s migrateverify.Summary) []string {
	var out []string
	if s.Unsupported > 0 {
		out = append(out, fmt.Sprintf(
			"%d quer%s emitted SQL but returned no comparable matrix live (unverified against the backend)",
			s.Unsupported, plural(s.Unsupported, "y", "ies"),
		))
	}
	if s.HarvestSkipped > 0 {
		out = append(out, fmt.Sprintf(
			"%d harvest-skipped corpus entr%s never became a replayable query (unchecked)",
			s.HarvestSkipped, plural(s.HarvestSkipped, "y", "ies"),
		))
	}
	if s.OutOfScope > 0 {
		out = append(out, fmt.Sprintf(
			"%d out-of-scope entr%s (not PromQL; no Prometheus baseline, unchecked)",
			s.OutOfScope, plural(s.OutOfScope, "y", "ies"),
		))
	}
	return out
}

// evalClassify blocks when any corpus query is unsupported (no cerberus
// equivalent). A supported-but-risky query WARNs but does not block; a
// harvest-skipped corpus entry (carried through but never classified) also
// WARNs. classify is a required artifact.
func evalClassify(path string) (StageResult, error) {
	res := StageResult{Stage: StageClassify, Required: true}
	if path == "" {
		return missingRequired(res, "--classify"), nil
	}
	var cl migrate.Classification
	if err := readArtifact(path, &cl); err != nil {
		return StageResult{}, err
	}
	if err := requireSchemaVersion(path, cl.SchemaVersion, migrate.ClassificationVersion); err != nil {
		return StageResult{}, err
	}
	res.Present = true
	if cl.Counts.Total == 0 {
		// Classifying zero queries proves no cerberus-support coverage — an empty
		// harvest must never green-light a cutover with nothing classified.
		res.Verdict = VerdictFail
		res.Blocking = true
		res.Reasons = append(res.Reasons, "nothing classified: 0 corpus queries were bucketed (an empty corpus cannot prove support)")
		res.Reasons = append(res.Reasons, classifyHarvestSkips(cl)...)
		return res, nil
	}
	if cl.Counts.Unsupported > 0 {
		res.Verdict = VerdictFail
		res.Blocking = true
		res.Reasons = append(res.Reasons,
			fmt.Sprintf("%d unsupported quer%s (no cerberus equivalent)",
				cl.Counts.Unsupported, plural(cl.Counts.Unsupported, "y", "ies")))
		for _, name := range unsupportedConstructs(cl) {
			res.Reasons = append(res.Reasons, "  unsupported: "+name)
		}
		res.Reasons = append(res.Reasons, classifyHarvestSkips(cl)...)
		return res, nil
	}
	var warn []string
	if cl.Counts.Risky > 0 {
		warn = append(warn, fmt.Sprintf(
			"%d supported quer%s carry an offline fan-out risk flag",
			cl.Counts.Risky, plural(cl.Counts.Risky, "y", "ies"),
		))
	}
	warn = append(warn, classifyHarvestSkips(cl)...)
	if len(warn) > 0 {
		res.Verdict = VerdictWarn
		res.Reasons = append(res.Reasons, warn...)
		return res, nil
	}
	res.Verdict = VerdictPass
	return res, nil
}

// classifyHarvestSkips surfaces the corpus entries the harvester could not turn
// into a query (an unreadable file, a YAML parse failure, a rule with no
// expression). classify carries them through "so the skip count never gets
// lost"; the gate must report them, because a skipped consumer is a query that
// was NEVER classified — an unexamined input that would otherwise hide behind a
// green classify.
func classifyHarvestSkips(cl migrate.Classification) []string {
	if len(cl.Skipped) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"%d harvest-skipped corpus entr%s never classified (unexamined)",
		len(cl.Skipped), plural(len(cl.Skipped), "y", "ies"),
	)}
}

// enrichmentUnavailable is the sentinel migrateinventory writes into a *Total
// field when its optional enrichment probe could not be fetched: the count is
// unknown, not zero. The gate surfaces it as a caveat so a probe that silently
// failed an enrichment does not read as a clean inventory.
const enrichmentUnavailable = -1

// evalInventory WARNs (never blocks) when the source carries a high-cardinality
// metric or label, or when the probe recorded an honesty caveat (an optional
// enrichment it could not fetch). inventory is advisory: its worst outcome is a
// WARN, so a missing inventory artifact is reported but does not block the
// cutover.
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
	if err := requireSchemaVersion(path, inv.SchemaVersion, migrateinventory.InventoryVersion); err != nil {
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
	res.Reasons = append(res.Reasons, inventoryCaveats(inv)...)

	if len(res.Reasons) > 0 {
		res.Verdict = VerdictWarn
		return res, nil
	}
	res.Verdict = VerdictPass
	return res, nil
}

// inventoryCaveats surfaces every honesty caveat the inventory carries: the
// optional-enrichment failures the probe recorded in Notes (which the inventory
// keeps "counted, never silently dropped"), plus an enrichment-not-obtained
// signal (a -1 total) for a note-less artifact. The gate must not re-drop them:
// an enrichment the probe could not fetch is an unchecked corner of the source,
// not a clean bill of health.
func inventoryCaveats(inv migrateinventory.Inventory) []string {
	out := append([]string(nil), inv.Notes...)
	if len(inv.Notes) > 0 {
		// A recorded Note already explains each -1 total; the probe appends one
		// per failed enrichment. Surfacing the notes covers the -1 case without
		// double-reporting it.
		return out
	}
	if inv.MetricNameTotal == enrichmentUnavailable {
		out = append(out, "distinct metric-name total not obtained (enrichment unavailable)")
	}
	if inv.MetadataMetricTotal == enrichmentUnavailable {
		out = append(out, "metric metadata total not obtained (enrichment unavailable)")
	}
	return out
}

// evalRuleGraph blocks when any recorded series is consumed: a dashboard or
// alert reads it, so it MUST keep being materialized after cutover — dropping
// its recording rule leaves a blank panel. Orphan recorded series (read by
// nobody) are safe to drop and never block — BUT any SKIPPED input also blocks:
// the builder could not use it (an unparseable consumer expr, an unreadable rule
// file), so a consumer that referenced a recorded series may have been dropped,
// leaving that series wrongly classified orphan. With a skip, "orphan ⇒ safe to
// drop" is unsound, so the gate refuses rather than green-light dropping a
// materializer the skipped consumer needs. rulegraph is a required artifact.
func evalRuleGraph(path string) (StageResult, error) {
	res := StageResult{Stage: StageRuleGraph, Required: true}
	if path == "" {
		return missingRequired(res, "--rulegraph"), nil
	}
	var g migrate.RuleGraph
	if err := readArtifact(path, &g); err != nil {
		return StageResult{}, err
	}
	if err := requireSchemaVersion(path, g.SchemaVersion, migrate.RuleGraphVersion); err != nil {
		return StageResult{}, err
	}
	res.Present = true

	var consumed []migrate.RecordedNode
	for _, n := range g.Recorded {
		if n.Status == migrate.StatusConsumed {
			consumed = append(consumed, n)
		}
	}
	// A skip invalidates the orphan classification the whole stage rests on, so
	// it blocks alongside any consumed series.
	blockingSkip := g.Counts.Skipped > 0
	if len(consumed) == 0 && !blockingSkip {
		res.Verdict = VerdictPass
		return res, nil
	}

	res.Verdict = VerdictFail
	res.Blocking = true
	if len(consumed) > 0 {
		res.Reasons = append(res.Reasons,
			fmt.Sprintf("%d consumed recorded series must stay materialized after cutover",
				len(consumed)))
		for _, n := range consumed {
			res.Reasons = append(res.Reasons,
				fmt.Sprintf("  %s <- %d consumer%s", n.Name, len(n.Consumers), plural(len(n.Consumers), "", "s")))
		}
	}
	if blockingSkip {
		res.Reasons = append(res.Reasons,
			fmt.Sprintf("%d skipped input%s make the orphan classification unsound (a dropped consumer may reference a series marked orphan)",
				g.Counts.Skipped, plural(g.Counts.Skipped, "", "s")))
		for _, sk := range g.Skipped {
			res.Reasons = append(res.Reasons,
				fmt.Sprintf("  skipped %s: %s", sk.Source, sk.Reason))
		}
	}
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

// readArtifact reads and strictly JSON-decodes one artifact file into v. A read
// or decode failure is wrapped with the path so the operator can tell which
// artifact is at fault. Decoding uses DisallowUnknownFields so a schema-drifted
// or wrong-type artifact (e.g. a classify.json handed to --verify) is a hard
// error the operator must fix, never a struct that silently zero-fills its
// counts to a bogus PASS.
func readArtifact(path string, v any) error {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied artifact path; offline CLI input.
	if err != nil {
		return fmt.Errorf("read artifact %q: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parse artifact %q: %w", path, err)
	}
	return nil
}

// requireSchemaVersion rejects an artifact whose stamped schema_version is not the
// one this gate build understands. A missing version zero-fills to 0 and a drifted
// producer stamps a different number; either way the gate cannot trust the file's
// shape, so it is a hard error (a block, never a silent PASS on a misread
// artifact), consistent with the unreadable/unparseable contract.
func requireSchemaVersion(path string, got, want int) error {
	if got != want {
		return fmt.Errorf(
			"artifact %q: unsupported schema_version %d (this gate understands %d); regenerate it with a matching `migrate` build",
			path, got, want,
		)
	}
	return nil
}
