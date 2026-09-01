package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/tsouza/cerberus/internal/config"
)

// newConfigDocsCmd regenerates docs/configuration.md from the single source of
// truth in internal/config: the CERBERUS_* env-key metadata (config.EnvDocs) and
// the LIVE viper loader defaults (config.DocDefaults). Flag parsing is delegated
// to the std flag package (DisableFlagParsing) so the historical single-dash
// `-out` / `-check` invocation used by `just gen-config-docs`, the config-docs
// workflow, and operators keeps working byte-for-byte.
func newConfigDocsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config-docs",
		Short: "Regenerate docs/configuration.md from internal/config metadata",
		Long: "Regenerate docs/configuration.md from the single source of truth in\n" +
			"internal/config (the CERBERUS_* env-key metadata + the live viper loader\n" +
			"defaults). With -check it renders in memory and exits non-zero (without\n" +
			"writing) if the on-disk file is stale; without it the file is regenerated\n" +
			"in place.\n\nUsage: cerberus config-docs [-out docs/configuration.md] [-check]",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return configDocsRun(args, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

// configDocsRun parses the -out/-check flags and regenerates (or drift-checks)
// the configuration reference. Diagnostics land on stderr; a stale file under
// -check, or a render/IO failure, returns a non-zero error.
func configDocsRun(args []string, stdout, stderr io.Writer) error {
	_ = stdout // config-docs writes to a file or reports to stderr; stdout is unused.
	fs := flag.NewFlagSet("config-docs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "docs/configuration.md", "path to write the generated configuration reference")
	check := fs.Bool("check", false, "do not write; exit non-zero if the on-disk file is stale")
	if err := fs.Parse(args); err != nil {
		return err
	}

	doc, err := cfgdocRender()
	if err != nil {
		return fmt.Errorf("config-docs: %w", err)
	}

	if *check {
		existing, err := os.ReadFile(*out) //nolint:gosec // doc artifact path from a flag default, not attacker-controlled
		if err != nil {
			return fmt.Errorf("config-docs: read %s: %w", *out, err)
		}
		if !bytes.Equal(existing, []byte(doc)) {
			return fmt.Errorf("config-docs: %s is stale - run 'just gen-config-docs' and commit the result", *out)
		}
		return nil
	}

	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil { //nolint:gosec // doc artifact is a world-readable source file
		return fmt.Errorf("config-docs: write %s: %w", *out, err)
	}
	fmt.Fprintf(stderr, "config-docs: wrote %s (%d keys, %d groups)\n",
		*out, len(config.AllEnvKeys()), len(config.EnvDocGroups()))
	return nil
}

// cfgdocSection is one rendered group: name, intro prose, aligned table.
type cfgdocSection struct {
	Name  string
	Intro string
	Table string
}

// cfgdocTemplateData is the root passed to the document template. DependencyMatrix
// is rendered through cfgdocRenderTable (rather than hand-written in the template)
// so its pipes always align (markdownlint MD060) and a future edit can't silently
// break alignment.
type cfgdocTemplateData struct {
	Sections         []cfgdocSection
	DependencyMatrix string
}

// cfgdocDependencyMatrixRows is the hand-authored set of cross-setting validation
// rules (knobs individually valid but incoherent in combination). It is
// documentation, not loader state, so it lives here; cfgdocRenderTable keeps it
// aligned. Each row is {Rule, Knobs involved, Why it fails fast}.
var cfgdocDependencyMatrixRows = [][]string{
	{"TLS cert/key are both-or-neither", "`_TLS_CERT_FILE`, `_TLS_KEY_FILE`", "A lone cert or key cannot form an mTLS client key pair."},
	{"TLS sub-knobs require enable", "`_TLS_ENABLED` vs the other `_TLS_*` knobs", "Silently-ignored TLS config is a security footgun."},
	{"skip-verify contradicts CA / server-name", "`_TLS_INSECURE_SKIP_VERIFY` vs `_TLS_CA_FILE` / `_TLS_SERVER_NAME`", "skip-verify ignores both - pinning a CA or hostname alongside it is incoherent."},
	{"HTTP-protocol knobs require `http`", "`CERBERUS_CH_PROTOCOL` vs the `_HTTP_*` protocol knobs", "Under `native` they would be silently dropped."},
	{"Compression level requires a method", "`CERBERUS_CH_COMPRESSION` vs `CERBERUS_CH_COMPRESSION_LEVEL`", "A level with `none` does nothing; a level must sit in the method's range (lz4 `0..12`, zstd `1..22`)."},
	{"Read timeout >= query timeout", "`CERBERUS_CH_READ_TIMEOUT` vs `CERBERUS_QUERY_TIMEOUT`", "A socket read shorter than the query budget would kill legitimate long queries."},
	{"Idle conns <= open conns", "`CERBERUS_CH_MAX_IDLE_CONNS` vs `CERBERUS_CH_MAX_OPEN_CONNS`", "More idle than total pooled connections is a degenerate pool. Fires only when idle is **explicitly set**."},
	{"Server header timeout <= read timeout", "`CERBERUS_HTTP_READ_HEADER_TIMEOUT` vs `CERBERUS_HTTP_READ_TIMEOUT`", "A header deadline longer than the whole-request deadline can never fire."},
}

// cfgdocRender assembles the full docs/configuration.md. The preamble + footer
// prose live in the template header/footer (kept hand-written and reviewed); the
// per-group tables are generated from config.EnvDocs + config.DocDefaults.
func cfgdocRender() (string, error) {
	docs := config.EnvDocs()
	defaults := config.DocDefaults()
	groups := config.EnvDocGroups()

	// Fail loud if a key carries a default but no metadata, or vice versa, so
	// the generator can never silently emit a partial table. (The unit test is
	// the primary guard; this is defence in depth for a direct `go run`.)
	byKey := make(map[string]config.EnvDoc, len(docs))
	for _, d := range docs {
		byKey[d.Key] = d
	}
	for _, k := range config.AllEnvKeys() {
		if _, ok := byKey[k]; !ok {
			return "", fmt.Errorf("env key %q has no EnvDoc metadata (add it to envDocs)", k)
		}
		if _, ok := defaults[k]; !ok {
			return "", fmt.Errorf("env key %q has no loader default", k)
		}
	}

	// Group the docs, preserving envDocs order within each group.
	rowsByGroup := make(map[string][][]string)
	known := make(map[string]bool, len(groups))
	for _, g := range groups {
		known[g.Name] = true
	}
	for _, d := range docs {
		if !known[d.Group] {
			return "", fmt.Errorf("env key %q has group %q which is not in envDocGroups", d.Key, d.Group)
		}
		rowsByGroup[d.Group] = append(rowsByGroup[d.Group], []string{
			"`" + d.Key + "`",
			cfgdocConfigFilePath(d.Key),
			d.Type,
			defaults[d.Key],
			d.Desc,
		})
	}

	sections := make([]cfgdocSection, 0, len(groups))
	for _, g := range groups {
		rows := rowsByGroup[g.Name]
		if len(rows) == 0 {
			return "", fmt.Errorf("group %q has no documented keys", g.Name)
		}
		sections = append(sections, cfgdocSection{
			Name:  g.Name,
			Intro: g.Intro,
			Table: cfgdocRenderTable([]string{"Variable", "Config file", "Type", "Default", "Description"}, rows),
		})
	}

	matrix := cfgdocRenderTable(
		[]string{"Rule", "Knobs involved", "Why it fails fast"},
		cfgdocDependencyMatrixRows,
	)

	var buf bytes.Buffer
	if err := cfgdocTemplate.Execute(&buf, cfgdocTemplateData{
		Sections:         sections,
		DependencyMatrix: matrix,
	}); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	// Guarantee a single trailing newline (markdownlint MD047).
	return strings.TrimRight(buf.String(), "\n") + "\n", nil
}

// cfgdocNoConfigFilePath marks a setting the nested shape has no name for. It is
// still reachable from a config file by its literal CERBERUS_* key, which the
// "Configuration file" section documents as the long tail's escape hatch.
const cfgdocNoConfigFilePath = "—"

// cfgdocConfigFilePath renders the nested path a setting is written as in a
// cerberus.yaml. It reads the binding table the loader itself uses, so the
// column cannot document a spelling the loader would reject.
func cfgdocConfigFilePath(key string) string {
	path, ok := config.ConfigFilePath(key)
	if !ok {
		return cfgdocNoConfigFilePath
	}
	return "`" + path + "`"
}

// cfgdocRenderTable emits a markdownlint-MD060-compliant aligned table: every
// pipe lines up because each cell is padded to its column's rune width. All
// columns are left-aligned. A literal `|` inside a cell (e.g. the `int | bool`
// admit type) is escaped to `\|` so it does not spuriously split the cell (MD056).
func cfgdocRenderTable(header []string, rows [][]string) string {
	cols := len(header)
	escaped := make([][]string, len(rows))
	for ri, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = strings.ReplaceAll(c, "|", `\|`)
		}
		escaped[ri] = cells
	}
	rows = escaped

	widths := make([]int, cols)
	for i, h := range header {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if w := utf8.RuneCountInString(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	for i := range widths {
		if widths[i] < 3 {
			widths[i] = 3
		}
	}

	var b strings.Builder
	writeRow := func(cells []string) {
		b.WriteString("|")
		for i, c := range cells {
			pad := widths[i] - utf8.RuneCountInString(c)
			b.WriteString(" " + c + strings.Repeat(" ", pad) + " |")
		}
		b.WriteString("\n")
	}
	writeRow(header)
	b.WriteString("|")
	for i := range header {
		b.WriteString(" " + strings.Repeat("-", widths[i]) + " |")
	}
	b.WriteString("\n")
	for _, r := range rows {
		writeRow(r)
	}
	return strings.TrimRight(b.String(), "\n")
}

// cfgdocTemplate is the document skeleton: a hand-written, code-reviewed preamble
// and footer wrapping the GENERATED per-group tables. Everything between the
// preamble and the footer is rendered from config.EnvDocs + config.DocDefaults,
// so the env-var tables cannot drift; the prose around them is preserved verbatim
// because it documents behaviour, not a single knob's default.
var cfgdocTemplate = template.Must(template.New("configuration.md").Parse(cfgdocTemplateText))

const cfgdocTemplateText = `<!-- Code-generated by cmd/cerberus config-docs from internal/config metadata +
     the live viper loader defaults. DO NOT EDIT this file by hand: edit the
     EnvDoc metadata in internal/config/envdocs.go (or the preamble/footer in
     cmd/cerberus/cmd_configdocs.go) and run "just gen-config-docs". CI gates on
     drift, so a hand-edit will fail the build. -->

# Configuration

Cerberus is a stateless 12-factor binary configured through ` + "`CERBERUS_*`" + `
environment variables or an equivalent ` + "`cerberus.yaml`" + `
([below](#configuration-file)) - the same settings either way, with the
environment winning where both speak.
ClickHouse and the (optional) OpenTelemetry collector are attached resources
reached through env-var connection inputs, so swapping a local single-node
ClickHouse for a managed cluster, or a sidecar collector for a SaaS ingest URL,
is a matter of flipping env vars and restarting. Every default below is the
value ` + "`internal/config/config.go`" + ` ships out of the box, read live from
the viper loader at generation time.

All boolean knobs accept ` + "`1`/`0`/`true`/`false`" + `, case-insensitive (the full
` + "`strconv.ParseBool`" + ` vocabulary), via one shared parser - so ` + "`true`" + ` and ` + "`1`" + ` are
interchangeable for any ` + "`bool`" + `-typed variable below.

All ` + "`duration`" + `-typed knobs share one grammar, via one shared parser: Go
duration syntax (` + "`300ms`" + `, ` + "`1.5h`" + `, ` + "`2160h`" + `) **plus** the retention units Go has
no spelling for (` + "`90d`" + `, ` + "`2w`" + `, ` + "`1y`" + `, fixed at 24h / 7d / 365d). Both spellings
mean the same thing for every variable below, so a retention written ` + "`90d`" + ` and
one written ` + "`2160h`" + ` are the same value wherever it is set. Calendar months and
leap-aware years are deliberately absent - they are variable-length and cannot
round-trip through a fixed duration.

That grammar accepts a leading sign, so the range is stated separately: no
` + "`duration`" + `-typed variable below accepts a negative value, and startup aborts
naming the variable if one is set. Where ` + "`0`" + ` is meaningful it is documented per
knob (usually "leave the underlying default in place"); where it would leave a
feature switched on but inert, it is rejected too.

Misconfigured values fail fast: an unparseable duration, an out-of-range
integer, an unknown log level, or a malformed OTLP header list aborts startup
with a clear error rather than silently downgrading behaviour. Secrets (the
ClickHouse password, OTLP bearer tokens) live in this same env-var namespace
and are sourced from a Kubernetes ` + "`Secret`" + `, a Docker ` + "`secrets:`" + ` mount, or a
vault-injecting init container - never committed.

For how these knobs interact with the running service - lifecycle, readiness,
deployment, scaling - see [` + "`operations.md`" + `](operations.md).

## Configuration file

Everything above can go in a ` + "`cerberus.yaml`" + ` instead of the environment. The
file is **exactly equivalent** to exporting the variables it names: no setting
is parsed differently and none resolves to a different value, so the two are
interchangeable and mixable. The resolution order is:

1. **Environment variable** (` + "`CERBERUS_*`" + `) - always wins.
2. **Config file** (` + "`cerberus.yaml`" + `) - fills in anything the environment leaves
   unset.
3. **Built-in default** - the value ` + "`internal/config/config.go`" + ` ships.

Cerberus looks for ` + "`cerberus.yaml`" + ` (or ` + "`cerberus.yml`" + `) in two places, in
order: the **working directory** (` + "`.`" + `) and **` + "`/etc/cerberus`" + `**. A **missing**
file is not an error - the environment configures cerberus completely on its
own. A file that **exists but cannot be understood** - malformed YAML, or a key
cerberus does not recognise - **is** an error, and cerberus refuses to start
rather than silently running on defaults nobody chose. The error names the key
and suggests the nearest one that exists:

` + "```text" + `
load config: cerberus.yaml: unknown setting: clickhouse.maxSamples
  clickhouse.maxSamples — did you mean query.maxSamples?
` + "```" + `

Each resolved value, whatever its source, then goes through the **same
fail-fast typed validation** an env value gets: an unparseable duration or an
out-of-range integer supplied by the file aborts startup with the same clear
error it would from an env var.

### The shape

The file is written in the **same shape as the Helm chart's ` + "`values.yaml`" + `**, so
a setting has one name whether you deploy cerberus with the chart or run the
binary yourself. Every table in this document carries the config-file path in
its **Config file** column.

` + "```yaml" + `
# /etc/cerberus/cerberus.yaml
clickhouse:
  addr:
    - clickhouse.observability.svc:9000
  database: otel
query:
  maxSamples: 5000000
  timeout: 2m
admit:
  tempo: 24
logFormat: json
` + "```" + `

The literal ` + "`CERBERUS_*`" + ` form is valid in the same document and means the same
thing. It is the escape hatch for the long tail - the settings with a ` + "`—`" + ` in the
**Config file** column have no nested name, and this is how you reach them:

` + "```yaml" + `
clickhouse:
  database: otel
CERBERUS_CH_BREAKER_THRESHOLD: 9
` + "```" + `

` + "`cerberus migrate`" + ` reads the same file, under a ` + "`migrate:`" + ` block - so one
document configures a migration and the gateway it migrates to. See
[` + "`migration.md`" + `](migration.md).

Secrets (the ClickHouse password, OTLP bearer tokens) are best left **out** of
the file and injected through the environment from a Kubernetes ` + "`Secret`" + ` or a
vault sidecar - the file is for non-secret settings.
{{ range .Sections }}
## {{ .Name }}
{{ if .Intro }}
{{ .Intro }}
{{ end }}
{{ .Table }}
{{ end }}
## Schema overrides and Prometheus resource labels

Two further setting families shape ClickHouse interaction but are resolved by
` + "`internal/schema`" + ` rather than by the loader documented above - it owns their
defaults, so they carry none of their own. They read from a config file exactly
as everything else does (` + "`schema.metrics.gaugeTable`" + `, ` + "`schema.logs.table`" + `,
` + "`schema.traces.table`" + `, ` + "`prom.resourceLabels`" + `), and are documented in
[` + "`observability.md`" + `](observability.md#schema-shape-overrides):

- **Schema-shape table-name overrides** - ` + "`CERBERUS_SCHEMA_METRICS_*_TABLE`" + `,
  ` + "`CERBERUS_SCHEMA_LOGS_TABLE`" + `, ` + "`CERBERUS_SCHEMA_TRACES_TABLE`" + ` - the table
  names cerberus reads when the ClickHouse layout deviates from the OTel-CH
  exporter defaults. The auto-create hook creates and the query heads read the
  same names, so a rename is consistent end to end.
- **` + "`CERBERUS_PROM_RESOURCE_LABELS`" + `** - allowlist of OTel ` + "`ResourceAttributes`" + `
  keys (dotted form, e.g. ` + "`k8s.namespace.name`" + `) projected as Prometheus labels.
  Empty / unset promotes **every** resource key.

The solver-tuning surface is likewise resolved by ` + "`internal/solver`" + ` rather
than the loader documented above (` + "`internal/solver/config_env.go`" + `).
` + "`CERBERUS_EVAL_ROUTE`" + ` is the master switch (default ` + "`auto`" + `; see
[` + "`operations.md`" + `](operations.md#sharded-pushdown-solver) for its three modes)
and ` + "`CERBERUS_SOLVER_ADAPTIVE_ENABLED`" + ` / the route-memo knobs are covered in
[` + "`solver.md`" + `](solver.md). The remaining seven tuning knobs default to
` + "`DefaultConfig`" + `'s conservative values and are otherwise undocumented
elsewhere, so they are enumerated here:

- **` + "`CERBERUS_SHARD_MIN_FANOUT`" + `** (int, default ` + "`16`" + `) - the minimum
  fan-out a plan must clear before the Planner even considers routing it.
- **` + "`CERBERUS_SHARD_MIN_ANCHOR_PAIRS`" + `** (int, default ` + "`4000`" + `) - the
  minimum anchor-pair count a plan must clear before routing.
- **` + "`CERBERUS_SHARD_MAX_K`" + `** (int, default ` + "`8`" + `) - the hard ceiling on
  how many shards a single request may be split into.
- **` + "`CERBERUS_SHARD_MIN_ANCHORS_PER_SLICE`" + `** (int, default ` + "`16`" + `) - the
  minimum anchors a slice must carry, so K never grows past what the anchor
  set can meaningfully divide.
- **` + "`CERBERUS_SHARD_PARALLEL`" + `** (int, default ` + "`3`" + `) - how many shards
  execute concurrently per request.
- **` + "`CERBERUS_SOLVER_TIMEOUT`" + `** (duration, default ` + "`60s`" + `) - the
  per-request deadline for the whole route-B fan-out.
- **` + "`CERBERUS_SHARD_MAX_OUTPUT_ROWS`" + `** (int64, default ` + "`2000000`" + `) -
  the per-request output-row ceiling across all shards combined.

Six further resource-bound safety ceilings (five from issue #2667, the last
from issue #2733) are resolved by ` + "`internal/chsql`" + ` and
` + "`internal/promql`" + ` rather than by the loader
documented above - those two packages may not import ` + "`internal/config`" + `
(` + "`.go-arch-lint.yml`" + `), so each owns a small, self-contained env-parsing file
instead (` + "`internal/engine/resource_bound_env.go`" + `,
` + "`internal/promql/resource_bounds_env.go`" + `):

- **` + "`CERBERUS_CH_RANGE_BUCKET_FANOUT_MAX_ROWS`" + `** (int64, default ` + "`4000000`" + `) -
  RangeBucketFanout's collapse GROUP BY row ceiling
  (` + "`internal/chsql/lwr_fanout_bound.go`" + `, ` + "`maxRangeBucketFanoutRows`" + `).
- **` + "`CERBERUS_CH_RANGE_LWR_FANOUT_MAX_ROWS`" + `** (int64, default ` + "`40000000`" + `) -
  RangeLWR's collapse GROUP BY row ceiling (same file, ` + "`maxRangeLWRFanoutRows`" + `).
- **` + "`CERBERUS_CH_RATE_WINDOW_FANOUT_MAX_ROWS`" + `** (int64, default ` + "`2800000`" + `) -
  the windowed-array-extrapolated-matrix regroup GROUP BY row ceiling
  (` + "`internal/chsql/rate_window_fanout_bound.go`" + `, ` + "`maxRateWindowFanoutRows`" + `).
- **` + "`CERBERUS_PROMQL_HISTOGRAM_MERGE_MAX_COST_UNITS`" + `** (int64, default
  ` + "`60000000`" + `) - the native-histogram across-series merge cost ceiling that
  closed issue #2385 after 19 real production OOMs
  (` + "`internal/promql/histogram_merge_bound.go`" + `, ` + "`maxHistogramMergeCostUnits`" + `).
- **` + "`CERBERUS_PROMQL_CLASSIC_BUCKET_MERGE_MAX_COST_UNITS`" + `** (int64, default
  ` + "`10000000`" + `) - the classic-histogram across-series bucket-merge cost ceiling
  (` + "`internal/promql/classic_bucket_merge_bound.go`" + `,
  ` + "`maxClassicBucketMergeCostUnits`" + `).
- **` + "`CERBERUS_CH_MAX_EMITTED_SQL_BYTES`" + `** (int64, default ` + "`262144`" + `) -
  the bytes of SQL cerberus will emit for one statement
  (` + "`internal/chsql/emit_size_bound.go`" + `, ` + "`maxEmittedSQLBytes`" + `).
  Applies to all three heads. Its default is ClickHouse's own
  ` + "`max_query_size`" + ` default rather than a cerberus calibration: a statement
  past it is one the server refuses to parse anyway, so the bound turns a raw
  driver ` + "`code 62`" + ` into a cerberus error naming the query shape. Raise it
  only alongside ` + "`max_query_size`" + ` on the server itself.

All six reject a malformed or non-positive override at startup rather than
silently falling back to the default or admitting every query; see each
constant's own doc for the calibration its shipped default protects.

The query-actuals predicted-vs-actual drift tracker (issue #2789) is
likewise resolved by ` + "`internal/actuals`" + ` rather than the loader documented
above (` + "`internal/actuals/config_env.go`" + `) - the same "kept in its own package
to avoid an import cycle" reasoning ` + "`CERBERUS_SOLVER_*`" + ` uses above. It is a
plain solver-policy config knob, **not** a ` + "`CERBERUS_CH_OPTIMIZATIONS`" + ` chopt
feature: ProfileEvents on the native protocol and ` + "`system.query_log`" + ` are
both ancient, always-available ClickHouse surfaces with no version floor to
probe. ` + "`CERBERUS_QUERY_ACTUALS_ENABLED`" + ` is the master switch (default
` + "`false`" + ` - the feature ships dark); every other knob below is inert while it
is unset:

- **` + "`CERBERUS_QUERY_ACTUALS_ENABLED`" + `** (bool, default ` + "`false`" + `) - master
  switch. Records EXPLAIN ESTIMATE / cardinality-probe predictions against
  the REAL read_rows/read_bytes/peak-memory a dispatch actually consumed
  (native-protocol progress/ProfileEvents packets, or ` + "`system.query_log`" + ` as
  the batch fallback), and alerts when a plan shape's predicted-vs-actual
  ratio drifts beyond ` + "`CERBERUS_QUERY_ACTUALS_DRIFT_LOWER_RATIO`" + ` /
  ` + "`_UPPER_RATIO`" + ` - see [` + "`solver.md`" + `](solver.md) for the full mechanism.
  Prom-only: the feature keys off the solver's own plan-shape-id / K-clamp
  machinery.
- **` + "`CERBERUS_QUERY_ACTUALS_DRIFT_LOWER_RATIO`" + `** / **` + "`_UPPER_RATIO`" + `** (float,
  default ` + "`0.1`" + ` / ` + "`3.0`" + `) - the "expected" band for actual-EMA/predicted.
  EXPLAIN ESTIMATE is a granule-resolution UPPER BOUND, so the band is
  deliberately asymmetric around 1.0 rather than a symmetric +/-X%.
- **` + "`CERBERUS_QUERY_ACTUALS_MIN_OBSERVATIONS`" + `** (int, default ` + "`2`" + `) - the
  corroboration floor before a shape's drift verdict is trusted at all.
- **` + "`CERBERUS_QUERY_ACTUALS_EMA_ALPHA`" + `** (float, default ` + "`0.2`" + `) - the bounded
  exponential-moving-average smoothing factor for the tracked actual-rows
  side; caps how far a single anomalous observation can move it.
- **` + "`CERBERUS_QUERY_ACTUALS_ENTRY_TTL`" + `** (duration, default ` + "`30m`" + `) - how long
  a shape's tracked state is trusted before it ages out.
- **` + "`CERBERUS_QUERY_ACTUALS_QUERY_LOG_POLL_INTERVAL`" + `** (duration, default
  ` + "`60s`" + `) - how often the ` + "`system.query_log`" + ` batch/fallback reconciler polls.
- **` + "`CERBERUS_QUERY_ACTUALS_QUERY_LOG_LOOKBACK`" + `** (duration, default ` + "`180s`" + `)
  - the overlap margin the reconciler's first poll (and any recovery poll)
  looks back by, sized well above the poll interval so a slow query_log
  flush never drops a row between two polls.

## Dependency matrix

Most knobs are validated in isolation (unknown enum, out-of-range buffer,
malformed URL, non-positive where positive is required). Some knobs, however,
only make sense in combination - an individually-valid value can be incoherent
next to another. Cerberus rejects these **combinations** at startup with an
error that names both knobs, rather than silently ignoring or downgrading one
of them. The full set of cross-setting rules:

{{ .DependencyMatrix }}

Benign-but-pointless combinations are **not** hard errors - they are noted here
rather than rejected:

- ` + "`CERBERUS_CH_CONN_OPEN_STRATEGY=round_robin`" + ` with a single ` + "`CERBERUS_CH_ADDR`" + `
  host: the strategy has nothing to rotate over, but it is harmless.
- Keepalive timing sub-knobs (` + "`CERBERUS_CH_KEEPALIVE_IDLE`" + ` / ` + "`_INTERVAL`" + ` /
  ` + "`_COUNT`" + `) while ` + "`CERBERUS_CH_KEEPALIVE_ENABLED=false`" + `: inert (the kernel never
  arms a probe schedule), so a degenerate value is accepted, not rejected.
`
