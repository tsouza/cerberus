package migrategate_test

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/migrategate"
	"github.com/tsouza/cerberus/internal/migrateinventory"
	"github.com/tsouza/cerberus/internal/migrateverify"
)

// writeArtifact renders one block's JSON (via its real WriteJSON writer, so the
// gate reads exactly what the CLI emits) into a temp file and returns its path.
func writeArtifact(t *testing.T, name string, wj func(io.Writer) error) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err := wj(f); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	return p
}

// cleanVerify is a parity report with no divergence and no error.
func cleanVerify(t *testing.T) string {
	rep := migrateverify.Report{Summary: migrateverify.Summary{Total: 3, Match: 3}}
	return writeArtifact(t, "verify.json", rep.WriteJSON)
}

// cleanClassify is a classification with everything supported and nothing risky.
func cleanClassify(t *testing.T) string {
	cl := migrate.Classification{Counts: migrate.BucketCounts{Total: 2, Supported: 2}}
	return writeArtifact(t, "classify.json", cl.WriteJSON)
}

// cleanInventory is an inventory whose top cardinalities are below the WARN
// thresholds and whose optional enrichments were all obtained (non-negative
// totals, no notes) — a genuinely clean bill of health.
func cleanInventory(t *testing.T) string {
	inv := migrateinventory.Inventory{
		Source: "http://src", Top: 5, MetricNameTotal: 42, MetadataMetricTotal: 40,
		TopMetricsBySeries: []migrateinventory.NameValue{{Name: "up", Value: 12}},
		TopLabelsByValues:  []migrateinventory.NameValue{{Name: "instance", Value: 30}},
	}
	return writeArtifact(t, "inventory.json", inv.WriteJSON)
}

// cleanRuleGraph is a rulegraph whose recorded series are all orphan (safe to
// drop; nobody consumes them).
func cleanRuleGraph(t *testing.T) string {
	g := migrate.RuleGraph{
		Counts:   migrate.RuleGraphCounts{Recorded: 1, Orphan: 1},
		Recorded: []migrate.RecordedNode{{Name: "job:foo:rate", Source: "rules.yml", Status: migrate.StatusOrphan}},
	}
	return writeArtifact(t, "rulegraph.json", g.WriteJSON)
}

// stageByName finds a stage result in a decision.
func stageByName(t *testing.T, d migrategate.Decision, name string) migrategate.StageResult {
	t.Helper()
	for _, s := range d.Stages {
		if s.Stage == name {
			return s
		}
	}
	t.Fatalf("stage %q not found in decision", name)
	return migrategate.StageResult{}
}

func TestEvaluatePassCleanArtifacts(t *testing.T) {
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  cleanClassify(t),
		Inventory: cleanInventory(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !dec.Pass || dec.Overall != migrategate.OverallPass {
		t.Fatalf("want PASS, got pass=%v overall=%q stages=%+v", dec.Pass, dec.Overall, dec.Stages)
	}
	for _, s := range dec.Stages {
		if s.Verdict != migrategate.VerdictPass {
			t.Errorf("stage %q: want PASS, got %q (%v)", s.Stage, s.Verdict, s.Reasons)
		}
		if s.Blocking {
			t.Errorf("stage %q blocked on clean input", s.Stage)
		}
	}
	if len(dec.Missing) != 0 {
		t.Errorf("clean run should report no missing artifacts, got %v", dec.Missing)
	}
}

func TestEvaluateBlocksOnVerifyDiverge(t *testing.T) {
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 3, Match: 2, Diverge: 1},
		Results: []migrateverify.QueryResult{{Source: "r", Expr: "up", Verdict: migrateverify.VerdictDiverge}},
	}
	in := migrategate.Inputs{
		Verify:    writeArtifact(t, "verify.json", rep.WriteJSON),
		Classify:  cleanClassify(t),
		Inventory: cleanInventory(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec.Pass || dec.Overall != migrategate.OverallFail {
		t.Fatalf("want FAIL, got pass=%v overall=%q", dec.Pass, dec.Overall)
	}
	v := stageByName(t, dec, migrategate.StageVerify)
	if v.Verdict != migrategate.VerdictFail || !v.Blocking {
		t.Fatalf("verify stage: want FAIL+blocking, got %q blocking=%v", v.Verdict, v.Blocking)
	}
	// Only verify should block; the other stages stay clean.
	if s := stageByName(t, dec, migrategate.StageClassify); s.Blocking {
		t.Errorf("classify blocked on a verify-only failure")
	}
}

func TestEvaluateBlocksOnVerifyError(t *testing.T) {
	rep := migrateverify.Report{Summary: migrateverify.Summary{Total: 2, Match: 1, Error: 1}}
	in := migrategate.Inputs{
		Verify:    writeArtifact(t, "verify.json", rep.WriteJSON),
		Classify:  cleanClassify(t),
		Inventory: cleanInventory(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	v := stageByName(t, dec, migrategate.StageVerify)
	if dec.Pass || v.Verdict != migrategate.VerdictFail || !v.Blocking {
		t.Fatalf("want verify FAIL+blocking, got pass=%v verdict=%q", dec.Pass, v.Verdict)
	}
}

func TestEvaluateBlocksOnClassifyUnsupported(t *testing.T) {
	cl := migrate.Classification{
		Counts: migrate.BucketCounts{Total: 2, Supported: 1, Unsupported: 1},
		Queries: []migrate.ClassifiedQuery{
			{Expr: "weird()", Source: "s", Kind: "rule", Bucket: migrate.BucketUnsupported, Construct: `unsupported function "weird"`},
		},
	}
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  writeArtifact(t, "classify.json", cl.WriteJSON),
		Inventory: cleanInventory(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	c := stageByName(t, dec, migrategate.StageClassify)
	if dec.Pass || c.Verdict != migrategate.VerdictFail || !c.Blocking {
		t.Fatalf("want classify FAIL+blocking, got pass=%v verdict=%q", dec.Pass, c.Verdict)
	}
	if !containsSubstr(c.Reasons, "weird") {
		t.Errorf("classify reasons should name the unsupported construct, got %v", c.Reasons)
	}
}

func TestEvaluateBlocksOnConsumedRecordedSeries(t *testing.T) {
	g := migrate.RuleGraph{
		Counts: migrate.RuleGraphCounts{Recorded: 2, Consumed: 1, Orphan: 1, Consumers: 1},
		Recorded: []migrate.RecordedNode{
			{Name: "job:http:rate", Source: "rules.yml", Status: migrate.StatusConsumed, Consumers: []string{"dash.json"}},
			{Name: "job:idle:rate", Source: "rules.yml", Status: migrate.StatusOrphan},
		},
		Consumers: []migrate.ConsumerNode{
			{Expr: "job:http:rate", Source: "dash.json", Kind: "dashboard", References: []string{"job:http:rate"}},
		},
	}
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  cleanClassify(t),
		Inventory: cleanInventory(t),
		RuleGraph: writeArtifact(t, "rulegraph.json", g.WriteJSON),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	r := stageByName(t, dec, migrategate.StageRuleGraph)
	if dec.Pass || r.Verdict != migrategate.VerdictFail || !r.Blocking {
		t.Fatalf("want rulegraph FAIL+blocking, got pass=%v verdict=%q", dec.Pass, r.Verdict)
	}
	if !containsSubstr(r.Reasons, "job:http:rate") {
		t.Errorf("rulegraph reasons should name the consumed series, got %v", r.Reasons)
	}
}

func TestEvaluateBlocksOnMissingRequiredArtifact(t *testing.T) {
	// Omit verify (a required artifact). The gate cannot prove parity safety, so
	// it blocks on the missing input rather than passing by omission.
	in := migrategate.Inputs{
		Classify:  cleanClassify(t),
		Inventory: cleanInventory(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	v := stageByName(t, dec, migrategate.StageVerify)
	if dec.Pass || v.Verdict != migrategate.VerdictMissing || !v.Blocking {
		t.Fatalf("want verify MISSING+blocking, got pass=%v verdict=%q blocking=%v", dec.Pass, v.Verdict, v.Blocking)
	}
	if !containsExact(dec.Missing, migrategate.StageVerify) {
		t.Errorf("verify should be reported as missing, got %v", dec.Missing)
	}
}

func TestEvaluateWarnsButPassesOnHighCardinality(t *testing.T) {
	inv := migrateinventory.Inventory{
		Source: "http://src", Top: 5, MetricNameTotal: 100, MetadataMetricTotal: 90,
		TopMetricsBySeries: []migrateinventory.NameValue{{Name: "http_requests_total", Value: 250_000}},
		TopLabelsByValues:  []migrateinventory.NameValue{{Name: "user_id", Value: 80_000}},
	}
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  cleanClassify(t),
		Inventory: writeArtifact(t, "inventory.json", inv.WriteJSON),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	iv := stageByName(t, dec, migrategate.StageInventory)
	if iv.Verdict != migrategate.VerdictWarn {
		t.Fatalf("want inventory WARN, got %q", iv.Verdict)
	}
	if iv.Blocking {
		t.Errorf("high cardinality must WARN, never block")
	}
	if !dec.Pass || dec.Overall != migrategate.OverallPass {
		t.Fatalf("a WARN-only run should PASS overall, got pass=%v overall=%q", dec.Pass, dec.Overall)
	}
	if !containsSubstr(iv.Reasons, "http_requests_total") || !containsSubstr(iv.Reasons, "user_id") {
		t.Errorf("inventory reasons should name both offenders, got %v", iv.Reasons)
	}
}

func TestEvaluateMissingInventoryIsAdvisoryNotBlocking(t *testing.T) {
	// Inventory is advisory: its worst outcome is a WARN, so a missing inventory
	// artifact is reported but does not block a cutover.
	in := migrategate.Inputs{
		Verify:   cleanVerify(t),
		Classify: cleanClassify(t),
		// Inventory omitted.
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	iv := stageByName(t, dec, migrategate.StageInventory)
	if iv.Verdict != migrategate.VerdictMissing || iv.Blocking {
		t.Fatalf("want inventory MISSING+non-blocking, got verdict=%q blocking=%v", iv.Verdict, iv.Blocking)
	}
	if !dec.Pass || dec.Overall != migrategate.OverallPass {
		t.Fatalf("a missing-inventory-only run should still PASS, got pass=%v overall=%q", dec.Pass, dec.Overall)
	}
	if !containsExact(dec.Missing, migrategate.StageInventory) {
		t.Errorf("inventory should be reported as missing, got %v", dec.Missing)
	}
}

func TestEvaluateClassifyRiskyWarnsNotBlocks(t *testing.T) {
	cl := migrate.Classification{
		Counts: migrate.BucketCounts{Total: 1, Supported: 1, Risky: 1},
		Queries: []migrate.ClassifiedQuery{
			{Expr: "a * b", Source: "s", Kind: "rule", Bucket: migrate.BucketSupported, Risky: true, Risks: []string{"vector-join fan-out"}},
		},
	}
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  writeArtifact(t, "classify.json", cl.WriteJSON),
		Inventory: cleanInventory(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	c := stageByName(t, dec, migrategate.StageClassify)
	if c.Verdict != migrategate.VerdictWarn || c.Blocking {
		t.Fatalf("risky-but-supported must WARN not block, got verdict=%q blocking=%v", c.Verdict, c.Blocking)
	}
	if !dec.Pass {
		t.Errorf("a risky-only classify should still PASS overall")
	}
}

func TestEvaluateMultipleBlockersAllReported(t *testing.T) {
	rep := migrateverify.Report{Summary: migrateverify.Summary{Total: 1, Diverge: 1}}
	cl := migrate.Classification{
		Counts:  migrate.BucketCounts{Total: 1, Unsupported: 1},
		Queries: []migrate.ClassifiedQuery{{Bucket: migrate.BucketUnsupported, Construct: "x"}},
	}
	in := migrategate.Inputs{
		Verify:   writeArtifact(t, "verify.json", rep.WriteJSON),
		Classify: writeArtifact(t, "classify.json", cl.WriteJSON),
		// inventory + rulegraph omitted: rulegraph is required (blocks), inventory advisory.
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec.Pass {
		t.Fatalf("want FAIL with multiple blockers")
	}
	blocked := map[string]bool{}
	for _, s := range dec.Stages {
		if s.Blocking {
			blocked[s.Stage] = true
		}
	}
	for _, want := range []string{migrategate.StageVerify, migrategate.StageClassify, migrategate.StageRuleGraph} {
		if !blocked[want] {
			t.Errorf("stage %q should block, blocked=%v", want, blocked)
		}
	}
	if blocked[migrategate.StageInventory] {
		t.Errorf("missing inventory (advisory) must not block")
	}
}

func TestEvaluateSuppliedButUnparseableArtifactIsHardError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "verify.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := migrategate.Inputs{Verify: bad, Classify: cleanClassify(t), RuleGraph: cleanRuleGraph(t)}
	if _, err := migrategate.Evaluate(in, migrategate.Options{}); err == nil {
		t.Fatal("a supplied-but-unparseable artifact must be a hard error, not a silent skip")
	}
}

func TestEvaluateMissingSuppliedFileIsHardError(t *testing.T) {
	in := migrategate.Inputs{Verify: filepath.Join(t.TempDir(), "does-not-exist.json"), Classify: cleanClassify(t)}
	if _, err := migrategate.Evaluate(in, migrategate.Options{}); err == nil {
		t.Fatal("a supplied path that does not exist must be a hard error")
	}
}

func TestDecisionWriteJSONRoundTrips(t *testing.T) {
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  cleanClassify(t),
		Inventory: cleanInventory(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	p := writeArtifact(t, "decision.json", dec.WriteJSON)
	data, err := os.ReadFile(p) //nolint:gosec // test-controlled path.
	if err != nil {
		t.Fatal(err)
	}
	var got migrategate.Decision
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decision JSON does not round-trip: %v", err)
	}
	if got.Overall != migrategate.OverallPass || !got.Pass {
		t.Errorf("round-tripped decision mismatch: %+v", got)
	}
	if len(got.Stages) != 4 {
		t.Errorf("want 4 stages in JSON, got %d", len(got.Stages))
	}
}

func TestEvaluateWarnsOnVerifyUnsupportedButPasses(t *testing.T) {
	// A query that emitted SQL offline but returned no comparable matrix live is
	// counted only in Summary.Unsupported. Report.Failed() does not block on it,
	// but the gate must surface it: it is a query UNVERIFIED against the backend,
	// not a clean pass.
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 3, Match: 2, Unsupported: 1, HarvestSkipped: 2, OutOfScope: 1},
	}
	in := migrategate.Inputs{
		Verify:    writeArtifact(t, "verify.json", rep.WriteJSON),
		Classify:  cleanClassify(t),
		Inventory: cleanInventory(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	v := stageByName(t, dec, migrategate.StageVerify)
	if v.Verdict != migrategate.VerdictWarn || v.Blocking {
		t.Fatalf("verify with unchecked entries must WARN not block, got verdict=%q blocking=%v", v.Verdict, v.Blocking)
	}
	if !containsSubstr(v.Reasons, "unverified") {
		t.Errorf("verify reasons should surface the unverified query, got %v", v.Reasons)
	}
	if !containsSubstr(v.Reasons, "harvest-skipped") || !containsSubstr(v.Reasons, "out-of-scope") {
		t.Errorf("verify reasons should surface harvest-skipped and out-of-scope entries, got %v", v.Reasons)
	}
	if !dec.Pass || dec.Overall != migrategate.OverallPass {
		t.Fatalf("a WARN-only verify should PASS overall, got pass=%v overall=%q", dec.Pass, dec.Overall)
	}
}

func TestEvaluateWarnsOnClassifySkippedButPasses(t *testing.T) {
	// A corpus entry the harvester could not turn into a query is carried in
	// Skipped but never classified. It must WARN — a green classify that hides an
	// unexamined 40%-of-corpus is the exact "hollow green" the gate exists to
	// prevent — yet it does not block (it is unexamined, not proven-unsupported).
	cl := migrate.Classification{
		Counts:  migrate.BucketCounts{Total: 2, Supported: 2},
		Queries: []migrate.ClassifiedQuery{{Bucket: migrate.BucketSupported}, {Bucket: migrate.BucketSupported}},
		Skipped: []migrate.SkippedEntry{
			{Source: "rules-a.yml", Reason: "parse: unexpected token"},
			{Source: "rules-b.yml", Reason: "read: permission denied"},
		},
	}
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  writeArtifact(t, "classify.json", cl.WriteJSON),
		Inventory: cleanInventory(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	c := stageByName(t, dec, migrategate.StageClassify)
	if c.Verdict != migrategate.VerdictWarn || c.Blocking {
		t.Fatalf("classify with skips must WARN not block, got verdict=%q blocking=%v", c.Verdict, c.Blocking)
	}
	if !containsSubstr(c.Reasons, "2 harvest-skipped") {
		t.Errorf("classify reasons should surface the skip count, got %v", c.Reasons)
	}
	if !dec.Pass || dec.Overall != migrategate.OverallPass {
		t.Fatalf("a skip-only classify should PASS overall, got pass=%v overall=%q", dec.Pass, dec.Overall)
	}
}

func TestEvaluateBlocksOnRuleGraphSkipped(t *testing.T) {
	// Every recorded series is orphan (nobody consumes it) so the naive verdict
	// is "safe to drop" — but the builder SKIPPED a consumer expr it could not
	// parse. That skipped consumer might have referenced the orphan, which would
	// then be needed after cutover. "orphan ⇒ safe to drop" is unsound with any
	// skip, so the gate BLOCKS rather than green-light a possibly-blank panel.
	g := migrate.RuleGraph{
		Counts:   migrate.RuleGraphCounts{Recorded: 1, Orphan: 1, Skipped: 1},
		Recorded: []migrate.RecordedNode{{Name: "job:foo:rate", Source: "rules.yml", Status: migrate.StatusOrphan}},
		Skipped:  []migrate.SkippedEntry{{Source: "dash.json", Reason: "parse: unexpected token in consumer expr"}},
	}
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  cleanClassify(t),
		Inventory: cleanInventory(t),
		RuleGraph: writeArtifact(t, "rulegraph.json", g.WriteJSON),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	r := stageByName(t, dec, migrategate.StageRuleGraph)
	if dec.Pass || r.Verdict != migrategate.VerdictFail || !r.Blocking {
		t.Fatalf("rulegraph with a skip must FAIL+block, got pass=%v verdict=%q blocking=%v", dec.Pass, r.Verdict, r.Blocking)
	}
	if !containsSubstr(r.Reasons, "unsound") || !containsSubstr(r.Reasons, "dash.json") {
		t.Errorf("rulegraph reasons should surface the skip and its source, got %v", r.Reasons)
	}
}

func TestEvaluateWarnsOnInventoryNotesButPasses(t *testing.T) {
	// An enrichment the probe could not fetch lands in Notes. The gate must
	// surface it: a probe that silently failed an enrichment is an unchecked
	// corner of the source, not a clean inventory — but it stays advisory (WARN).
	inv := migrateinventory.Inventory{
		Source: "http://src", Top: 5, MetricNameTotal: -1, MetadataMetricTotal: 40,
		TopMetricsBySeries: []migrateinventory.NameValue{{Name: "up", Value: 12}},
		TopLabelsByValues:  []migrateinventory.NameValue{{Name: "instance", Value: 30}},
		Notes:              []string{"metric-name total unavailable (/api/v1/label/__name__/values): 503"},
	}
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  cleanClassify(t),
		Inventory: writeArtifact(t, "inventory.json", inv.WriteJSON),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	iv := stageByName(t, dec, migrategate.StageInventory)
	if iv.Verdict != migrategate.VerdictWarn || iv.Blocking {
		t.Fatalf("inventory with a note must WARN not block, got verdict=%q blocking=%v", iv.Verdict, iv.Blocking)
	}
	if !containsSubstr(iv.Reasons, "metric-name total unavailable") {
		t.Errorf("inventory reasons should surface the enrichment note, got %v", iv.Reasons)
	}
	if !dec.Pass || dec.Overall != migrategate.OverallPass {
		t.Fatalf("a note-only inventory should PASS overall, got pass=%v overall=%q", dec.Pass, dec.Overall)
	}
}

func TestEvaluateWarnsOnInventoryEnrichmentUnavailableWithoutNotes(t *testing.T) {
	// A -1 total with no explaining note (an enrichment silently not obtained)
	// must still surface a caveat, so the note-less path cannot read as clean.
	inv := migrateinventory.Inventory{
		Source: "http://src", Top: 5, MetricNameTotal: -1, MetadataMetricTotal: -1,
		TopMetricsBySeries: []migrateinventory.NameValue{{Name: "up", Value: 12}},
		TopLabelsByValues:  []migrateinventory.NameValue{{Name: "instance", Value: 30}},
	}
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  cleanClassify(t),
		Inventory: writeArtifact(t, "inventory.json", inv.WriteJSON),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	iv := stageByName(t, dec, migrategate.StageInventory)
	if iv.Verdict != migrategate.VerdictWarn || iv.Blocking {
		t.Fatalf("inventory with unobtained enrichment must WARN not block, got verdict=%q blocking=%v", iv.Verdict, iv.Blocking)
	}
	if !containsSubstr(iv.Reasons, "not obtained") {
		t.Errorf("inventory reasons should surface the enrichment-not-obtained caveat, got %v", iv.Reasons)
	}
	if !dec.Pass {
		t.Errorf("an enrichment-caveat-only inventory should still PASS overall")
	}
}

// rawArtifact writes verbatim bytes to a temp file and returns its path, for the
// artifact shapes a producer's WriteJSON would never emit — a version-less,
// version-mismatched, or field-drifted file — so the gate's strict decode +
// schema-version check can be exercised against them.
func rawArtifact(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// TestEvaluateBlocksOnVersionMismatch pins that a verify.json stamped with a
// schema_version this gate build does not understand is a hard error (a block),
// never a zero-filled struct that reads as a silent PASS.
func TestEvaluateBlocksOnVersionMismatch(t *testing.T) {
	verify := rawArtifact(t, "verify.json",
		`{"schema_version":999,"params":{"tolerance":1e-9},"summary":{"total":3,"match":3},"results":[]}`)
	in := migrategate.Inputs{Verify: verify, Classify: cleanClassify(t), RuleGraph: cleanRuleGraph(t)}
	if _, err := migrategate.Evaluate(in, migrategate.Options{}); err == nil {
		t.Fatal("a version-mismatched verify artifact must hard error, not silently PASS")
	}
}

// TestEvaluateBlocksOnMissingVersion pins that a version-less artifact (which
// zero-fills schema_version to 0) is rejected, not trusted.
func TestEvaluateBlocksOnMissingVersion(t *testing.T) {
	verify := rawArtifact(t, "verify.json",
		`{"params":{"tolerance":1e-9},"summary":{"total":3,"match":3},"results":[]}`)
	in := migrategate.Inputs{Verify: verify, Classify: cleanClassify(t), RuleGraph: cleanRuleGraph(t)}
	if _, err := migrategate.Evaluate(in, migrategate.Options{}); err == nil {
		t.Fatal("a version-less verify artifact must hard error")
	}
}

// TestEvaluateBlocksOnDriftedUnknownField pins that the strict decoder rejects an
// unknown field (schema drift) rather than ignoring it — a drifted producer
// cannot slip a zero-filled struct past the gate.
func TestEvaluateBlocksOnDriftedUnknownField(t *testing.T) {
	verify := rawArtifact(t, "verify.json",
		`{"schema_version":1,"summary":{"total":3,"match":3},"bogus_new_field":42}`)
	in := migrategate.Inputs{Verify: verify, Classify: cleanClassify(t), RuleGraph: cleanRuleGraph(t)}
	if _, err := migrategate.Evaluate(in, migrategate.Options{}); err == nil {
		t.Fatal("a drifted verify artifact with an unknown field must hard error")
	}
}

// TestEvaluateBlocksOnWrongTypeArtifact pins that a classify.json handed to
// --verify is rejected: its fields are unknown to the verify Report, so the
// strict decoder blocks rather than zero-filling verify's counts to a bogus PASS.
func TestEvaluateBlocksOnWrongTypeArtifact(t *testing.T) {
	in := migrategate.Inputs{Verify: cleanClassify(t), Classify: cleanClassify(t), RuleGraph: cleanRuleGraph(t)}
	if _, err := migrategate.Evaluate(in, migrategate.Options{}); err == nil {
		t.Fatal("a classify artifact fed to --verify must hard error (wrong type), not PASS")
	}
}

// TestEvaluateBlocksOnZeroQueryVerify pins that a parity run that replayed zero
// queries blocks: an empty harvest proves nothing and must not green-light cutover.
func TestEvaluateBlocksOnZeroQueryVerify(t *testing.T) {
	rep := migrateverify.Report{Summary: migrateverify.Summary{Total: 0}}
	in := migrategate.Inputs{
		Verify:    writeArtifact(t, "verify.json", rep.WriteJSON),
		Classify:  cleanClassify(t),
		Inventory: cleanInventory(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	v := stageByName(t, dec, migrategate.StageVerify)
	if dec.Pass || v.Verdict != migrategate.VerdictFail || !v.Blocking {
		t.Fatalf("zero-query verify must FAIL+block, got pass=%v verdict=%q blocking=%v", dec.Pass, v.Verdict, v.Blocking)
	}
	if !containsSubstr(v.Reasons, "nothing verified") {
		t.Errorf("verify reasons should say nothing verified, got %v", v.Reasons)
	}
}

// TestEvaluateBlocksOnZeroQueryClassify pins the same for classify: bucketing
// zero queries proves no support coverage and must block.
func TestEvaluateBlocksOnZeroQueryClassify(t *testing.T) {
	cl := migrate.Classification{Counts: migrate.BucketCounts{Total: 0}}
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  writeArtifact(t, "classify.json", cl.WriteJSON),
		Inventory: cleanInventory(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	c := stageByName(t, dec, migrategate.StageClassify)
	if dec.Pass || c.Verdict != migrategate.VerdictFail || !c.Blocking {
		t.Fatalf("zero-query classify must FAIL+block, got pass=%v verdict=%q blocking=%v", dec.Pass, c.Verdict, c.Blocking)
	}
	if !containsSubstr(c.Reasons, "nothing classified") {
		t.Errorf("classify reasons should say nothing classified, got %v", c.Reasons)
	}
}

// containsSubstr reports whether any element of ss contains sub.
func containsSubstr(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// containsExact reports whether ss contains want exactly.
func containsExact(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
