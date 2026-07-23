package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/routerrules"
)

// newRouteRulesCmd is the offline operator entrypoint for the router-rules
// catalog. It mines the cerberus_router_corpus table (or its per-pod JSONL
// fallback) and prints findings where the recorded A/B routing decision is paying
// an observable cost the corpus shows the other route would avoid. It changes no
// routing — it is a report generator. Flag parsing is delegated to the std flag
// package (DisableFlagParsing) so the historical single-dash flags and the
// `benchmark` pseudo-subcommand keep working exactly as before.
func newRouteRulesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "route-rules",
		Short: "Offline router-rules catalog analysis (report generator; changes no routing)",
		Long: "Mine the cerberus_router_corpus table (or its per-pod JSONL fallback) and\n" +
			"print findings where the recorded A/B route is paying a cost the corpus\n" +
			"shows the other route would avoid. The nested `benchmark` verb scores the\n" +
			"catalog against a fabricated labeled corpus.\n\n" +
			"Usage: cerberus route-rules [-source jsonl|chtable] [-corpus-path PATH] [-validate-only] ...\n" +
			"       cerberus route-rules benchmark [-seed N] [-min-support N]",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return routeRulesRun(args, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

// rrBenchmarkSubcommand is the offline detection-fidelity benchmark verb: it
// scores the catalog against a fabricated, seed-deterministic labeled corpus and
// prints the per-rule precision/recall/F1 table. It takes no corpus path.
const rrBenchmarkSubcommand = "benchmark"

type routeRulesOptions struct {
	catalogPath  string
	source       string
	corpusPath   string
	since        time.Duration
	format       string
	validateOnly bool
	experimental bool
	params       rrParamFlags
}

type rrParamFlags map[string]string

func (p rrParamFlags) String() string { return fmt.Sprintf("%v", map[string]string(p)) }

func (p rrParamFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("--param expects KEY=VALUE, got %q", v)
	}
	p[k] = val
	return nil
}

func routeRulesRun(args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == rrBenchmarkSubcommand {
		return routeRulesRunBenchmark(args[1:], stdout, stderr)
	}

	opts := routeRulesOptions{params: rrParamFlags{}}
	fs := flag.NewFlagSet("route-rules", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.catalogPath, "catalog", "", "path to a router-rules catalog YAML (default: embedded)")
	fs.StringVar(&opts.source, "source", "jsonl", "corpus source: chtable|jsonl")
	fs.StringVar(&opts.corpusPath, "corpus-path", "", "JSONL corpus file or directory (jsonl source)")
	fs.DurationVar(&opts.since, "since", 0, "event_time lookback window (e.g. 720h); 0 = unbounded")
	fs.StringVar(&opts.format, "format", "table", "output format: table|json")
	fs.BoolVar(&opts.validateOnly, "validate-only", false, "load + validate the catalog, resolve nothing, exit")
	fs.Var(opts.params, "param", "config-kind param override KEY=VALUE (repeatable)")
	fs.BoolVar(&opts.experimental, "experimental", false, "also evaluate rules with status: experimental")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cat, err := rrLoadCatalog(opts.catalogPath)
	if err != nil {
		return err
	}

	if opts.validateOnly {
		fmt.Fprintf(stdout, "catalog valid: apiVersion=%s catalogVersion=%d params=%d rules=%d\n",
			cat.APIVersion, cat.CatalogVersion, len(cat.Params), len(cat.Rules))
		return nil
	}

	cfg := rrNewConfigLookup(opts.params)
	src, err := rrBuildSource(opts)
	if err != nil {
		return err
	}

	ev := routerrules.NewEvaluator(cat, cfg, src)
	report, err := ev.Evaluate(context.Background(), routerrules.EvalOptions{IncludeExperimental: opts.experimental})
	if err != nil {
		return err
	}

	switch opts.format {
	case "json":
		return rrWriteJSON(stdout, report)
	case "table":
		return rrWriteTable(stdout, report)
	default:
		return fmt.Errorf("unknown --format %q (want table|json)", opts.format)
	}
}

// rrBenchDefaultConfig is the nominal operating point the benchmark scores at: a
// p95 watermark on both percentile knobs and a min_rows_per_class of 5. These are
// benchmark settings, not catalog thresholds; an operator can override any with
// --param.
var rrBenchDefaultConfig = map[string]string{
	"router_rules.watermark_percentile":     "0.95",
	"router_rules.cumulative_d_percentile":  "0.95",
	"router_rules.min_rows_per_class":       "5",
	"router_rules.memory_near_cap_fraction": "0.8",
	"query.max_memory_bytes":                "1073741824",
	"query.max_samples":                     "50000000",
}

func routeRulesRunBenchmark(args []string, stdout, stderr io.Writer) error {
	params := rrParamFlags{}
	fs := flag.NewFlagSet("route-rules benchmark", flag.ContinueOnError)
	fs.SetOutput(stderr)
	catalogPath := fs.String("catalog", "", "path to a router-rules catalog YAML (default: embedded)")
	seed := fs.Int64("seed", 1, "PRNG seed for the fabricated labeled corpus (deterministic)")
	minSupport := fs.Int("min-support", 5, "min_rows_per_class the corpus is sized and scored at")
	fs.Var(params, "param", "config-kind param override KEY=VALUE (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cat, err := rrLoadCatalog(*catalogPath)
	if err != nil {
		return err
	}

	cfg := routerrules.BenchConfig{}
	for k, v := range rrBenchDefaultConfig {
		cfg[k] = v
	}
	cfg["router_rules.min_rows_per_class"] = strconv.Itoa(*minSupport)
	for k, v := range params {
		cfg[k] = v
	}

	corpus := routerrules.GenerateBenchCorpus(routerrules.BenchParams{Seed: *seed, MinSupport: *minSupport})
	metrics, err := routerrules.ScoreCatalog(context.Background(), cat, cfg, corpus)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "router-rules detection-fidelity benchmark (seed=%d, rows=%d, labeled classes=%d):\n\n",
		*seed, len(corpus.Rows), len(corpus.Classes))
	fmt.Fprint(stdout, routerrules.FormatMetricsTable(metrics))
	return nil
}

func rrLoadCatalog(path string) (*routerrules.Catalog, error) {
	if path == "" {
		return routerrules.LoadEmbeddedCatalog()
	}
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied catalog path.
	if err != nil {
		return nil, fmt.Errorf("read catalog %q: %w", path, err)
	}
	return routerrules.LoadCatalog(data)
}

func rrBuildSource(opts routeRulesOptions) (routerrules.CorpusSource, error) {
	sinceUnix := float64(0)
	if opts.since > 0 {
		sinceUnix = float64(time.Now().Add(-opts.since).Unix())
	}
	switch opts.source {
	case "jsonl":
		if opts.corpusPath == "" {
			return nil, errors.New("--source jsonl requires --corpus-path")
		}
		return routerrules.NewJSONLCorpusSource(opts.corpusPath, sinceUnix), nil
	case "chtable":
		conn, err := rrConnectClickHouse()
		if err != nil {
			return nil, err
		}
		return routerrules.NewCHCorpusSource(conn, sinceUnix), nil
	default:
		return nil, fmt.Errorf("unknown --source %q (want chtable|jsonl)", opts.source)
	}
}

// rrConfigEnvKeys maps the catalog's dotted config keys to the deployment env
// vars that carry their values. A --param override takes precedence over the env.
var rrConfigEnvKeys = map[string]string{
	"query.max_memory_bytes":                "CERBERUS_CH_QUERY_MAX_MEMORY",
	"query.max_samples":                     "CERBERUS_QUERY_MAX_SAMPLES",
	"router_rules.watermark_percentile":     "ROUTER_RULES_WATERMARK_PERCENTILE",
	"router_rules.min_rows_per_class":       "ROUTER_RULES_MIN_ROWS_PER_CLASS",
	"router_rules.memory_near_cap_fraction": "ROUTER_RULES_MEMORY_NEAR_CAP_FRACTION",
}

// rrNewConfigLookup resolves a catalog config key from --param overrides first,
// then from the mapped deployment env var. routerrules never imports
// internal/config; the CLI is the single boundary where deployment numbers enter.
func rrNewConfigLookup(overrides rrParamFlags) routerrules.ConfigLookup {
	return func(key string) (string, bool) {
		if v, ok := overrides[key]; ok {
			return v, true
		}
		if env, ok := rrConfigEnvKeys[key]; ok {
			if v, ok := os.LookupEnv(env); ok {
				return v, true
			}
		}
		return "", false
	}
}

// rrConnectClickHouse builds a CH connection for the chtable source from the
// deployment's own environment, returning the narrow driver.Conn the source needs.
func rrConnectClickHouse() (routerrules.CHConn, error) {
	cfg, err := config.FromEnv()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	client, err := chclient.New(cfg.ClickHouse)
	if err != nil {
		return nil, fmt.Errorf("connect ClickHouse: %w", err)
	}
	return client.Conn(), nil
}

func rrWriteJSON(out io.Writer, report *routerrules.Report) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func rrWriteTable(out io.Writer, report *routerrules.Report) error {
	if len(report.Findings) == 0 {
		fmt.Fprintln(out, "no findings")
	} else {
		tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "SEVERITY\tRULE\tCLASS\tSUPPORT\tACTION\tMESSAGE")
		for _, f := range report.Findings {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				f.Severity, f.RuleID, rrFormatClass(f.GroupKey), strconv.FormatInt(f.Support, 10), f.Action, f.Message)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	// Skipped rules are surfaced, not silent: a rule that gates on a watermark
	// learned from an empty sub-population has no signal and is not evaluated.
	for _, s := range report.Skipped {
		fmt.Fprintf(out, "skipped %s: %s\n", s.RuleID, s.Reason)
	}
	return nil
}

func rrFormatClass(gk map[string]string) string {
	keys := make([]string, 0, len(gk))
	for k := range gk {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + gk[k]
	}
	return strings.Join(parts, ",")
}
