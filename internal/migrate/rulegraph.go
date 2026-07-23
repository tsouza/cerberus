package migrate

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promrules"
)

// Status values for a recorded series in the dependency graph. Because cerberus
// has NO ruler, a recording rule's OUTPUT series is not materialized by cerberus
// itself — it must be produced elsewhere (a materialized view, an external
// recording engine, …) or every dashboard panel and alert that reads it goes
// silently blank. The graph classifies each recorded series by whether anything
// in the corpus still reads it:
//
//   - StatusConsumed — at least one corpus query or alerting-rule expr
//     references the recorded series name. It MUST keep being materialized
//     post-cutover or its consumers break.
//   - StatusOrphan   — nothing in the scanned inputs references it. It is a
//     candidate to drop (no consumer would notice), but the operator should
//     confirm the corpus is complete before deleting anything.
const (
	StatusConsumed = "consumed"
	StatusOrphan   = "orphan"
)

// RecordedSeries is one recording rule's OUTPUT: the series name it produces
// (`record:`), tagged with the rule source it came from. Two recording rules may
// declare the same output name; each is kept as its own entry so the operator
// sees every site that must be re-materialized.
type RecordedSeries struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// RecordedNode is one recorded series in the emitted graph: its output name, the
// rule that declares it, its consumed/orphan status, and the sorted, deduplicated
// list of consumer sources that reference it.
type RecordedNode struct {
	Name      string   `json:"name"`
	Source    string   `json:"source"`
	Status    string   `json:"status"`
	Consumers []string `json:"consumers"`
}

// ConsumerNode is one query (a corpus query or an alerting-rule expr) that
// references at least one recorded series. References is the sorted, deduplicated
// list of recorded series names it reads — exactly the set that MUST keep being
// materialized for this consumer to keep working after cutover.
type ConsumerNode struct {
	Expr       string   `json:"expr"`
	Source     string   `json:"source"`
	Kind       string   `json:"kind"`
	References []string `json:"references"`
}

// RuleGraphCounts is the headline tally. Recorded is every recording-rule output;
// Consumed + Orphan == Recorded. Consumers is the number of scanned queries that
// reference at least one recorded series. Skipped is every input the builder
// could not use (an unparseable consumer expr, an unreadable rule file, …) —
// counted, never silently dropped.
type RuleGraphCounts struct {
	Recorded  int `json:"recorded"`
	Consumed  int `json:"consumed"`
	Orphan    int `json:"orphan"`
	Consumers int `json:"consumers"`
	Skipped   int `json:"skipped"`
}

// RuleGraphVersion is the schema version stamped into every emitted RuleGraph.
// WriteJSON stamps it and the cutover gate refuses a graph whose version it does
// not understand, so a schema-drifted or wrong-type artifact blocks rather than
// zero-filling to a silent PASS. Bump it on any breaking change to the JSON shape.
const RuleGraphVersion = 1

// RuleGraph is the full recording-rule-output → consumer dependency graph:
// the schema version, the per-recorded-series consumed/orphan classification with
// edges, the flat list of consumers that reference a recorded series, and
// everything the builder had to skip. It marshals deterministically so the same
// inputs always produce byte-identical output.
//
// HONESTY: this is a NAME-LEVEL dependency approximation. A consumer is linked
// to a recorded series when it references that series' metric NAME (via a vector
// selector); label matchers and value equality are not analysed. Rule-to-rule
// chains ARE analysed: each recording rule's own expr is fed in as a consumer, so
// a recorded series read by another recording rule is linked, not left orphan. A
// name collision (an unrelated metric that happens to share a
// recorded name) would over-link; that is the safe direction (it keeps a series
// marked "must materialize" rather than dropping one that is needed). A consumer
// whose matched name set CANNOT be statically reduced — a regex or negated
// `__name__` matcher, or a selector with no name constraint at all — is NOT
// under-linked to "references nothing" (which would be the UNSAFE direction: it
// could hide a real consumer and leave a needed series wrongly orphan). It is
// counted as a skip instead, which blocks the gate, keeping the over-link (never
// under-link) invariant honest.
type RuleGraph struct {
	SchemaVersion int             `json:"schema_version"`
	Counts        RuleGraphCounts `json:"counts"`
	Recorded      []RecordedNode  `json:"recorded"`
	Consumers     []ConsumerNode  `json:"consumers"`
	Skipped       []SkippedEntry  `json:"skipped"`
}

// MetricNameExtractor pulls the set of metric-name references out of one query
// expression. It returns an error when the expression cannot be parsed at all,
// so the caller can count it as a reported skip rather than silently treating it
// as referencing nothing. Injected into BuildRuleGraph so the pure graph logic
// is testable without a full engine.
type MetricNameExtractor func(expr string) ([]string, error)

// ruleGraphParser is the PromQL parser used for name extraction. It matches the
// options the explain/handler paths use (EnableExperimentalFunctions) so a rule
// or corpus expr that parses for the engine also parses here — the graph never
// skips an expr the rest of the tool accepts.
var ruleGraphParser = promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})

// PromQLMetricNames is the production MetricNameExtractor: it parses the
// expression with the upstream Prometheus PromQL parser and walks every vector
// selector, collecting the metric name each one reads (from an explicit
// `__name__` equality matcher, or the selector's bare name). An expression that
// does not parse is returned as an error so BuildRuleGraph records it as a skip.
//
// A selector whose matched name set cannot be statically reduced to concrete
// names — a regex or negated `__name__` matcher, or a selector with no name
// constraint at all — also returns an error: linking it to "references nothing"
// would UNDER-link (the unsafe direction, potentially leaving a truly-consumed
// recorded series marked orphan), so the whole consumer is recorded as a skip
// instead, which blocks the gate.
func PromQLMetricNames(expr string) ([]string, error) {
	ast, err := ruleGraphParser.ParseExpr(expr)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var unreducible error
	promparser.Inspect(ast, func(node promparser.Node, _ []promparser.Node) error {
		vs, ok := node.(*promparser.VectorSelector)
		if !ok {
			return nil
		}
		name, reducible := metricNameOf(vs)
		if !reducible {
			unreducible = fmt.Errorf(
				"consumer selects __name__ non-statically (regex/negated matcher or no name constraint): %s",
				vs.String(),
			)
			return unreducible // stop the walk; the whole consumer is a skip
		}
		seen[name] = struct{}{}
		return nil
	})
	if unreducible != nil {
		return nil, unreducible
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// metricNameOf returns the single recorded-series name a vector selector reads
// and whether that name is statically reducible. A `__name__` equality matcher
// (or the selector's bare Name) yields a concrete name and reducible=true. A
// regex or negated `__name__` matcher, or a selector with no name constraint at
// all (e.g. `{job="x"}`, which matches every metric), yields reducible=false: its
// matched name set is not a single concrete name, so the caller must count it as a
// skip rather than under-link it to nothing.
func metricNameOf(vs *promparser.VectorSelector) (name string, reducible bool) {
	for _, m := range vs.LabelMatchers {
		if m.Name != model.MetricNameLabel {
			continue
		}
		if m.Type == labels.MatchEqual {
			return m.Value, true
		}
		// A regex/negated __name__ matcher selects a name SET, not one name.
		return "", false
	}
	if vs.Name != "" {
		return vs.Name, true
	}
	// No name constraint: the selector matches every metric name.
	return "", false
}

// BuildRuleGraph is the pure, offline core: given the recording-rule outputs and
// the consumer queries (corpus queries + alerting-rule exprs), it links each
// consumer to the recorded series it references (by extracted metric name) and
// classifies each recorded series as consumed or orphan. A consumer whose expr
// the extractor cannot parse is appended to skipped (never silently dropped).
// The result is fully sorted so it is deterministic regardless of input order.
func BuildRuleGraph(recorded []RecordedSeries, consumers []HarvestedQuery, extract MetricNameExtractor, skipped []SkippedEntry) RuleGraph {
	nodes := make([]*RecordedNode, 0, len(recorded))
	byName := map[string][]*RecordedNode{}
	for _, r := range recorded {
		n := &RecordedNode{Name: r.Name, Source: r.Source, Status: StatusOrphan}
		nodes = append(nodes, n)
		byName[r.Name] = append(byName[r.Name], n)
	}

	sk := make([]SkippedEntry, len(skipped))
	copy(sk, skipped)

	consumerNodes := make([]ConsumerNode, 0, len(consumers))
	seenConsumer := map[string]struct{}{}
	for _, c := range consumers {
		// Collapse exact-duplicate consumer entries (same source+expr+kind):
		// overlapping --rules / --corpus inputs (e.g. a corpus harvested from
		// the same rule files) scan an identical consumer twice. Deduping here
		// keeps counts.Consumers and the Consumers list honest, mirroring the
		// sortedUnique edge-dedup on recorded nodes. Distinct consumers that
		// merely share a source (different expr) are preserved.
		ck := c.Source + "\x00" + c.Expr + "\x00" + c.Kind
		if _, dup := seenConsumer[ck]; dup {
			continue
		}
		seenConsumer[ck] = struct{}{}
		names, err := extract(c.Expr)
		if err != nil {
			sk = append(sk, SkippedEntry{
				Source: c.Source,
				Reason: fmt.Sprintf("unparseable consumer expr: %v", err),
			})
			continue
		}
		refSeen := map[string]struct{}{}
		for _, name := range names {
			targets, ok := byName[name]
			if !ok {
				continue
			}
			refSeen[name] = struct{}{}
			for _, t := range targets {
				t.Consumers = append(t.Consumers, c.Source)
			}
		}
		if len(refSeen) == 0 {
			continue
		}
		refs := make([]string, 0, len(refSeen))
		for name := range refSeen {
			refs = append(refs, name)
		}
		sort.Strings(refs)
		consumerNodes = append(consumerNodes, ConsumerNode{
			Expr:       c.Expr,
			Source:     c.Source,
			Kind:       c.Kind,
			References: refs,
		})
	}

	counts := RuleGraphCounts{Recorded: len(nodes)}
	out := make([]RecordedNode, 0, len(nodes))
	for _, n := range nodes {
		n.Consumers = sortedUnique(n.Consumers)
		if len(n.Consumers) > 0 {
			n.Status = StatusConsumed
			counts.Consumed++
		} else {
			counts.Orphan++
		}
		out = append(out, *n)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Source < out[j].Source
	})

	sort.SliceStable(consumerNodes, func(i, j int) bool {
		if consumerNodes[i].Source != consumerNodes[j].Source {
			return consumerNodes[i].Source < consumerNodes[j].Source
		}
		return consumerNodes[i].Expr < consumerNodes[j].Expr
	})
	counts.Consumers = len(consumerNodes)

	sort.SliceStable(sk, func(i, j int) bool {
		if sk[i].Source != sk[j].Source {
			return sk[i].Source < sk[j].Source
		}
		return sk[i].Reason < sk[j].Reason
	})
	counts.Skipped = len(sk)

	return RuleGraph{Counts: counts, Recorded: out, Consumers: consumerNodes, Skipped: sk}
}

// sortedUnique returns a sorted copy of in with duplicates removed. Used to
// dedupe the consumer-source list on a recorded node (the same corpus query can
// reference a recorded name more than once) so edges are stable and counted once.
func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// HarvestRuleFiles reads the recording/alerting rule files matched by rulePaths
// and splits them into what the graph needs: the recording-rule OUTPUT series
// (RecordedSeries), and every rule expr — both alerting exprs AND recording-rule
// exprs — as consumer queries. Feeding recording-rule exprs as consumers links
// rule-to-rule chains (a recorded series consumed by another recording rule),
// keeping the over-link-never-under-link invariant honest. Any input it cannot
// use — a bad glob, an unreadable file, a YAML parse failure, a rule that is
// neither a record nor an alert, a rule with an empty expr — is returned as a
// counted skip rather than dropped.
func HarvestRuleFiles(rulePaths []string) (recorded []RecordedSeries, consumers []HarvestedQuery, skipped []SkippedEntry) {
	for _, pattern := range rulePaths {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			skipped = append(skipped, SkippedEntry{Source: pattern, Reason: fmt.Sprintf("bad path pattern: %v", err)})
			continue
		}
		if len(matches) == 0 {
			skipped = append(skipped, SkippedEntry{Source: pattern, Reason: "no files matched"})
			continue
		}
		for _, file := range matches {
			data, err := os.ReadFile(file) //nolint:gosec // operator-supplied rule-file path; offline CLI.
			if err != nil {
				skipped = append(skipped, SkippedEntry{Source: file, Reason: fmt.Sprintf("read: %v", err)})
				continue
			}
			rg, err := promrules.Parse(data)
			if err != nil {
				skipped = append(skipped, SkippedEntry{Source: file, Reason: fmt.Sprintf("parse: %v", err)})
				continue
			}
			rec, cons, sk := splitRuleGroups(file, rg)
			recorded = append(recorded, rec...)
			consumers = append(consumers, cons...)
			skipped = append(skipped, sk...)
		}
	}
	return recorded, consumers, skipped
}

// splitRuleGroups walks one parsed rule file: a rule with a `record:` name is a
// recorded output series AND a consumer (its own expr reads whatever series it
// aggregates — a rule-to-rule chain); a rule with an `alert:` name and a
// non-empty expr is a consumer query; a rule that is neither is a counted skip.
// A rule with an empty expr is a skip (there is nothing to scan for references).
//
// Feeding the recording rule's expr into the consumer set is what keeps a
// rule-to-rule chain honest: for `record: top / expr: sum(inter)`, the recorded
// series `inter` is referenced by `top`'s expr, so without this it would be
// misclassified orphan ("safe to drop") while a live chain still depends on it —
// an UNDER-link that violates the over-link-never-under-link invariant.
func splitRuleGroups(file string, rg promrules.RuleGroups) (recorded []RecordedSeries, consumers []HarvestedQuery, skipped []SkippedEntry) {
	for _, g := range rg.Groups {
		for _, r := range g.Rules {
			switch {
			case r.Record != "":
				source := fmt.Sprintf("rule:%s/%s/%s", file, g.Name, r.Record)
				recorded = append(recorded, RecordedSeries{Name: r.Record, Source: source})
				if strings.TrimSpace(r.Expr) == "" {
					skipped = append(skipped, SkippedEntry{Source: source, Reason: "recording rule has empty expr"})
					continue
				}
				consumers = append(consumers, HarvestedQuery{Expr: r.Expr, Source: source, Kind: KindRecord})
			case r.Alert != "":
				source := fmt.Sprintf("rule:%s/%s/%s", file, g.Name, r.Alert)
				if strings.TrimSpace(r.Expr) == "" {
					skipped = append(skipped, SkippedEntry{Source: source, Reason: "alerting rule has empty expr"})
					continue
				}
				consumers = append(consumers, HarvestedQuery{Expr: r.Expr, Source: source, Kind: KindAlert})
			default:
				source := fmt.Sprintf("rule:%s/%s/?", file, g.Name)
				skipped = append(skipped, SkippedEntry{Source: source, Reason: "rule is neither a record nor an alert"})
			}
		}
	}
	return recorded, consumers, skipped
}

// Write renders the graph as scannable, human-readable text. It leads with the
// name-level-approximation honesty note, then the headline counts, then the
// orphan recorded series (the ones nothing reads — drop candidates), then the
// consumed recorded series with their consumer edges (what MUST keep being
// materialized), then the skipped inputs with their reasons.
func (g RuleGraph) Write(w io.Writer) error {
	bw := &errWriter{w: w}
	bw.printf("# cerberus migrate rulegraph\n")
	bw.printf("#\n")
	bw.printf("# cerberus has NO ruler: a recording rule's OUTPUT series is not produced\n")
	bw.printf("# by cerberus. Every recorded series a dashboard panel or alert still reads\n")
	bw.printf("# MUST be materialized elsewhere post-cutover, or the panel goes silently\n")
	bw.printf("# blank. This graph links each recorded series to the queries that consume it.\n")
	bw.printf("#\n")
	bw.printf("# HONESTY: this is a NAME-LEVEL dependency approximation — a consumer links to\n")
	bw.printf("# a recorded series when it references that series' metric NAME. Label matchers\n")
	bw.printf("# and value equality are not analysed; rule-to-rule chains ARE (a recording\n")
	bw.printf("# rule's own expr is scanned as a consumer). Unparseable consumer\n")
	bw.printf("# exprs — and consumers whose __name__ set can't be statically reduced (a regex or\n")
	bw.printf("# negated __name__ matcher, or no name constraint) — are counted as skips below\n")
	bw.printf("# (never under-linked to nothing), so the over-link direction stays honest.\n")
	bw.printf("#\n")
	bw.printf("# %d recorded series: %d consumed, %d orphan; %d consumers; %d skipped\n\n",
		g.Counts.Recorded, g.Counts.Consumed, g.Counts.Orphan, g.Counts.Consumers, g.Counts.Skipped)

	bw.printf("== orphan recorded series (%d) — nothing scanned reads these\n", g.Counts.Orphan)
	for _, n := range g.Recorded {
		if n.Status != StatusOrphan {
			continue
		}
		bw.printf("   %s (%s)\n", n.Name, n.Source)
	}

	bw.printf("\n== consumed recorded series (%d) — MUST keep being materialized\n", g.Counts.Consumed)
	for _, n := range g.Recorded {
		if n.Status != StatusConsumed {
			continue
		}
		bw.printf("   %s (%s)\n", n.Name, n.Source)
		for _, c := range n.Consumers {
			bw.printf("     <- %s\n", c)
		}
	}

	bw.printf("\n== consumers referencing recorded series (%d)\n", g.Counts.Consumers)
	for _, c := range g.Consumers {
		bw.printf("   [%s] %s\n", c.Kind, c.Source)
		bw.printf("     expr: %s\n", c.Expr)
		bw.printf("     refs: %s\n", strings.Join(c.References, ", "))
	}

	if len(g.Skipped) > 0 {
		bw.printf("\n== skipped (%d)\n", len(g.Skipped))
		for _, s := range g.Skipped {
			bw.printf("   %s: %s\n", s.Source, s.Reason)
		}
	}
	return bw.err
}

// WriteJSON renders the graph as deterministic, indented JSON with a trailing
// newline. Nil slices become empty slices so the graph always carries `[]` rather
// than `null`, matching the corpus/classify JSON convention.
func (g RuleGraph) WriteJSON(w io.Writer) error {
	g.SchemaVersion = RuleGraphVersion
	if g.Recorded == nil {
		g.Recorded = []RecordedNode{}
	}
	if g.Consumers == nil {
		g.Consumers = []ConsumerNode{}
	}
	if g.Skipped == nil {
		g.Skipped = []SkippedEntry{}
	}
	for i := range g.Recorded {
		if g.Recorded[i].Consumers == nil {
			g.Recorded[i].Consumers = []string{}
		}
	}
	data, err := json.MarshalIndent(g, "", jsonIndent)
	if err != nil {
		return fmt.Errorf("migrate: marshal rulegraph: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("migrate: write rulegraph: %w", err)
	}
	return nil
}
