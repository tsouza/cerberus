package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
)

// runClassifyCmd harvests a corpus (from --corpus, --rules and/or --dashboards),
// dry-runs every query through the exact offline explain pipeline, and buckets
// each one as supported (parses/lowers/emits clean) or unsupported (rejected,
// with the offending construct named), flagging supported-but-risky queries.
// Fully offline: DryRunSQL never opens a ClickHouse connection. Writes the
// ledger to --out (or stdout), as text or --json.
func runClassifyCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate classify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		rules      stringList
		dashboards string
		corpus     string
		out        string
		asJSON     bool
	)
	fs.Var(&rules, "rules",
		"classify PromQL in Prometheus rule files (repeatable comma-separated paths/globs)")
	fs.StringVar(&dashboards, "dashboards", "",
		"classify PromQL in exported Grafana dashboard JSON under a directory (walked recursively)")
	fs.StringVar(&corpus, "corpus", "",
		"classify a corpus.json previously written by `migrate harvest`")
	fs.StringVar(&out, "out", "", "write the classification here (default: stdout)")
	fs.BoolVar(&asJSON, "json", false, "emit the classification ledger as JSON instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	src, err := harvestSources(corpus, rules, dashboards)
	if err != nil {
		return err
	}
	if out == "" {
		return runClassifyReport(stdout, src, asJSON)
	}
	// Render into a buffer, then commit with a checked os.WriteFile (via
	// writeOut). A streaming os.Create with an unchecked Close would swallow a
	// flush-at-close error and truncate the ledger silently.
	var buf bytes.Buffer
	if err := runClassifyReport(&buf, src, asJSON); err != nil {
		return err
	}
	return writeOut(stdout, out, buf.Bytes())
}

// runClassifyReport dry-runs every query in src through the read-side pipeline,
// buckets the resulting explain report, and writes it to w. It is fully offline:
// the engine has no Client and DryRunSQL never executes.
func runClassifyReport(w io.Writer, src migrate.CorpusSource, asJSON bool) error {
	metrics := schema.DefaultOTelMetricsFromEnv()
	eng := &engine.Engine{Optimizer: optimizer.Default()}
	lang := prom.NewExplainLang(metrics, time.Unix(explainEvalUnix, 0).UTC())

	ex := dryRunExplainer{eng: eng, lang: lang}
	rep, err := migrate.BuildReport(context.Background(), src, ex)
	if err != nil {
		return fmt.Errorf("build classify report: %w", err)
	}
	cl := migrate.Classify(rep)
	if asJSON {
		return cl.WriteJSON(w)
	}
	return cl.Write(w)
}
