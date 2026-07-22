// Command migrate is cerberus's pre-cutover migration preview tool. It runs
// offline — without a ClickHouse connection — so an operator can review exactly
// what cerberus will do before provisioning anything.
//
// Usage:
//
//	migrate --schema                          # print the CREATE statements cerberus expects
//	migrate --rules <path/glob>               # explain the ClickHouse SQL for each PromQL rule
//	migrate harvest --rules <glob> \           # build a machine-readable query corpus from
//	  --dashboards <dir> --out corpus.json     #   rule files + exported Grafana dashboards
//	migrate explain --corpus corpus.json       # explain a previously-harvested corpus
//	migrate classify --corpus corpus.json      # bucket each corpus query by PromQL support
//	migrate rulegraph --rules <glob> \          # recording-rule output -> consumer dependency
//	  --corpus corpus.json                       #   graph (what MUST stay materialized)
//	migrate verify --corpus corpus.json \       # replay the corpus against a reference
//	  --ref <prom-url> --cerberus <url>          #   Prometheus and cerberus and diff (parity gate)
//	migrate inventory --source <prom-url>        # probe the LIVE source Prometheus for the
//	  --top 50                                    #   cardinality that drives OOM risk
//	migrate gate --verify verify.json \          # fold the migration artifacts into one
//	  --classify classify.json ...                 #   cutover go/no-go (exit 0 only on PASS)
//
// --schema reads the SAME CERBERUS_* environment the server uses
// (config.FromEnv), so the previewed schema is byte-identical to what the
// server would apply on startup, and pipes straight into clickhouse-client.
//
// harvest scans Prometheus rule files (--rules) and exported Grafana dashboard
// JSON (--dashboards) and emits a versioned, deterministic corpus.json of the
// operator's real PromQL — every dropped item (unreadable file, non-Prometheus
// panel, empty expr) is counted, never silently discarded.
//
// explain harvests a corpus (from --rules/--dashboards, or a --corpus file
// produced by harvest), dry-runs every query through the exact read-side
// pipeline the server runs (parse → project → optimize → emit, via
// engine.DryRunSQL — the ClickHouse client is never touched), and prints the
// emitted SQL, the physical tables each query scans, and conservative offline
// risk flags, or marks the query UNSUPPORTED. Row cardinality is data-dependent
// and is deliberately NOT estimated offline.
//
// classify is a re-view of that same explain data specialised into a
// classification ledger: it buckets each corpus query as supported (PromQL-pure
// / rewritable — parses, lowers, and emits SQL cleanly) or unsupported
// (no-equivalent — a parse/lower/emit error, with the offending construct
// named), flags supported-but-risky queries, and prints per-bucket counts as
// text or --json. "supported" means the query TRANSLATES, not that cerberus
// returns the same numbers as Prometheus — only verify proves parity.
//
// rulegraph builds the recording-rule-output -> consumer dependency graph.
// Because cerberus has NO ruler, a recording rule's OUTPUT series (its record:
// name) is not produced by cerberus — every dashboard panel or alert that reads
// it must be re-materialized elsewhere post-cutover or the panel goes silently
// blank. rulegraph takes the recorded names from --rules and scans every corpus
// query (--corpus) and alerting-rule expr for references to them (a name-level
// approximation via the PromQL parser), then marks each recorded series consumed
// or orphan and lists the consumers that MUST keep being materialized. Text or
// --json; anything unparseable is a counted skip, never silently dropped.
//
// verify is the online cutover parity gate: it replays each PromQL query in a
// harvested corpus against a reference Prometheus AND cerberus over one
// query_range window and diffs the results series-by-series. It exits non-zero
// if any query diverges or errors — a divergence is never allow-listed.
//
// inventory probes the LIVE source Prometheus for the runtime cardinality facts
// config can't reveal offline: it calls /api/v1/status/tsdb (plus optional
// /api/v1/label/__name__/values and /api/v1/metadata) and ranks the top-N
// metrics by series count and labels by cardinality — the OOM candidates
// cerberus can't see before cutover. It refuses to infer from prometheus.yml
// (scrape config is not realized cardinality). The numbers RANK RISK; they do
// not predict cerberus's exact memory. A source that 404s the status endpoint
// exits non-zero.
//
// gate is the cutover decision aggregator: it reads the JSON artifacts the
// other blocks emit (--verify / --classify / --inventory / --rulegraph, all
// optional) and folds them into one PASS/FAIL go/no-go, reporting a per-stage
// checklist and which artifacts were missing. It is pure and offline — reads
// JSON, runs no query. It REFUSES (non-zero exit), never merely warns, on any
// blocking input: a verify divergence/error, a classify unsupported, a
// consumed recorded series that must stay materialized, or a missing required
// artifact. High source cardinality WARNs but does not block. Exit 0 only on
// overall PASS.
package main

import (
	"bytes"
	"context"
	"errors"
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

// outFileMode is the permission for files written by --out (corpus / explain
// report): owner read-write only, since a corpus can name internal metric paths.
const outFileMode = 0o600

func main() {
	err := run(os.Args[1:], os.Stdout, os.Stderr)
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "migrate:", err)
	// A failed parity gate is a real, expected outcome (cerberus diverged) — it
	// gets its own exit code so callers can tell it apart from a tool error.
	var vgate verifyFailedError
	if errors.As(err, &vgate) {
		os.Exit(verifyExitFail)
	}
	// A failed cutover gate (a blocking stage said no-go) is likewise an expected
	// outcome with its own exit code, distinct from a tool error.
	var cgate gateFailedError
	if errors.As(err, &cgate) {
		os.Exit(gateExitFail)
	}
	os.Exit(1)
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
// -testable without spawning a process. The first arg selects a subcommand
// (harvest / explain); anything else falls back to the legacy top-level
// --schema / --rules flags so existing invocations keep working.
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "harvest":
			return runHarvest(args[1:], stdout, stderr)
		case "explain":
			return runExplainCmd(args[1:], stdout, stderr)
		case "classify":
			return runClassifyCmd(args[1:], stdout, stderr)
		case "rulegraph":
			return runRuleGraphCmd(args[1:], stdout, stderr)
		case "verify":
			return runVerify(args[1:], stdout, stderr)
		case "inventory":
			return runInventory(args[1:], stdout, stderr)
		case "gate":
			return runGate(args[1:], stdout, stderr)
		}
	}

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
		src := migrate.MultiSource{migrate.FileSource{RulePaths: opts.rules}}
		return runExplainReport(stdout, src)
	case opts.schema:
		cfg, err := config.FromEnv()
		if err != nil {
			return fmt.Errorf("load config from environment: %w", err)
		}
		return writeSchema(stdout, cfg)
	default:
		fs.Usage()
		return fmt.Errorf("nothing to do: pass --schema to print the expected schema, " +
			"--rules <path> to explain PromQL rule files, or the harvest/explain subcommand")
	}
}

// runHarvest builds a machine-readable query corpus from rule files and/or
// Grafana dashboards and writes deterministic corpus JSON to --out (or stdout).
func runHarvest(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate harvest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		rules      stringList
		dashboards string
		out        string
	)
	fs.Var(&rules, "rules",
		"harvest PromQL from these Prometheus rule files (repeatable or comma-separated paths/globs)")
	fs.StringVar(&dashboards, "dashboards", "",
		"harvest PromQL from exported Grafana dashboard JSON under this directory (walked recursively)")
	fs.StringVar(&out, "out", "", "write the corpus JSON here (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	src, err := harvestSources("", rules, dashboards)
	if err != nil {
		return err
	}
	queries, skipped, err := src.Harvest(context.Background())
	if err != nil {
		return fmt.Errorf("harvest corpus: %w", err)
	}
	data, err := migrate.BuildCorpus(queries, skipped).Marshal()
	if err != nil {
		return err
	}
	return writeOut(stdout, out, data)
}

// runExplainCmd harvests a corpus (from --corpus, --rules and/or --dashboards),
// dry-runs every query through the read-side pipeline, and writes the explain
// report to --out (or stdout). Fully offline: DryRunSQL never executes.
func runExplainCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate explain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		rules      stringList
		dashboards string
		corpus     string
		out        string
	)
	fs.Var(&rules, "rules",
		"explain PromQL from these Prometheus rule files (repeatable or comma-separated paths/globs)")
	fs.StringVar(&dashboards, "dashboards", "",
		"explain PromQL from exported Grafana dashboard JSON under this directory (walked recursively)")
	fs.StringVar(&corpus, "corpus", "",
		"explain a corpus.json previously written by `migrate harvest`")
	fs.StringVar(&out, "out", "", "write the explain report here (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	src, err := harvestSources(corpus, rules, dashboards)
	if err != nil {
		return err
	}
	if out == "" {
		return runExplainReport(stdout, src)
	}
	// Render into a buffer, then commit with a checked os.WriteFile (via
	// writeOut). A streaming os.Create with an unchecked Close would swallow a
	// flush-at-close failure and report a truncated report as success.
	var buf bytes.Buffer
	if err := runExplainReport(&buf, src); err != nil {
		return err
	}
	return writeOut(stdout, out, buf.Bytes())
}

// harvestSources assembles the corpus sources from the provided inputs. Order is
// stable (corpus, then rules, then dashboards) but the corpus itself is sorted
// deterministically, and the explain report renders in that same stable order.
func harvestSources(corpus string, rules []string, dashboards string) (migrate.CorpusSource, error) {
	var src migrate.MultiSource
	if corpus != "" {
		src = append(src, migrate.CorpusFileSource{Path: corpus})
	}
	if len(rules) > 0 {
		src = append(src, migrate.FileSource{RulePaths: rules})
	}
	if dashboards != "" {
		src = append(src, migrate.DashboardSource{Dir: dashboards})
	}
	if len(src) == 0 {
		return nil, errors.New("nothing to harvest: pass --rules, --dashboards, and/or --corpus")
	}
	return src, nil
}

// runExplainReport dry-runs every query from src through the read-side pipeline
// and writes the explain report to w. It is fully offline: the engine has no
// Client and DryRunSQL never executes.
func runExplainReport(w io.Writer, src migrate.CorpusSource) error {
	metrics := schema.DefaultOTelMetricsFromEnv()
	eng := &engine.Engine{Optimizer: optimizer.Default()}
	lang := prom.NewExplainLang(metrics, time.Unix(explainEvalUnix, 0).UTC())

	ex := dryRunExplainer{eng: eng, lang: lang}
	rep, err := migrate.BuildReport(context.Background(), src, ex)
	if err != nil {
		return fmt.Errorf("build explain report: %w", err)
	}
	return rep.Write(w)
}

// writeOut writes data to the named file, or to stdout when out is empty. The
// file write is checked end to end (os.WriteFile), so a flush failure surfaces
// as an error rather than a silently truncated corpus or report.
func writeOut(stdout io.Writer, out string, data []byte) error {
	if out == "" {
		if _, err := stdout.Write(data); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(out, data, outFileMode); err != nil {
		return fmt.Errorf("write output file %q: %w", out, err)
	}
	return nil
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
