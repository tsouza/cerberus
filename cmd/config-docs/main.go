// Command config-docs regenerates docs/configuration.md from the single
// source of truth in internal/config: the CERBERUS_* env-key metadata
// (config.EnvDocs) and the LIVE viper loader defaults (config.DocDefaults).
//
// The document cannot drift from the code because every column is derived:
// the key set comes from config.AllEnvKeys (the same slice newLoader binds),
// the Default comes from config.DocDefaults (read straight off a freshly-built
// loader with no env set), and the Type/Group/Desc come from the hand-authored,
// code-reviewed config.EnvDocs metadata. A test (TestEnvDocsCoverAllKeys)
// asserts EnvDocs <-> AllEnvKeys is 1:1, and a CI drift gate runs
// `git diff --exit-code docs/configuration.md` after regenerating, so an
// undocumented env var fails `go test` and a stale doc fails CI.
//
// Run via `just gen-config-docs`. Usage:
//
//	config-docs [-out docs/configuration.md] [-check]
//
// With -check the command renders to memory and exits non-zero (without
// writing) if the on-disk file differs, printing a unified-ish hint. Without
// it the file is regenerated in place.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/tsouza/cerberus/internal/config"
)

func main() {
	out := flag.String("out", "docs/configuration.md", "path to write the generated configuration reference")
	check := flag.Bool("check", false, "do not write; exit non-zero if the on-disk file is stale")
	flag.Parse()

	doc, err := render()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config-docs: %v\n", err)
		os.Exit(1)
	}

	if *check {
		existing, err := os.ReadFile(*out) //nolint:gosec // doc artifact path
		if err != nil {
			fmt.Fprintf(os.Stderr, "config-docs: read %s: %v\n", *out, err)
			os.Exit(1)
		}
		if !bytes.Equal(existing, []byte(doc)) {
			fmt.Fprintf(os.Stderr, "config-docs: %s is stale - run 'just gen-config-docs' and commit the result\n", *out)
			os.Exit(1)
		}
		return
	}

	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil { //nolint:gosec // doc artifact
		fmt.Fprintf(os.Stderr, "config-docs: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "config-docs: wrote %s (%d keys, %d groups)\n",
		*out, len(config.AllEnvKeys()), len(config.EnvDocGroups()))
}

// section is one rendered group: its name, intro prose, and aligned table.
type section struct {
	Name  string
	Intro string
	Table string
}

// templateData is the root passed to the document template.
type templateData struct {
	Sections []section
	// DependencyMatrix is the rendered cross-setting validation table. It is
	// generated through renderTable (rather than hand-written in the template)
	// so its pipes always align (markdownlint MD060) and a future edit can't
	// silently break the alignment.
	DependencyMatrix string
}

// dependencyMatrixRows is the hand-authored set of cross-setting validation
// rules (knobs that are individually valid but incoherent in combination). It
// is documentation, not loader state, so it lives here; renderTable keeps it
// aligned. Each row is {Rule, Knobs involved, Why it fails fast}.
var dependencyMatrixRows = [][]string{
	{"TLS cert/key are both-or-neither", "`_TLS_CERT_FILE`, `_TLS_KEY_FILE`", "A lone cert or key cannot form an mTLS client key pair."},
	{"TLS sub-knobs require enable", "`_TLS_ENABLED` vs the other `_TLS_*` knobs", "Silently-ignored TLS config is a security footgun."},
	{"skip-verify contradicts CA / server-name", "`_TLS_INSECURE_SKIP_VERIFY` vs `_TLS_CA_FILE` / `_TLS_SERVER_NAME`", "skip-verify ignores both - pinning a CA or hostname alongside it is incoherent."},
	{"HTTP-protocol knobs require `http`", "`CERBERUS_CH_PROTOCOL` vs the `_HTTP_*` protocol knobs", "Under `native` they would be silently dropped."},
	{"Compression level requires a method", "`CERBERUS_CH_COMPRESSION` vs `CERBERUS_CH_COMPRESSION_LEVEL`", "A level with `none` does nothing; a level must sit in the method's range (lz4 `0..12`, zstd `1..22`)."},
	{"Read timeout >= query timeout", "`CERBERUS_CH_READ_TIMEOUT` vs `CERBERUS_QUERY_TIMEOUT`", "A socket read shorter than the query budget would kill legitimate long queries."},
	{"Idle conns <= open conns", "`CERBERUS_CH_MAX_IDLE_CONNS` vs `CERBERUS_CH_MAX_OPEN_CONNS`", "More idle than total pooled connections is a degenerate pool. Fires only when idle is **explicitly set**."},
	{"Server header timeout <= read timeout", "`CERBERUS_HTTP_READ_HEADER_TIMEOUT` vs `CERBERUS_HTTP_READ_TIMEOUT`", "A header deadline longer than the whole-request deadline can never fire."},
}

// render assembles the full docs/configuration.md. The preamble + footer prose
// live in the template header/footer (kept hand-written and reviewed); the
// per-group tables are generated from config.EnvDocs + config.DocDefaults.
func render() (string, error) {
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

	sections := make([]section, 0, len(groups))
	for _, g := range groups {
		rows := rowsByGroup[g.Name]
		if len(rows) == 0 {
			return "", fmt.Errorf("group %q has no documented keys", g.Name)
		}
		sections = append(sections, section{
			Name:  g.Name,
			Intro: g.Intro,
			Table: renderTable([]string{"Variable", "Type", "Default", "Description"}, rows),
		})
	}

	matrix := renderTable(
		[]string{"Rule", "Knobs involved", "Why it fails fast"},
		dependencyMatrixRows,
	)

	var buf bytes.Buffer
	if err := docTemplate.Execute(&buf, templateData{
		Sections:         sections,
		DependencyMatrix: matrix,
	}); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	// Guarantee a single trailing newline (markdownlint MD047).
	return strings.TrimRight(buf.String(), "\n") + "\n", nil
}

// renderTable emits a markdownlint-MD060-compliant aligned table: every pipe
// lines up because each cell is padded to its column's rune width. All columns
// are left-aligned (the config reference has no numeric columns worth right-
// aligning). A literal `|` inside a cell (e.g. the `int | bool` admit type) is
// escaped to `\|` so it does not spuriously split the cell (MD056). Mirrors
// cmd/bench-report's writeTable.
func renderTable(header []string, rows [][]string) string {
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
