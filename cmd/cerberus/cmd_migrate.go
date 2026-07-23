package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/migrategate"
	"github.com/tsouza/cerberus/internal/migrateinventory"
	"github.com/tsouza/cerberus/internal/migrateverify"
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

// defaultVerifyTolerance is the epsilon used when neither --tolerance nor
// CERBERUS_VERIFY_TOLERANCE is supplied. It reuses the package default so the
// CLI and the comparator agree on what "the same number" means.
const defaultVerifyTolerance = migrateverify.DefaultTolerance

// verifyExitFail is the process exit code when the parity gate fails (any query
// diverges or errors). It is distinct from the code Go uses for an internal tool
// error so a divergence reads as "the gate did its job", not "the tool broke".
const verifyExitFail = 2

// gateExitFail is the process exit code when the cutover gate returns FAIL (a
// blocking stage said no-go). It is distinct from a tool error and from verify's
// own gate code, so a no-go reads as "the gate did its job", not "the tool broke".
const gateExitFail = 3

// reportFileMode is the permission for the --report JSON diagnostic: operator-
// readable, not world-writable.
const reportFileMode = 0o600

// unknownToolVersion is the tool_version stamped into the diagnostic when the
// binary carries no VCS/module build stamp (e.g. a `go test` binary or a
// -buildvcs=false build).
const unknownToolVersion = "unknown"

// errNoMigrateSubcommand is returned when `cerberus migrate` is invoked bare.
// migrate is a group of verbs — with none selected there is nothing to do, and
// falling through to a silent success would hide operator mistakes.
var errNoMigrateSubcommand = errors.New("nothing to do: pass a migrate subcommand (see `cerberus migrate --help`)")

// newMigrateCmd builds the `cerberus migrate` command group: the offline
// pre-cutover migration preview toolkit. Every subcommand is offline unless it
// explicitly probes a live source (inventory) or replays against live backends
// (verify); the rest run without a ClickHouse connection so an operator can
// review exactly what cerberus will do before provisioning anything.
func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Offline pre-cutover migration preview toolkit",
		Long: "migrate previews a Prometheus→cerberus cutover without provisioning\n" +
			"anything. schema prints the ClickHouse DDL cerberus expects; harvest\n" +
			"builds a query corpus from rule files + dashboards; explain/classify\n" +
			"dry-run that corpus through the read pipeline; rulegraph maps recording\n" +
			"rules to their consumers; verify replays the corpus against a reference\n" +
			"Prometheus and cerberus and diffs (the parity gate); inventory probes a\n" +
			"live source for cardinality/OOM risk; and gate folds the artifacts into\n" +
			"one go/no-go decision.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Usage to stderr (keeps stdout clean); the error drives a non-zero exit.
			printUsageToStderr(cmd)
			return errNoMigrateSubcommand
		},
	}
	cmd.AddCommand(
		newMigrateSchemaCmd(),
		newMigrateHarvestCmd(),
		newMigrateExplainCmd(),
		newMigrateClassifyCmd(),
		newMigrateRuleGraphCmd(),
		newMigrateVerifyCmd(),
		newMigrateInventoryCmd(),
		newMigrateGateCmd(),
	)
	return cmd
}

// execCmd drives a freshly-built cobra command over explicit args + IO writers.
// It normalizes a nil args slice to an empty one: cobra treats a nil SetArgs as
// "fall back to os.Args[1:]", which would leak the parent process's argv into a
// standalone command invocation (tests, wrappers). It is the seam the wrappers
// and unit tests use to exercise a single subcommand without the full root tree.
func execCmd(cmd *cobra.Command, args []string, stdout, stderr io.Writer) error {
	if args == nil {
		args = []string{}
	}
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	return cmd.Execute()
}

// printUsageToStderr writes the command's usage to stderr. cobra's cmd.Usage()
// targets OutOrStderr — which prefers the Out writer — so a usage-on-error hint
// would otherwise land on stdout and pollute the command's data output; the
// migrate verbs keep usage on stderr and stdout for results only.
func printUsageToStderr(cmd *cobra.Command) {
	fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
}

// runMigrate dispatches through the full migrate command group. It is the seam
// unit tests drive (and mirrors the production `cerberus migrate <verb>` path).
func runMigrate(args []string, stdout, stderr io.Writer) error {
	return execCmd(newMigrateCmd(), args, stdout, stderr)
}

// runVerify / runGate / runInventory execute a single migrate subcommand
// standalone. They exist so tests (and the parity/cutover exit-code contract)
// exercise exactly one verb's flag parsing + logic without the parent tree.
func runVerify(args []string, stdout, stderr io.Writer) error {
	return execCmd(newMigrateVerifyCmd(), args, stdout, stderr)
}

func runGate(args []string, stdout, stderr io.Writer) error {
	return execCmd(newMigrateGateCmd(), args, stdout, stderr)
}

func runInventory(args []string, stdout, stderr io.Writer) error {
	return execCmd(newMigrateInventoryCmd(), args, stdout, stderr)
}

// normalizeList reproduces the legacy stringList semantics on top of cobra's
// StringSlice accumulation: each element is trimmed and blanks are dropped, so
// `--rules a,b --rules ' c '` accumulates ["a","b","c"] exactly as the old
// repeatable+comma flag did.
func normalizeList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// newMigrateSchemaCmd prints the ClickHouse schema cerberus expects, read from
// the SAME CERBERUS_* environment the server uses (config.FromEnv), so the
// previewed schema is byte-identical to what the server would apply on startup.
func newMigrateSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the ClickHouse schema (CREATE statements) cerberus expects",
		Long: "Render the ClickHouse schema cerberus expects from the current CERBERUS_*\n" +
			"environment — offline, no database connection — so it can be reviewed\n" +
			"before provisioning. The output is ';'-terminated and pipes straight into\n" +
			"clickhouse-client.",
		Example:       "  cerberus migrate schema | clickhouse-client -h clickhouse --multiquery",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.FromEnv()
			if err != nil {
				return fmt.Errorf("load config from environment: %w", err)
			}
			return writeSchema(cmd.OutOrStdout(), cfg)
		},
	}
}

// newMigrateHarvestCmd builds a machine-readable query corpus from rule files
// and/or Grafana dashboards and writes deterministic corpus JSON to --out (or
// stdout).
func newMigrateHarvestCmd() *cobra.Command {
	var (
		rules      []string
		lokiRules  []string
		dashboards string
		out        string
	)
	cmd := &cobra.Command{
		Use:   "harvest",
		Short: "Build a machine-readable three-headed query corpus from rule files + dashboards",
		Long: "Scan Prometheus rule files (--rules), Loki rule files (--loki-rules), and\n" +
			"exported Grafana dashboard JSON (--dashboards) and emit a versioned,\n" +
			"deterministic corpus.json of the operator's real PromQL, LogQL, and TraceQL —\n" +
			"each query tagged with its language and provenance. Dashboard Prometheus\n" +
			"panels (PromQL), Loki panels (LogQL), and Tempo panels (TraceQL, read from the\n" +
			"panel's `query` field) are all harvested. Every dropped item (unreadable file,\n" +
			"unsupported datasource, empty expr) is counted, never silently discarded.",
		Example:       "  cerberus migrate harvest --rules 'rules/*.yml' --loki-rules 'loki/*.yml' --dashboards dashboards/ --out corpus.json",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runHarvestCommand(cmd, normalizeList(rules), normalizeList(lokiRules), dashboards, out)
		},
	}
	cmd.Flags().StringSliceVar(&rules, "rules", nil,
		"harvest PromQL from these Prometheus rule files (repeatable or comma-separated paths/globs)")
	cmd.Flags().StringSliceVar(&lokiRules, "loki-rules", nil,
		"harvest LogQL from these Loki rule files, which share the Prometheus rule-file YAML shape (repeatable or comma-separated paths/globs)")
	cmd.Flags().StringVar(&dashboards, "dashboards", "",
		"harvest PromQL/LogQL/TraceQL from exported Grafana dashboard JSON under this directory (walked recursively)")
	cmd.Flags().StringVar(&out, "out", "", "write the corpus JSON here (default: stdout)")
	return cmd
}

func runHarvestCommand(cmd *cobra.Command, rules, lokiRules []string, dashboards, out string) error {
	src, err := harvestSources("", rules, lokiRules, dashboards)
	if err != nil {
		if errors.Is(err, errNothingToHarvest) {
			printUsageToStderr(cmd)
		}
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
	return writeOut(cmd.OutOrStdout(), out, data)
}

// newMigrateExplainCmd harvests a corpus (from --corpus, --rules and/or
// --dashboards), dry-runs every query through the read-side pipeline, and writes
// the explain report to --out (or stdout). Fully offline: DryRunSQL never
// executes. This absorbs the legacy `migrate --rules` root flag.
func newMigrateExplainCmd() *cobra.Command {
	var (
		rules      []string
		lokiRules  []string
		dashboards string
		corpus     string
		out        string
	)
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Dry-run corpus queries through the read pipeline (offline emitted SQL)",
		Long: "Harvest a corpus (from --corpus, --rules and/or --dashboards), dry-run\n" +
			"every query through the exact read-side pipeline the server runs\n" +
			"(parse → project → optimize → emit; the ClickHouse client is never\n" +
			"touched), and print the emitted SQL, the physical tables each query\n" +
			"scans, and conservative offline risk flags — or mark the query\n" +
			"UNSUPPORTED. Row cardinality is data-dependent and is deliberately NOT\n" +
			"estimated offline.",
		Example: "  cerberus migrate explain --rules 'rules/*.yml'\n" +
			"  cerberus migrate explain --corpus corpus.json --out explain.txt",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runExplainCommand(cmd, normalizeList(rules), normalizeList(lokiRules), dashboards, corpus, out)
		},
	}
	cmd.Flags().StringSliceVar(&rules, "rules", nil,
		"explain PromQL from these Prometheus rule files (repeatable or comma-separated paths/globs)")
	cmd.Flags().StringSliceVar(&lokiRules, "loki-rules", nil,
		"explain LogQL from these Loki rule files, which share the Prometheus rule-file YAML shape (repeatable or comma-separated paths/globs)")
	cmd.Flags().StringVar(&dashboards, "dashboards", "",
		"explain PromQL/LogQL/TraceQL from exported Grafana dashboard JSON under this directory (walked recursively)")
	cmd.Flags().StringVar(&corpus, "corpus", "",
		"explain a corpus.json previously written by `cerberus migrate harvest`")
	cmd.Flags().StringVar(&out, "out", "", "write the explain report here (default: stdout)")
	return cmd
}

func runExplainCommand(cmd *cobra.Command, rules, lokiRules []string, dashboards, corpus, out string) error {
	src, err := harvestSources(corpus, rules, lokiRules, dashboards)
	if err != nil {
		if errors.Is(err, errNothingToHarvest) {
			printUsageToStderr(cmd)
		}
		return err
	}
	if out == "" {
		return runExplainReport(cmd.OutOrStdout(), src)
	}
	// Render into a buffer, then commit with a checked os.WriteFile (via
	// writeOut). A streaming os.Create with an unchecked Close would swallow a
	// flush-at-close failure and report a truncated report as success.
	var buf bytes.Buffer
	if err := runExplainReport(&buf, src); err != nil {
		return err
	}
	return writeOut(cmd.OutOrStdout(), out, buf.Bytes())
}

// newMigrateClassifyCmd harvests a corpus and buckets each query as supported
// (parses/lowers/emits clean) or unsupported (rejected, with the offending
// construct named), flagging supported-but-risky queries. Fully offline.
func newMigrateClassifyCmd() *cobra.Command {
	var (
		rules      []string
		lokiRules  []string
		dashboards string
		corpus     string
		out        string
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "classify",
		Short: "Bucket each corpus query by how cleanly it maps onto cerberus PromQL support",
		Long: "Dry-run every corpus query through the offline explain pipeline and bucket\n" +
			"each as supported (parses/lowers/emits cleanly) or unsupported (rejected,\n" +
			"with the offending construct named), flagging supported-but-risky queries.\n" +
			"\"Supported\" means the query TRANSLATES, not that cerberus returns the same\n" +
			"numbers as Prometheus — only `verify` proves parity.",
		Example:       "  cerberus migrate classify --corpus corpus.json --json",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClassifyCommand(cmd, normalizeList(rules), normalizeList(lokiRules), dashboards, corpus, out, asJSON)
		},
	}
	cmd.Flags().StringSliceVar(&rules, "rules", nil,
		"classify PromQL in Prometheus rule files (repeatable or comma-separated paths/globs)")
	cmd.Flags().StringSliceVar(&lokiRules, "loki-rules", nil,
		"classify LogQL in Loki rule files, which share the Prometheus rule-file YAML shape (repeatable or comma-separated paths/globs)")
	cmd.Flags().StringVar(&dashboards, "dashboards", "",
		"classify PromQL/LogQL/TraceQL in exported Grafana dashboard JSON under a directory (walked recursively)")
	cmd.Flags().StringVar(&corpus, "corpus", "",
		"classify a corpus.json previously written by `cerberus migrate harvest`")
	cmd.Flags().StringVar(&out, "out", "", "write the classification here (default: stdout)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the classification ledger as JSON instead of text")
	return cmd
}

func runClassifyCommand(cmd *cobra.Command, rules, lokiRules []string, dashboards, corpus, out string, asJSON bool) error {
	src, err := harvestSources(corpus, rules, lokiRules, dashboards)
	if err != nil {
		if errors.Is(err, errNothingToHarvest) {
			printUsageToStderr(cmd)
		}
		return err
	}
	if out == "" {
		return runClassifyReport(cmd.OutOrStdout(), src, asJSON)
	}
	var buf bytes.Buffer
	if err := runClassifyReport(&buf, src, asJSON); err != nil {
		return err
	}
	return writeOut(cmd.OutOrStdout(), out, buf.Bytes())
}

// newMigrateRuleGraphCmd builds the recording-rule-output → consumer dependency
// graph and lists the consumers that MUST keep being materialized post-cutover.
// Needs --rules and/or --corpus.
func newMigrateRuleGraphCmd() *cobra.Command {
	var (
		rules  []string
		corpus string
		out    string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "rulegraph",
		Short: "Map recording-rule outputs to the consumers that must stay materialized",
		Long: "Link each recording rule's recorded OUTPUT series (its record: name, from\n" +
			"--rules) to the corpus queries and alerting exprs that reference it\n" +
			"(--rules alerts + --corpus), classify every recorded series as consumed or\n" +
			"orphan, and list the consumers that MUST keep being materialized after\n" +
			"cutover (cerberus has no ruler). Fully offline; anything unparseable is a\n" +
			"counted skip, never silently dropped.",
		Example:       "  cerberus migrate rulegraph --rules 'rules/*.yml' --corpus corpus.json",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRuleGraphCommand(cmd, normalizeList(rules), corpus, out, asJSON)
		},
	}
	cmd.Flags().StringSliceVar(&rules, "rules", nil,
		"recording/alerting rule files: record: names are the recorded series, alerting exprs are consumers "+
			"(repeatable or comma-separated paths/globs)")
	cmd.Flags().StringVar(&corpus, "corpus", "",
		"harvested corpus.json (from `cerberus migrate harvest`) whose queries are scanned as consumers")
	cmd.Flags().StringVar(&out, "out", "", "write the graph here (default: stdout)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the graph as JSON instead of text")
	return cmd
}

func runRuleGraphCommand(cmd *cobra.Command, rules []string, corpus, out string, asJSON bool) error {
	if len(rules) == 0 && corpus == "" {
		_ = cmd.Usage()
		return fmt.Errorf("rulegraph needs --rules (for recorded series) and/or --corpus (for consumers)")
	}
	g, err := buildRuleGraph(rules, corpus)
	if err != nil {
		return err
	}
	if out == "" {
		return writeRuleGraph(cmd.OutOrStdout(), g, asJSON)
	}
	var buf bytes.Buffer
	if err := writeRuleGraph(&buf, g, asJSON); err != nil {
		return err
	}
	return writeOut(cmd.OutOrStdout(), out, buf.Bytes())
}

// verifyInputs carries the resolved verify flags to runVerifyCommand.
type verifyInputs struct {
	corpus, ref, cerberus string
	refToken, cerToken    string
	start, end, step      string
	tolerance             float64
	asJSON                bool
	report, out           string
}

// newMigrateVerifyCmd is the online cutover parity gate: it replays each PromQL
// query against a reference Prometheus AND cerberus over one query_range window
// and diffs the results series-by-series, exiting non-zero on any divergence or
// error. Every flag falls back to CERBERUS_VERIFY_*.
func newMigrateVerifyCmd() *cobra.Command {
	var in verifyInputs
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Replay the corpus against reference Prometheus + cerberus (parity gate)",
		Long: "Replay each PromQL query in a harvested corpus against a reference\n" +
			"Prometheus AND cerberus over one query_range window and diff the results\n" +
			"series-by-series. Exits non-zero if any query diverges or errors — a\n" +
			"divergence is never allow-listed. Bearer tokens keep credentials out of\n" +
			"the URL and every artifact.",
		Example:       "  cerberus migrate verify --corpus corpus.json --ref http://prometheus:9090 --cerberus http://cerberus:8080",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The tolerance env is resolved even when --tolerance is set so a
			// fat-fingered CERBERUS_VERIFY_TOLERANCE surfaces as a hard error rather
			// than silently tightening the gate; an explicit flag still wins.
			envTol, err := envFloat("CERBERUS_VERIFY_TOLERANCE", defaultVerifyTolerance)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("tolerance") {
				in.tolerance = envTol
			}
			return runVerifyCommand(cmd, in)
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.corpus, "corpus", envOr("CERBERUS_VERIFY_CORPUS", ""),
		"corpus.json produced by `cerberus migrate harvest` (env: CERBERUS_VERIFY_CORPUS)")
	f.StringVar(&in.ref, "ref", envOr("CERBERUS_VERIFY_REF", ""),
		"reference Prometheus base URL (env: CERBERUS_VERIFY_REF)")
	f.StringVar(&in.cerberus, "cerberus", envOr("CERBERUS_VERIFY_CERBERUS", ""),
		"cerberus base URL (env: CERBERUS_VERIFY_CERBERUS)")
	f.StringVar(&in.refToken, "ref-token", envOr("CERBERUS_VERIFY_REF_TOKEN", ""),
		"bearer token for the reference, sent as an Authorization header (env: CERBERUS_VERIFY_REF_TOKEN)")
	f.StringVar(&in.cerToken, "cerberus-token", envOr("CERBERUS_VERIFY_CERBERUS_TOKEN", ""),
		"bearer token for cerberus, sent as an Authorization header (env: CERBERUS_VERIFY_CERBERUS_TOKEN)")
	f.StringVar(&in.start, "start", envOr("CERBERUS_VERIFY_START", "-1h"),
		"range start (RFC3339, Unix seconds, or relative like -1h/now) (env: CERBERUS_VERIFY_START)")
	f.StringVar(&in.end, "end", envOr("CERBERUS_VERIFY_END", "now"),
		"range end (RFC3339, Unix seconds, or relative like -1h/now) (env: CERBERUS_VERIFY_END)")
	f.StringVar(&in.step, "step", envOr("CERBERUS_VERIFY_STEP", "60s"),
		"range step, e.g. 60s (env: CERBERUS_VERIFY_STEP)")
	f.Float64Var(&in.tolerance, "tolerance", defaultVerifyTolerance,
		"absolute value tolerance for a match (env: CERBERUS_VERIFY_TOLERANCE)")
	f.BoolVar(&in.asJSON, "json", false, "emit the machine-readable JSON report instead of the text report")
	f.StringVar(&in.report, "report", envOr("CERBERUS_VERIFY_REPORT", ""),
		"write the full JSON diagnostics to this file; additive, the text report still prints (env: CERBERUS_VERIFY_REPORT)")
	f.StringVar(&in.out, "out", "",
		"write the report here (default: stdout); same content (text or --json) as stdout")
	return cmd
}

func runVerifyCommand(cmd *cobra.Command, in verifyInputs) error {
	switch {
	case in.corpus == "":
		return errors.New("missing --corpus (or CERBERUS_VERIFY_CORPUS): the harvested corpus.json to replay")
	case in.ref == "":
		return errors.New("missing --ref (or CERBERUS_VERIFY_REF): the reference Prometheus base URL")
	case in.cerberus == "":
		return errors.New("missing --cerberus (or CERBERUS_VERIFY_CERBERUS): the cerberus base URL")
	}

	c, err := migrateverify.LoadCorpus(in.corpus)
	if err != nil {
		return err
	}
	params, err := migrateverify.BuildParams(in.start, in.end, in.step, in.tolerance, time.Now().UTC())
	if err != nil {
		return err
	}

	refBackend := migrateverify.NewHTTPBackend(in.ref, migrateverify.WithBearerToken(in.refToken))
	cerBackend := migrateverify.NewHTTPBackend(in.cerberus, migrateverify.WithBearerToken(in.cerToken))
	rep := migrateverify.Verify(context.Background(), c, refBackend, cerBackend, params)

	// The resolved run params drive both the JSON diagnostic and the copy-pasteable
	// repro command, so the two always describe the exact same window. The backend
	// URLs are REDACTED here (the live requests above already used the real URLs):
	// any user:pass@ basic-auth credential must never reach the repro line, the
	// report JSON, or the text output — the operator re-supplies auth via
	// --ref-token / --cerberus-token (or their own URL) on replay.
	reportParams := migrateverify.VerifyReportParams{
		RefURL:      migrateverify.RedactURL(in.ref),
		CerberusURL: migrateverify.RedactURL(in.cerberus),
		Start:       params.Start.UTC().Format(time.RFC3339),
		End:         params.End.UTC().Format(time.RFC3339),
		Step:        params.Step.String(),
		Tolerance:   params.Tolerance,
		Corpus:      in.corpus,
	}
	repro := reproCommand(reportParams)

	if in.report != "" {
		diag := migrateverify.NewVerifyReport(rep, reportParams, toolVersion(), time.Now().UTC())
		if err := writeReportFile(in.report, diag); err != nil {
			return err
		}
	}

	// Render into a buffer, then commit via the checked writeOut so a flush error
	// surfaces rather than truncating the report. --out follows the file-output
	// convention (file when set, stdout when empty); the parity-gate exit verdict
	// below fires regardless of where the report landed.
	var buf bytes.Buffer
	if err := writeReport(&buf, rep, in.asJSON, repro); err != nil {
		return err
	}
	if err := writeOut(cmd.OutOrStdout(), in.out, buf.Bytes()); err != nil {
		return err
	}
	if rep.Failed() {
		return verifyFailedError{summary: rep.Summary}
	}
	return nil
}

// newMigrateInventoryCmd probes a LIVE source Prometheus for the runtime
// cardinality facts config can't reveal offline — the head-block series/label
// cardinality that drives OOM risk — ranks the top-N candidates, and writes the
// report. Flags fall back to CERBERUS_INVENTORY_*.
func newMigrateInventoryCmd() *cobra.Command {
	var (
		source string
		top    int
		window string
		asJSON bool
		out    string
	)
	cmd := &cobra.Command{
		Use:   "inventory",
		Short: "Probe a live source Prometheus for cardinality / OOM-risk facts",
		Long: "Probe the LIVE source Prometheus for the runtime cardinality facts config\n" +
			"can't reveal offline: /api/v1/status/tsdb (plus optional label-values and\n" +
			"metadata) ranked as the top-N metrics by series count and labels by\n" +
			"cardinality — the OOM candidates cerberus can't see before cutover. It\n" +
			"refuses to infer from prometheus.yml. The numbers RANK RISK; they do not\n" +
			"predict cerberus's exact memory.",
		Example:       "  cerberus migrate inventory --source http://prometheus:9090 --top 50 --json",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if source == "" {
				return errors.New("missing --source (or CERBERUS_INVENTORY_SOURCE): the source Prometheus base URL to probe")
			}
			opts := migrateinventory.Options{Top: top, Window: window}
			if err := opts.Validate(); err != nil {
				return err
			}
			inv, err := migrateinventory.NewClient(source).Probe(context.Background(), opts)
			if err != nil {
				return err
			}
			return writeInventory(cmd.OutOrStdout(), out, inv, asJSON)
		},
	}
	cmd.Flags().StringVar(&source, "source", envOr("CERBERUS_INVENTORY_SOURCE", ""),
		"source Prometheus base URL to probe for live cardinality (env: CERBERUS_INVENTORY_SOURCE)")
	cmd.Flags().IntVar(&top, "top", migrateinventory.DefaultTop, "rank the top N metrics/labels by cardinality")
	cmd.Flags().StringVar(&window, "window", envOr("CERBERUS_INVENTORY_WINDOW", ""),
		"optional observation window (duration like 1h) recorded as report context (env: CERBERUS_INVENTORY_WINDOW)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the machine-readable JSON report instead of text")
	cmd.Flags().StringVar(&out, "out", "", "write the inventory here (default: stdout)")
	return cmd
}

// newMigrateGateCmd folds the JSON artifacts the other migration blocks emit —
// verify, classify, inventory, rulegraph — into a single cutover go/no-go
// decision. Pure and offline; a MISSING required artifact blocks.
func newMigrateGateCmd() *cobra.Command {
	var (
		verify         string
		classify       string
		inventory      string
		rulegraph      string
		out            string
		asJSON         bool
		highCard       int64
		highCardLabels int64
	)
	cmd := &cobra.Command{
		Use:   "gate",
		Short: "Fold the artifacts into one cutover go/no-go decision",
		Long: "Read the JSON artifacts the other blocks emit (--verify / --classify /\n" +
			"--inventory / --rulegraph, all optional) and fold them into one PASS/FAIL\n" +
			"go/no-go, reporting a per-stage checklist and which artifacts were missing.\n" +
			"Pure and offline. It REFUSES (non-zero exit), never merely warns, on any\n" +
			"blocking input; a MISSING required artifact blocks. Exit 0 only on PASS.",
		Example:       "  cerberus migrate gate --verify verify.json --classify classify.json --rulegraph rulegraph.json",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := migrategate.Inputs{
				Verify:    verify,
				Classify:  classify,
				Inventory: inventory,
				RuleGraph: rulegraph,
			}
			opts := migrategate.Options{
				HighCardSeries:      highCard,
				HighCardLabelValues: highCardLabels,
			}
			dec, err := migrategate.Evaluate(in, opts)
			if err != nil {
				return err
			}
			if err := writeGate(cmd.OutOrStdout(), out, dec, asJSON); err != nil {
				return err
			}
			if !dec.Pass {
				return gateFailedError{overall: dec.Overall}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&verify, "verify", "", "verify.json produced by `cerberus migrate verify --json`")
	cmd.Flags().StringVar(&classify, "classify", "", "classify.json produced by `cerberus migrate classify --json`")
	cmd.Flags().StringVar(&inventory, "inventory", "", "inventory.json produced by `cerberus migrate inventory --json`")
	cmd.Flags().StringVar(&rulegraph, "rulegraph", "", "rulegraph.json produced by `cerberus migrate rulegraph --json`")
	cmd.Flags().StringVar(&out, "out", "", "write the decision here (default: stdout)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the decision as JSON instead of text")
	cmd.Flags().Int64Var(&highCard, "high-card-series", migrategate.DefaultHighCardSeries,
		"WARN when a metric's head series count reaches this threshold")
	cmd.Flags().Int64Var(&highCardLabels, "high-card-label-values", migrategate.DefaultHighCardLabelValues,
		"WARN when a label's distinct-value count reaches this threshold")
	return cmd
}

// harvestSources assembles the corpus sources from the provided inputs. Order is
// stable (corpus, then Prometheus rules, then Loki rules, then dashboards) but the
// corpus itself is sorted deterministically, and the explain report renders in
// that same stable order. Prometheus rules harvest as PromQL; Loki rules share
// the identical YAML shape and harvest as LogQL; dashboards harvest all three
// heads from their panel datasource types.
func harvestSources(corpus string, rules, lokiRules []string, dashboards string) (migrate.CorpusSource, error) {
	var src migrate.MultiSource
	if corpus != "" {
		src = append(src, migrate.CorpusFileSource{Path: corpus})
	}
	if len(rules) > 0 {
		src = append(src, migrate.FileSource{RulePaths: rules, Lang: migrate.LangPromQL})
	}
	if len(lokiRules) > 0 {
		src = append(src, migrate.FileSource{RulePaths: lokiRules, Lang: migrate.LangLogQL})
	}
	if dashboards != "" {
		src = append(src, migrate.DashboardSource{Dir: dashboards})
	}
	if len(src) == 0 {
		return nil, errNothingToHarvest
	}
	return src, nil
}

// errNothingToHarvest is returned by harvestSources when no corpus-input flag was
// supplied. The corpus subcommands treat it as a usage error — print the flag
// help before returning — so every corpus command is consistent.
var errNothingToHarvest = errors.New("nothing to harvest: pass --rules, --loki-rules, --dashboards, and/or --corpus")

// runExplainReport dry-runs every query from src through the read-side pipeline
// and writes the explain report to w. It is fully offline: the engine has no
// Client and DryRunSQL never executes. It loads config.FromEnv() so the preview
// engine carries the SAME resolved per-query sample budget the server enforces.
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

// runClassifyReport dry-runs every query in src through the read-side pipeline,
// buckets the resulting explain report, and writes it to w. Fully offline.
func runClassifyReport(w io.Writer, src migrate.CorpusSource, asJSON bool) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("load config from environment: %w", err)
	}
	ex := newDryRunExplainer(cfg)
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

// buildRuleGraph assembles the graph inputs: recorded series + alerting
// consumers from the rule files, plus every corpus query as an additional
// consumer. Rule-file and corpus skips are threaded through so the graph's skip
// count accounts for every dropped input.
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

// writeInventory renders the inventory (text or JSON) to --out, or stdout when
// out is empty, buffering a file write so a flush-at-close error surfaces rather
// than truncating the report silently.
func writeInventory(stdout io.Writer, out string, inv migrateinventory.Inventory, asJSON bool) error {
	render := func(w io.Writer) error {
		if asJSON {
			return inv.WriteJSON(w)
		}
		return inv.WriteText(w)
	}
	if out == "" {
		return render(stdout)
	}
	var buf bytes.Buffer
	if err := render(&buf); err != nil {
		return err
	}
	return writeOut(stdout, out, buf.Bytes())
}

// writeGate renders the decision (text or JSON) to --out, or stdout when out is
// empty, buffering a file write so a flush-at-close error surfaces.
func writeGate(stdout io.Writer, out string, dec migrategate.Decision, asJSON bool) error {
	render := func(w io.Writer) error {
		if asJSON {
			return dec.WriteJSON(w)
		}
		return dec.Write(w)
	}
	if out == "" {
		return render(stdout)
	}
	var buf bytes.Buffer
	if err := render(&buf); err != nil {
		return err
	}
	return writeOut(stdout, out, buf.Bytes())
}

// newExplainEngine builds the offline preview engine, applying the SAME resolved
// per-query sample budget the server runs with. Without this the engine's
// MaxQuerySamples would be 0, which DISABLES the subquery sample-budget gate.
// The engine has no Client: DryRunSQL never executes.
func newExplainEngine(cfg config.Config) *engine.Engine {
	return &engine.Engine{
		Optimizer:       optimizer.Default(),
		MaxQuerySamples: cfg.ClickHouse.MaxQuerySamples,
	}
}

// newDryRunExplainer wires the offline explainer: one engine (with the prod
// sample budget) plus a per-head pair of langs — an instant lang for rules and a
// range lang for dashboard panels — so each query previews in the evaluation mode
// the server would run it in. Both the PromQL head (metrics schema) and the LogQL
// head (logs schema) are wired; the corpus is three-headed and each query routes
// to the lang matching its Lang tag. Every lang shares the same fixed evalTime and
// window so the emitted SQL stays deterministic.
func newDryRunExplainer(cfg config.Config) dryRunExplainer {
	metrics := schema.DefaultOTelMetricsFromEnv()
	logs := schema.DefaultOTelLogsFromEnv()
	evalTime := time.Unix(explainEvalUnix, 0).UTC()
	rangeStart := evalTime.Add(-explainRangeWindow)
	return dryRunExplainer{
		eng:          newExplainEngine(cfg),
		promInstant:  prom.NewExplainLang(metrics, evalTime),
		promRange:    prom.NewExplainLangRange(metrics, rangeStart, evalTime, explainRangeStep),
		logqlInstant: &logql.Lang{Schema: logs, Start: evalTime, End: evalTime},
		logqlRange:   &logql.Lang{Schema: logs, Start: rangeStart, End: evalTime, Step: explainRangeStep},
	}
}

// dryRunExplainer adapts engine.DryRunSQL to migrate.Explainer. The SQL it
// reports is byte-identical to what the server would send to ClickHouse for the
// same query. It picks the lang matching the query's source language (PromQL vs
// LogQL) AND its evaluation mode: a panel previews as a range query, a rule as an
// instant query. TraceQL corpus entries have no offline SQL preview yet, so they
// are reported UNSUPPORTED with the language named rather than mis-parsed.
type dryRunExplainer struct {
	eng          *engine.Engine
	promInstant  engine.Lang
	promRange    engine.Lang
	logqlInstant engine.Lang
	logqlRange   engine.Lang
}

func (d dryRunExplainer) Explain(ctx context.Context, q migrate.HarvestedQuery) migrate.Explanation {
	instant, rangeLang, ok := d.langsFor(q.Lang)
	if !ok {
		// The offline SQL preview models the PromQL and LogQL read pipelines. A
		// TraceQL query is still harvested into the corpus (so the report accounts
		// for it), but its SQL is not previewed offline in this wave — report that
		// honestly rather than parsing a TraceQL string as PromQL and surfacing a
		// mislabelled parse error.
		return migrate.Explanation{Err: fmt.Errorf("offline SQL preview covers PromQL and LogQL only; %s query harvested but not previewed here", q.Lang)}
	}
	evalLang := instant
	if q.Kind == migrate.KindPanel {
		evalLang = rangeLang
	}
	dr, err := d.eng.DryRunSQL(ctx, evalLang, q.Expr)
	return migrate.Explanation{SQL: dr.SQL, Plan: dr.Plan, Err: err}
}

// langsFor returns the (instant, range) lang pair for a harvested query's source
// language and whether that language has an offline SQL preview. An empty Lang is
// treated as PromQL for corpora written before the corpus went three-headed.
func (d dryRunExplainer) langsFor(lang string) (instant, rangeLang engine.Lang, ok bool) {
	switch lang {
	case "", migrate.LangPromQL:
		return d.promInstant, d.promRange, true
	case migrate.LangLogQL:
		return d.logqlInstant, d.logqlRange, true
	default:
		return nil, nil, false
	}
}

// writeSchema renders the schema cerberus expects for cfg and writes it to w. It
// takes an already-loaded config so it is unit-testable without touching the
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

// writeReport renders the report in the requested form. The text report is guided
// by the repro command so a failing run ends with a copy-pasteable reproduction.
func writeReport(w io.Writer, rep migrateverify.Report, asJSON bool, repro string) error {
	if asJSON {
		return rep.WriteJSON(w)
	}
	return rep.WriteTextGuided(w, migrateverify.TextGuidance{ReproCommand: repro})
}

// writeReportFile marshals the full JSON diagnostic to path, buffering first so a
// marshal failure never leaves a half-written file behind.
func writeReportFile(path string, diag migrateverify.VerifyReport) error {
	var buf strings.Builder
	if err := diag.WriteJSON(&buf); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(buf.String()), reportFileMode); err != nil {
		return fmt.Errorf("write report file %q: %w", path, err)
	}
	return nil
}

// reproCommand reconstructs the exact, copy-pasteable `cerberus migrate verify …`
// invocation that regenerates this diagnostic, using the RESOLVED window (so a
// relative -1h/now input reproduces the same instants) and suggesting --report.
func reproCommand(p migrateverify.VerifyReportParams) string {
	return strings.Join([]string{
		"cerberus migrate verify",
		"--corpus", shellQuote(p.Corpus),
		"--ref", shellQuote(p.RefURL),
		"--cerberus", shellQuote(p.CerberusURL),
		"--start", shellQuote(p.Start),
		"--end", shellQuote(p.End),
		"--step", shellQuote(p.Step),
		"--tolerance", strconv.FormatFloat(p.Tolerance, 'g', -1, 64),
		"--report", "verify-report.json",
	}, " ")
}

// shellQuote renders s as a single copy-pasteable shell word: bare when it holds
// only safe characters, single-quoted otherwise (with embedded single quotes
// escaped), so URL or path special characters survive a paste.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		if !isShellSafe(s[i]) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isShellSafe reports whether c can appear unquoted in a shell word.
func isShellSafe(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '-' || c == '.' || c == '/' || c == ':' || c == '=' || c == '@' || c == '%' || c == '+':
		return true
	default:
		return false
	}
}

// toolVersion returns the binary's version for the diagnostic, read from the
// embedded module build info. It is "unknown" when the build carries no such
// stamp (e.g. a `go test` binary).
func toolVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return unknownToolVersion
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return unknownToolVersion
}

// verifyFailedError signals a failed parity gate: the run completed and the
// report was written, but cerberus diverged from the reference (or a backend
// errored). main maps it to a dedicated non-zero exit code.
type verifyFailedError struct {
	summary migrateverify.Summary
}

func (e verifyFailedError) Error() string {
	return fmt.Sprintf("parity gate failed: %d diverge, %d error (of %d queries)",
		e.summary.Diverge, e.summary.Error, e.summary.Total)
}

// gateFailedError signals a no-go cutover decision: the gate ran and the
// checklist was written, but a blocking stage failed. main maps it to a
// dedicated non-zero exit code so a no-go is distinguishable from a tool error.
type gateFailedError struct {
	overall string
}

func (e gateFailedError) Error() string {
	return fmt.Sprintf("cutover gate failed: overall %s (a blocking stage said no-go)", e.overall)
}

// envOr returns the environment value for key, or def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envFloat returns the float parsed from the environment value for key, or def
// when the variable is unset. A set-but-unparseable value is an error, not a
// silent fallback to def.
func envFloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid float: %w", key, v, err)
	}
	return f, nil
}
