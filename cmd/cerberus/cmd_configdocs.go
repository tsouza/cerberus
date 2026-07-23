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
			Table: cfgdocRenderTable([]string{"Variable", "Type", "Default", "Description"}, rows),
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

Cerberus is a stateless 12-factor binary configured primarily through
` + "`CERBERUS_*`" + ` environment variables. An optional ` + "`cerberus.yaml`" + `
([below](#configuration-file-optional)) may supply file-level defaults, but the
environment contract always wins - env vars are the source of truth.
ClickHouse and the (optional) OpenTelemetry collector are attached resources
reached through env-var connection inputs, so swapping a local single-node
ClickHouse for a managed cluster, or a sidecar collector for a SaaS ingest URL,
is a matter of flipping env vars and restarting. Every default below is the
value ` + "`internal/config/config.go`" + ` ships out of the box, read live from
the viper loader at generation time.

All boolean knobs accept ` + "`1`/`0`/`true`/`false`" + `, case-insensitive (the full
` + "`strconv.ParseBool`" + ` vocabulary), via one shared parser - so ` + "`true`" + ` and ` + "`1`" + ` are
interchangeable for any ` + "`bool`" + `-typed variable below.

Misconfigured values fail fast: an unparseable duration, an out-of-range
integer, an unknown log level, or a malformed OTLP header list aborts startup
with a clear error rather than silently downgrading behaviour. Secrets (the
ClickHouse password, OTLP bearer tokens) live in this same env-var namespace
and are sourced from a Kubernetes ` + "`Secret`" + `, a Docker ` + "`secrets:`" + ` mount, or a
vault-injecting init container - never committed.

For how these knobs interact with the running service - lifecycle, readiness,
deployment, scaling - see [` + "`operations.md`" + `](operations.md).

## Configuration file (optional)

Cerberus loads configuration through a [viper](https://github.com/spf13/viper)
loader, so an **optional** ` + "`cerberus.yaml`" + ` may supply values alongside the
environment. The resolution order is:

1. **Environment variable** (` + "`CERBERUS_*`" + `) - always wins.
2. **Config file** (` + "`cerberus.yaml`" + `) - fills in anything the environment leaves
   unset.
3. **Built-in default** - the value ` + "`internal/config/config.go`" + ` ships.

The loader probes two paths for ` + "`cerberus.yaml`" + `, in order: the **working
directory** (` + "`.`" + `) and **` + "`/etc/cerberus`" + `**. The file is **purely additive** - it
can only supply a value the operator hasn't set in the environment; it can never
override an env var. That keeps the 12-factor contract intact (the deployment's
environment remains the source of truth) while giving a baked-image or
bare-metal deployment a place to pin defaults without a long ` + "`-e`" + ` list.

The file is **optional and best-effort**: a **missing** ` + "`cerberus.yaml`" + ` is not
an error, and a **malformed** one is tolerated at load time rather than crashing
startup - values still resolve from the environment and the built-in defaults.
Each resolved value, whatever its source, is then run through the **same
fail-fast typed validation** an env value gets: an unparseable duration or an
out-of-range integer supplied by the file aborts startup with the same clear
error it would from an env var.

The keys are the literal ` + "`CERBERUS_*`" + ` names (the loader binds each viper key to
its environment variable). A minimal example pinning the ClickHouse endpoint and
log format:

` + "```yaml" + `
# /etc/cerberus/cerberus.yaml - defaults; any CERBERUS_* env var overrides these
CERBERUS_CH_ADDR: clickhouse.observability.svc:9000
CERBERUS_CH_DATABASE: otel
CERBERUS_LOG_FORMAT: json
CERBERUS_ADMIT_TEMPO: 24
` + "```" + `

Secrets (the ClickHouse password, OTLP bearer tokens) are best left **out** of
the file and injected through the environment from a Kubernetes ` + "`Secret`" + ` or a
vault sidecar, exactly as without a config file - the file is for non-secret
defaults.
{{ range .Sections }}
## {{ .Name }}
{{ if .Intro }}
{{ .Intro }}
{{ end }}
{{ .Table }}
{{ end }}
## Schema overrides and Prometheus resource labels

Two further env-var families shape ClickHouse interaction but are resolved by
` + "`internal/schema`" + ` (not the loader documented above), so they carry no built-in
viper default and are documented in
[` + "`observability.md`" + `](observability.md#schema-shape-overrides):

- **Schema-shape table-name overrides** - ` + "`CERBERUS_SCHEMA_METRICS_*_TABLE`" + `,
  ` + "`CERBERUS_SCHEMA_LOGS_TABLE`" + `, ` + "`CERBERUS_SCHEMA_TRACES_TABLE`" + ` - the table
  names cerberus reads when the ClickHouse layout deviates from the OTel-CH
  exporter defaults. The auto-create hook creates and the query heads read the
  same names, so a rename is consistent end to end.
- **` + "`CERBERUS_PROM_RESOURCE_LABELS`" + `** - allowlist of OTel ` + "`ResourceAttributes`" + `
  keys (dotted form, e.g. ` + "`k8s.namespace.name`" + `) projected as Prometheus labels.
  Empty / unset promotes **every** resource key.

The solver-tuning surface (` + "`CERBERUS_EVAL_ROUTE`" + `, ` + "`CERBERUS_SHARD_*`" + `,
` + "`CERBERUS_SOLVER_TIMEOUT`" + `) is likewise resolved by ` + "`internal/solver`" + ` and is
documented in [` + "`solver.md`" + `](solver.md).

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
