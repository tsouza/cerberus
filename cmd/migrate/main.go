// Command migrate is cerberus's pre-cutover migration preview tool. It runs
// offline — without a ClickHouse connection — so an operator can review exactly
// what cerberus will do before provisioning anything.
//
// Usage:
//
//	migrate --schema             # print the CREATE statements cerberus expects
//	migrate --rules <path/glob>  # explain the ClickHouse SQL for each PromQL
//	                             # query in the given Prometheus rule files
//
// --schema reads the SAME CERBERUS_* environment the server uses
// (config.FromEnv), so the previewed schema is byte-identical to what the
// server would apply on startup, and pipes straight into clickhouse-client.
//
// --rules harvests every recording/alerting rule's PromQL, dry-runs it through
// the exact read-side pipeline the server runs (parse → project → optimize →
// emit, via engine.DryRunSQL — the ClickHouse client is never touched), and
// prints the emitted SQL, the physical tables each query scans, and
// conservative offline risk flags, or marks the query UNSUPPORTED. Row
// cardinality is data-dependent and is deliberately NOT estimated offline.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	"github.com/tsouza/cerberus/internal/schemaboot"
)

// explainEvalUnix pins the offline explain's instant-evaluation anchor to a
// fixed wall-clock (Unix seconds) so the emitted SQL — and any goldens over it
// — are deterministic regardless of when the tool runs. The exact instant is
// arbitrary; only its stability matters.
const explainEvalUnix = 1_700_000_000

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

// stringList is a repeatable + comma-separated flag value. `--rules a,b --rules
// c` accumulates ["a", "b", "c"].
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

// options holds the parsed flags. --schema renders the expected ClickHouse
// schema; --rules switches to the offline PromQL explain over rule files.
type options struct {
	schema bool
	rules  stringList
}

// run is the testable entrypoint: it parses args, then dispatches. Splitting it
// from main() (mirroring cmd/route-rules) keeps flag parsing and dispatch unit
// -testable without spawning a process.
func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts options
	fs.BoolVar(&opts.schema, "schema", false,
		"print the ClickHouse schema (CREATE statements) cerberus expects, then exit")
	fs.Var(&opts.rules, "rules",
		"explain the ClickHouse SQL for each PromQL query in these Prometheus rule "+
			"files (repeatable or comma-separated paths/globs)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch {
	case len(opts.rules) > 0:
		return runExplain(stdout, opts.rules)
	case opts.schema:
		cfg, err := config.FromEnv()
		if err != nil {
			return fmt.Errorf("load config from environment: %w", err)
		}
		return writeSchema(stdout, cfg)
	default:
		fs.Usage()
		return fmt.Errorf("nothing to do: pass --schema to print the expected schema, " +
			"or --rules <path> to explain PromQL rule files")
	}
}

// runExplain harvests PromQL from the given rule files, dry-runs each query
// through the read-side pipeline, and writes the explain report to w. It is
// fully offline: the engine has no Client and DryRunSQL never executes.
func runExplain(w io.Writer, rulePaths []string) error {
	metrics := schema.DefaultOTelMetricsFromEnv()
	eng := &engine.Engine{Optimizer: optimizer.Default()}
	lang := prom.NewExplainLang(metrics, time.Unix(explainEvalUnix, 0).UTC())

	src := migrate.FileSource{RulePaths: rulePaths}
	ex := dryRunExplainer{eng: eng, lang: lang}
	rep, err := migrate.BuildReport(context.Background(), src, ex)
	if err != nil {
		return fmt.Errorf("build explain report: %w", err)
	}
	return rep.Write(w)
}

// dryRunExplainer adapts engine.DryRunSQL to migrate.Explainer. The SQL it
// reports is byte-identical to what the server would send to ClickHouse for the
// same query — DryRunSQL runs the identical parse → project → optimize → emit
// stages, minus Execute.
type dryRunExplainer struct {
	eng  *engine.Engine
	lang engine.Lang
}

func (d dryRunExplainer) Explain(ctx context.Context, query string) migrate.Explanation {
	dr, err := d.eng.DryRunSQL(ctx, d.lang, query)
	return migrate.Explanation{SQL: dr.SQL, Plan: dr.Plan, Err: err}
}

// writeSchema renders the schema cerberus expects for cfg and writes it to w.
// It takes an already-loaded config so it is unit-testable without touching the
// process environment.
func writeSchema(w io.Writer, cfg config.Config) error {
	ddlCfg, err := schemaboot.DDLConfig(cfg)
	if err != nil {
		return fmt.Errorf("build schema config: %w", err)
	}
	stmts, err := ddl.RenderAll(ddlCfg, ddl.All)
	if err != nil {
		return fmt.Errorf("render schema: %w", err)
	}
	if len(stmts) == 0 {
		return fmt.Errorf("no schema to render (no signals configured)")
	}
	// Terminate every statement with ';' so the output pipes straight into
	// clickhouse-client; blank lines between statements keep it readable.
	if _, err := fmt.Fprintln(w, strings.Join(stmts, ";\n\n")+";"); err != nil {
		return fmt.Errorf("write schema: %w", err)
	}
	return nil
}
