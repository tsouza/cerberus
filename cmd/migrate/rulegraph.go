package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/tsouza/cerberus/internal/migrate"
)

// runRuleGraphCmd builds the recording-rule-output → consumer dependency graph.
// It harvests recording/alerting rules from --rules (record: names become the
// recorded OUTPUT series; alerting exprs become consumers) and the harvested
// dashboard/alert queries from --corpus (also consumers), then links each
// consumer to the recorded series it references by metric name. It classifies
// every recorded series as consumed or orphan and lists the consumers that MUST
// keep being materialized post-cutover. Fully offline: the only parsing is the
// PromQL name extraction; no ClickHouse connection is opened. Writes the graph to
// --out (or stdout), text or --json.
func runRuleGraphCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate rulegraph", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		rules  stringList
		corpus string
		out    string
		asJSON bool
	)
	fs.Var(&rules, "rules",
		"recording/alerting rule files: record: names are the recorded series, "+
			"alerting exprs are consumers (repeatable comma-separated paths/globs)")
	fs.StringVar(&corpus, "corpus", "",
		"harvested corpus.json (from `migrate harvest`) whose queries are scanned as consumers")
	fs.StringVar(&out, "out", "", "write graph here (default: stdout)")
	fs.BoolVar(&asJSON, "json", false, "emit the graph as JSON instead of text")
	if handled, err := parseFlags(fs, args, stdout, stderr); err != nil || handled {
		return err
	}
	if len(rules) == 0 && corpus == "" {
		fs.Usage()
		return fmt.Errorf("rulegraph needs --rules (for recorded series) and/or --corpus (for consumers)")
	}

	g, err := buildRuleGraph(rules, corpus)
	if err != nil {
		return err
	}
	if out == "" {
		return writeRuleGraph(stdout, g, asJSON)
	}
	// Render into a buffer, then commit with a checked writeOut. A streaming
	// os.Create with an unchecked Close would swallow a flush-at-close error
	// and truncate the graph silently.
	var buf bytes.Buffer
	if err := writeRuleGraph(&buf, g, asJSON); err != nil {
		return err
	}
	return writeOut(stdout, out, buf.Bytes())
}

// buildRuleGraph assembles the graph inputs: recorded series + alerting
// consumers from the rule files, plus every corpus query as an additional
// consumer. Rule-file and corpus skips are threaded through so the graph's skip
// count accounts for every dropped input. The PromQL name extractor is the only
// thing that parses; a corpus/alert expr that will not parse becomes a skip.
func buildRuleGraph(rules []string, corpus string) (migrate.RuleGraph, error) {
	recorded, consumers, skipped := migrate.HarvestRuleFiles(rules)

	if corpus != "" {
		cq, csk, err := migrate.CorpusFileSource{Path: corpus}.Harvest(context.Background())
		if err != nil {
			return migrate.RuleGraph{}, fmt.Errorf("read corpus: %w", err)
		}
		consumers = append(consumers, cq...)
		skipped = append(skipped, csk...)
	}

	return migrate.BuildRuleGraph(recorded, consumers, migrate.PromQLMetricNames, skipped), nil
}

// writeRuleGraph renders the graph as text or JSON.
func writeRuleGraph(w io.Writer, g migrate.RuleGraph, asJSON bool) error {
	if asJSON {
		return g.WriteJSON(w)
	}
	return g.Write(w)
}
