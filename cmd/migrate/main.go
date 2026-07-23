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

// explainRangeWindow / explainRangeStep pin the representative query_range
// window used to preview PANEL-kind queries. Dashboard panels run as ranges
// (unlike rules, which evaluate at a single instant), so previewing them as
// instant queries would emit SQL the server never runs. The window ends at
// explainEvalUnix and spans explainRangeWindow at explainRangeStep resolution;
// the exact span is arbitrary — only its stability (deterministic SQL) and the
// fact that it is a range (non-zero step) matter.
const (
	explainRangeWindow = time.Hour
	explainRangeStep   = time.Minute
)

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

// subcommand is one dispatchable `migrate` verb: its name and a one-line
// description for the root usage listing.
type subcommand struct {
	name string
	desc string
}

// subcommands is the full, ordered list the root usage prints. schema is invoked
// as the `--schema` root flag rather than a verb, but it is listed here so the
// operator sees every capability in one place.
var subcommands = []subcommand{
	{"schema", "print the ClickHouse schema cerberus expects (invoke as: migrate --schema)"},
	{"harvest", "build a machine-readable PromQL query corpus from rule files + dashboards"},
	{"explain", "dry-run corpus queries through the read pipeline (offline emitted SQL)"},
	{"classify", "bucket each corpus query by how cleanly it maps onto cerberus PromQL support"},
	{"rulegraph", "map recording-rule outputs to the consumers that must stay materialized"},
	{"verify", "replay the corpus against reference Prometheus + cerberus (parity gate)"},
	{"inventory", "probe a live source Prometheus for cardinality / OOM-risk facts"},
	{"gate", "fold the artifacts into one cutover go/no-go decision"},
}

// writeRootUsage prints the root help: what the tool is, how to invoke it, the
// full subcommand listing, and the root flag defaults. It sets the flagset output
// to w so PrintDefaults lands on the same writer as the rest of the usage.
func writeRootUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(w, "migrate — cerberus pre-cutover migration preview tool (offline)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  migrate <subcommand> [flags]")
	fmt.Fprintln(w, "  migrate --schema | --rules <path/glob>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	for _, sc := range subcommands {
		fmt.Fprintf(w, "  %-10s %s\n", sc.name, sc.desc)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Root flags (no subcommand):")
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// parseFlags parses fs, routing a -h/--help request cleanly to stdout (exit 0, no
// error line) and a genuine flag error to stderr (non-zero). It buffers the
// flagset's own output so the destination is chosen AFTER the parse outcome is
// known. handled is true only for a help request, telling the caller to return
// without running the command.
func parseFlags(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) (handled bool, err error) {
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	switch parseErr := fs.Parse(args); {
	case errors.Is(parseErr, flag.ErrHelp):
		_, _ = io.Copy(stdout, &buf)
		return true, nil
	case parseErr != nil:
		_, _ = io.Copy(stderr, &buf)
		return false, parseErr
	default:
		// Restore stderr so any post-parse validation usage (fs.Usage) a
		// subcommand prints lands on stderr, not the now-discarded buffer.
		fs.SetOutput(stderr)
		return false, nil
	}
}

// run is the testable entrypoint: it parses args, then dispatches. Splitting it
// from main() (mirroring cmd/route-rules) keeps flag parsing and dispatch unit
// -testable without spawning a process. A non-flag first arg selects a subcommand
// (an unrecognized one is a clear error, never a silent fall-through); otherwise
// the top-level --schema / --rules flags run.
func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	var opts options
	fs.BoolVar(&opts.schema, "schema", false,
		"print the ClickHouse schema (CREATE statements) cerberus expects, then exit")
	fs.Var(&opts.rules, "rules",
		"explain the ClickHouse SQL for each PromQL query in these Prometheus rule "+
			"files (repeatable or comma-separated paths/globs)")
	fs.Usage = func() { writeRootUsage(fs.Output(), fs) }

	// A first arg that is not a flag is a subcommand selector: dispatch a known
	// one, and fail a mistyped one loudly rather than letting it fall through to
	// the root flags and print a misleading "nothing to do".
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
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
		default:
			writeRootUsage(stderr, fs)
			return fmt.Errorf("unknown subcommand %q", args[0])
		}
	}

	if handled, err := parseFlags(fs, args, stdout, stderr); err != nil || handled {
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
		writeRootUsage(stderr, fs)
		return errors.New("nothing to do: pass a subcommand, --schema to print the expected " +
			"schema, or --rules <path> to explain PromQL rule files")
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
	if handled, err := parseFlags(fs, args, stdout, stderr); err != nil || handled {
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
	if handled, err := parseFlags(fs, args, stdout, stderr); err != nil || handled {
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
// Client and DryRunSQL never executes. It loads config.FromEnv() so the preview
// engine carries the SAME resolved per-query sample budget the server enforces
// (see newExplainEngine) — otherwise a budget-busting subquery would preview as
// clean SQL offline yet 422 in production.
func runExplainReport(w io.Writer, src migrate.CorpusSource) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("load config from environment: %w", err)
	}
	ex := newDryRunExplainer(cfg)
	rep, err := migrate.BuildReport(context.Background(), src, ex)
	if err != nil {
		return fmt.Errorf("build explain report: %w", err)
	}
	return rep.Write(w)
}

// newExplainEngine builds the offline preview engine, applying the SAME resolved
// per-query sample budget the server runs with (config.FromEnv coerces the
// CERBERUS_QUERY_MAX_SAMPLES zero-value back to the 5M default via
// resolveQueryMaxSamples). Without this the engine's MaxQuerySamples would be 0,
// which DISABLES the subquery sample-budget gate — so an anchor-grid-busting
// subquery would preview as clean SQL and classify Supported offline while the
// live server 422s it. The engine has no Client: DryRunSQL never executes.
func newExplainEngine(cfg config.Config) *engine.Engine {
	return &engine.Engine{
		Optimizer:       optimizer.Default(),
		MaxQuerySamples: cfg.ClickHouse.MaxQuerySamples,
	}
}

// newDryRunExplainer wires the offline explainer: one engine (with the prod
// sample budget) plus two langs — an instant lang for rules and a range lang for
// dashboard panels — so each query previews in the evaluation mode the server
// would actually run it in.
func newDryRunExplainer(cfg config.Config) dryRunExplainer {
	metrics := schema.DefaultOTelMetricsFromEnv()
	evalTime := time.Unix(explainEvalUnix, 0).UTC()
	return dryRunExplainer{
		eng:         newExplainEngine(cfg),
		instantLang: prom.NewExplainLang(metrics, evalTime),
		rangeLang:   prom.NewExplainLangRange(metrics, evalTime.Add(-explainRangeWindow), evalTime, explainRangeStep),
	}
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
// stages, minus Execute. It holds two langs and picks the one matching the
// query's evaluation mode: a dashboard panel previews as a range query, a rule
// as an instant query.
type dryRunExplainer struct {
	eng         *engine.Engine
	instantLang engine.Lang
	rangeLang   engine.Lang
}

func (d dryRunExplainer) Explain(ctx context.Context, q migrate.HarvestedQuery) migrate.Explanation {
	lang := d.instantLang
	if q.Kind == migrate.KindPanel {
		lang = d.rangeLang
	}
	dr, err := d.eng.DryRunSQL(ctx, lang, q.Expr)
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
