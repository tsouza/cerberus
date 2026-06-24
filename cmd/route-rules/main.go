// Command route-rules is the offline operator entrypoint for the router-rules
// catalog. It mines the cerberus_router_corpus table (or its per-pod JSONL
// fallback) and prints findings: shape classes where the recorded A/B routing
// decision is paying an observable cost the corpus shows the other route would
// avoid. It changes no routing — it is a report generator.
//
// The shipped catalog carries only generic drivers (rule structure + named
// parameters); every threshold is resolved at runtime, per-deployment, from the
// deployment's own corpus aggregates or its config. See docs/router-rules.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/routerrules"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "route-rules:", err)
		os.Exit(1)
	}
}

type options struct {
	catalogPath  string
	source       string
	corpusPath   string
	since        time.Duration
	format       string
	validateOnly bool
	experimental bool
	params       paramFlags
}

type paramFlags map[string]string

func (p paramFlags) String() string { return fmt.Sprintf("%v", map[string]string(p)) }

func (p paramFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("--param expects KEY=VALUE, got %q", v)
	}
	p[k] = val
	return nil
}

func run(args []string, stdout, stderr *os.File) error {
	opts := options{params: paramFlags{}}
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

	cat, err := loadCatalog(opts.catalogPath)
	if err != nil {
		return err
	}

	if opts.validateOnly {
		fmt.Fprintf(stdout, "catalog valid: apiVersion=%s catalogVersion=%d params=%d rules=%d\n",
			cat.APIVersion, cat.CatalogVersion, len(cat.Params), len(cat.Rules))
		return nil
	}

	cfg := newConfigLookup(opts.params)
	src, err := buildSource(opts)
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
		return writeJSON(stdout, report)
	case "table":
		return writeTable(stdout, report)
	default:
		return fmt.Errorf("unknown --format %q (want table|json)", opts.format)
	}
}

func loadCatalog(path string) (*routerrules.Catalog, error) {
	if path == "" {
		return routerrules.LoadEmbeddedCatalog()
	}
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied catalog path.
	if err != nil {
		return nil, fmt.Errorf("read catalog %q: %w", path, err)
	}
	return routerrules.LoadCatalog(data)
}

func buildSource(opts options) (routerrules.CorpusSource, error) {
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
		conn, err := connectClickHouse()
		if err != nil {
			return nil, err
		}
		return routerrules.NewCHCorpusSource(conn, sinceUnix), nil
	default:
		return nil, fmt.Errorf("unknown --source %q (want chtable|jsonl)", opts.source)
	}
}

// configEnvKeys maps the catalog's dotted config keys to the deployment env vars
// that carry their values. A --param override takes precedence over the env. The
// router-rules-specific keys have no cerberus env yet, so they are supplied
// exclusively via --param (or a future env), keeping zero numbers in the repo.
var configEnvKeys = map[string]string{
	"query.max_memory_bytes":            "CERBERUS_CH_QUERY_MAX_MEMORY",
	"query.max_samples":                 "CERBERUS_QUERY_MAX_SAMPLES",
	"router_rules.watermark_percentile": "ROUTER_RULES_WATERMARK_PERCENTILE",
	"router_rules.min_rows_per_class":   "ROUTER_RULES_MIN_ROWS_PER_CLASS",
}

// newConfigLookup resolves a catalog config key from --param overrides first,
// then from the mapped deployment env var. routerrules never imports
// internal/config; the CLI is the single boundary where deployment numbers
// enter, so a reviewer confirms the invariant by checking that no number is
// hard-coded here either.
func newConfigLookup(overrides paramFlags) routerrules.ConfigLookup {
	return func(key string) (string, bool) {
		if v, ok := overrides[key]; ok {
			return v, true
		}
		if env, ok := configEnvKeys[key]; ok {
			if v, ok := os.LookupEnv(env); ok {
				return v, true
			}
		}
		return "", false
	}
}

// connectClickHouse builds a CH connection for the chtable source from the
// deployment's own environment (the same CERBERUS_CH_* knobs cerberus boots
// with), returning the narrow driver.Conn the source needs.
func connectClickHouse() (routerrules.CHConn, error) {
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

func writeJSON(out *os.File, report *routerrules.Report) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func writeTable(out *os.File, report *routerrules.Report) error {
	if len(report.Findings) == 0 {
		fmt.Fprintln(out, "no findings")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SEVERITY\tRULE\tCLASS\tSUPPORT\tACTION\tMESSAGE")
	for _, f := range report.Findings {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			f.Severity, f.RuleID, formatClass(f.GroupKey), strconv.FormatInt(f.Support, 10), f.Action, f.Message)
	}
	return tw.Flush()
}

func formatClass(gk map[string]string) string {
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
