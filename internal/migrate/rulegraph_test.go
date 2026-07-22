package migrate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildRuleGraphConsumedAndOrphan is the core case: one recording rule whose
// output IS referenced by a dashboard query (consumed, with an edge back to that
// query) and one recording rule whose output NOTHING references (orphan). It runs
// the REAL PromQL name extractor, so the edge is proven by actual selector
// parsing, not a stub.
func TestBuildRuleGraphConsumedAndOrphan(t *testing.T) {
	recorded := []RecordedSeries{
		{Name: "job:http_requests:rate5m", Source: "rule:a.yml/api/job:http_requests:rate5m"},
		{Name: "job:errors:rate5m", Source: "rule:a.yml/api/job:errors:rate5m"},
	}
	consumers := []HarvestedQuery{
		// A dashboard panel that reads the first recorded series (inside a
		// selector with a label matcher — the name still matches).
		{Expr: `sum(job:http_requests:rate5m{env="prod"})`, Source: "dash:overview/panel-1", Kind: KindPanel},
		// A panel that reads only a raw, non-recorded metric — references nothing.
		{Expr: `rate(http_requests_total[5m])`, Source: "dash:overview/panel-2", Kind: KindPanel},
	}

	g := BuildRuleGraph(recorded, consumers, PromQLMetricNames, nil)

	if g.Counts.Recorded != 2 || g.Counts.Consumed != 1 || g.Counts.Orphan != 1 {
		t.Fatalf("counts = %+v, want recorded 2 / consumed 1 / orphan 1", g.Counts)
	}
	if g.Counts.Consumers != 1 {
		t.Fatalf("consumers = %d, want 1", g.Counts.Consumers)
	}

	byName := map[string]RecordedNode{}
	for _, n := range g.Recorded {
		byName[n.Name] = n
	}

	consumed, ok := byName["job:http_requests:rate5m"]
	if !ok {
		t.Fatal("recorded series job:http_requests:rate5m missing from graph")
	}
	if consumed.Status != StatusConsumed {
		t.Errorf("job:http_requests:rate5m status = %q, want %q", consumed.Status, StatusConsumed)
	}
	if len(consumed.Consumers) != 1 || consumed.Consumers[0] != "dash:overview/panel-1" {
		t.Errorf("consumed edges = %v, want [dash:overview/panel-1]", consumed.Consumers)
	}

	orphan, ok := byName["job:errors:rate5m"]
	if !ok {
		t.Fatal("recorded series job:errors:rate5m missing from graph")
	}
	if orphan.Status != StatusOrphan {
		t.Errorf("job:errors:rate5m status = %q, want %q", orphan.Status, StatusOrphan)
	}
	if len(orphan.Consumers) != 0 {
		t.Errorf("orphan should have no consumers, got %v", orphan.Consumers)
	}

	if len(g.Consumers) != 1 {
		t.Fatalf("consumer nodes = %d, want 1", len(g.Consumers))
	}
	cn := g.Consumers[0]
	if cn.Source != "dash:overview/panel-1" || len(cn.References) != 1 ||
		cn.References[0] != "job:http_requests:rate5m" {
		t.Errorf("consumer node = %+v, want panel-1 referencing job:http_requests:rate5m", cn)
	}
}

// TestBuildRuleGraphUnparseableIsSkipped pins that a consumer expr the extractor
// cannot parse is COUNTED as a skip, never silently treated as referencing
// nothing.
func TestBuildRuleGraphUnparseableIsSkipped(t *testing.T) {
	recorded := []RecordedSeries{{Name: "job:up", Source: "rule:a.yml/g/job:up"}}
	consumers := []HarvestedQuery{
		{Expr: "job:up", Source: "dash:d/ok", Kind: KindPanel},
		{Expr: "sum(job:up{", Source: "dash:d/broken", Kind: KindPanel},
	}

	g := BuildRuleGraph(recorded, consumers, PromQLMetricNames, nil)

	if g.Counts.Skipped != 1 || len(g.Skipped) != 1 {
		t.Fatalf("skipped = %d, want 1", g.Counts.Skipped)
	}
	if g.Skipped[0].Source != "dash:d/broken" ||
		!strings.Contains(g.Skipped[0].Reason, "unparseable") {
		t.Errorf("skip entry = %+v, want dash:d/broken / unparseable", g.Skipped[0])
	}
	// The parseable consumer still links the recorded series.
	if g.Counts.Consumed != 1 {
		t.Errorf("consumed = %d, want 1 (the ok consumer)", g.Counts.Consumed)
	}
}

// TestBuildRuleGraphAlertingConsumer proves an alerting-rule expr is scanned as a
// consumer just like a corpus query.
func TestBuildRuleGraphAlertingConsumer(t *testing.T) {
	recorded := []RecordedSeries{{Name: "job:latency:p99", Source: "rule:a.yml/g/job:latency:p99"}}
	consumers := []HarvestedQuery{
		{Expr: "job:latency:p99 > 0.5", Source: "rule:a.yml/g/HighLatency", Kind: KindAlert},
	}

	g := BuildRuleGraph(recorded, consumers, PromQLMetricNames, nil)

	if g.Counts.Consumed != 1 || g.Counts.Consumers != 1 {
		t.Fatalf("counts = %+v, want consumed 1 / consumers 1", g.Counts)
	}
	if g.Recorded[0].Status != StatusConsumed ||
		len(g.Recorded[0].Consumers) != 1 ||
		g.Recorded[0].Consumers[0] != "rule:a.yml/g/HighLatency" {
		t.Errorf("recorded node = %+v, want consumed by the alerting rule", g.Recorded[0])
	}
}

// TestBuildRuleGraphDedupsExactDuplicateConsumers pins that an identical consumer
// entry (same source+expr+kind) scanned twice — as happens when --corpus is
// harvested from the same rule files as --rules, so an alerting expr arrives via
// both HarvestRuleFiles and the corpus — is collapsed to a single consumer node.
// counts.Consumers and the recorded node's edge count stay honest instead of
// double-counting the same consumer.
func TestBuildRuleGraphDedupsExactDuplicateConsumers(t *testing.T) {
	recorded := []RecordedSeries{{Name: "job:latency:p99", Source: "rule:a.yml/g/job:latency:p99"}}
	dup := HarvestedQuery{Expr: "job:latency:p99 > 0.5", Source: "rule:a.yml/g/HighLatency", Kind: KindAlert}
	consumers := []HarvestedQuery{dup, dup} // same consumer arriving via two overlapping inputs

	g := BuildRuleGraph(recorded, consumers, PromQLMetricNames, nil)

	if g.Counts.Consumers != 1 || len(g.Consumers) != 1 {
		t.Fatalf("consumers = %d / nodes %d, want 1 (exact duplicate collapsed)", g.Counts.Consumers, len(g.Consumers))
	}
	if g.Counts.Consumed != 1 {
		t.Fatalf("consumed = %d, want 1", g.Counts.Consumed)
	}
	if len(g.Recorded[0].Consumers) != 1 {
		t.Errorf("recorded edges = %v, want a single de-duplicated edge", g.Recorded[0].Consumers)
	}

	// A genuinely distinct consumer that merely SHARES a source (different expr)
	// is preserved, not folded away.
	consumers2 := []HarvestedQuery{
		{Expr: "job:latency:p99 > 0.5", Source: "rule:a.yml/g/HighLatency", Kind: KindAlert},
		{Expr: "job:latency:p99 > 0.9", Source: "rule:a.yml/g/HighLatency", Kind: KindAlert},
	}
	g2 := BuildRuleGraph(recorded, consumers2, PromQLMetricNames, nil)
	if g2.Counts.Consumers != 2 || len(g2.Consumers) != 2 {
		t.Fatalf("distinct-expr consumers = %d, want 2 (not deduped)", g2.Counts.Consumers)
	}
}

// TestBuildRuleGraphDeterministicJSON pins that the JSON output is stable and
// carries empty slices (never null) for a graph with no consumers or skips.
func TestBuildRuleGraphDeterministicJSON(t *testing.T) {
	recorded := []RecordedSeries{{Name: "job:up", Source: "rule:a.yml/g/job:up"}}
	g := BuildRuleGraph(recorded, nil, PromQLMetricNames, nil)

	var buf bytes.Buffer
	if err := g.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	// Round-trips and the recorded node's consumers is [] not null.
	var back RuleGraph
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if back.Counts.Orphan != 1 {
		t.Errorf("orphan count round-trip = %d, want 1", back.Counts.Orphan)
	}
	if !strings.Contains(buf.String(), `"consumers": []`) {
		t.Errorf("empty consumers should marshal as [], got:\n%s", buf.String())
	}
}

// TestHarvestRuleFilesSplitsRecordAndAlert pins the file harvester: a recording
// rule becomes a recorded series, an alerting rule becomes a consumer, an empty
// alert expr is a counted skip, and a no-name rule is a counted skip.
func TestHarvestRuleFilesSplitsRecordAndAlert(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rules.yml")
	const rules = `
groups:
  - name: g
    rules:
      - record: job:up:rate5m
        expr: rate(up[5m])
      - alert: HighErr
        expr: job:up:rate5m > 1
      - alert: Empty
        expr: ""
      - expr: something
`
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}

	recorded, consumers, skipped := HarvestRuleFiles([]string{file})

	if len(recorded) != 1 || recorded[0].Name != "job:up:rate5m" {
		t.Fatalf("recorded = %+v, want one job:up:rate5m", recorded)
	}
	if len(consumers) != 1 || consumers[0].Kind != KindAlert {
		t.Fatalf("consumers = %+v, want one alert consumer", consumers)
	}
	// One empty-expr alert + one no-name rule = two skips.
	if len(skipped) != 2 {
		t.Fatalf("skipped = %+v, want 2 (empty-expr alert + no-name rule)", skipped)
	}
}
