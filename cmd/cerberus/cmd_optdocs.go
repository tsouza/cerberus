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

	"github.com/tsouza/cerberus/internal/chopt"
)

// newOptDocsCmd regenerates the structurally-derivable feature table in
// docs/clickhouse-optimizations.md from chopt.Registry(). It renders only the
// columns derived from a registry entry without human judgement (id, minVersion,
// stability, autoSelect) into the marked-off block; the rich hand-authored
// columns live outside the markers and are never touched. Flag parsing is
// delegated to the std flag package (DisableFlagParsing) so the historical
// single-dash `-doc` / `-check` invocation used by the `go:generate` directive
// in internal/chopt/registry.go and `just gen-opt-docs` keeps working.
func newOptDocsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "optdocs",
		Short: "Regenerate the chopt feature table in docs/clickhouse-optimizations.md",
		Long: "Regenerate the structural feature table (id / minVersion / stability /\n" +
			"autoSelect) in docs/clickhouse-optimizations.md from internal/chopt's\n" +
			"registry. With -check it regenerates into memory and exits non-zero on\n" +
			"drift without writing.\n\nUsage: cerberus optdocs [-doc docs/clickhouse-optimizations.md] [-check]",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return optdocsRun(args, cmd.ErrOrStderr())
		},
	}
}

// optdocsRun parses the -doc/-check flags and regenerates (or drift-checks) the
// optimizations doc, mirroring the legacy `optdocs` binary's error prefix.
func optdocsRun(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("optdocs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	docPath := fs.String("doc", "docs/clickhouse-optimizations.md", "path to the optimizations doc")
	check := fs.Bool("check", false, "exit non-zero on drift without writing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := optdocsGenerate(*docPath, *check); err != nil {
		return fmt.Errorf("optdocs: %w", err)
	}
	return nil
}

func optdocsGenerate(docPath string, check bool) error {
	original, err := os.ReadFile(docPath) //nolint:gosec // doc path is a fixed flag default, not attacker-controlled
	if err != nil {
		return fmt.Errorf("read %s: %w", docPath, err)
	}

	block, err := optdocRenderBlock()
	if err != nil {
		return err
	}

	updated, err := optdocReplaceBlock(original, block)
	if err != nil {
		return err
	}

	if bytes.Equal(original, updated) {
		return nil
	}
	if check {
		return fmt.Errorf("%s is stale: run `just gen-opt-docs` and commit the result", docPath)
	}
	if err := os.WriteFile(docPath, updated, 0o644); err != nil { //nolint:gosec // doc file is a world-readable source, 0644 intentional
		return fmt.Errorf("write %s: %w", docPath, err)
	}
	return nil
}

const (
	optdocBeginMarker = "<!-- BEGIN GENERATED: chopt-feature-table (do not edit; regenerate with `just gen-opt-docs`) -->"
	optdocEndMarker   = "<!-- END GENERATED: chopt-feature-table -->"
)

// optdocTableBody is the structural feature table template. Columns are padded to
// a fixed width so the emitted markdown is MD060-aligned; optdocWidthsFor computes
// the padding from the actual rows so the block stays lint-clean as ids grow.
const optdocTableBody = "{{.Begin}}\n" +
	"| {{pad \"id\" .W.ID}} | {{pad \"minVersion\" .W.MinVersion}} | {{pad \"stability\" .W.Stability}} | {{pad \"autoSelect\" .W.AutoSelect}} | {{pad \"experimental setting\" .W.ExperimentalSetting}} | {{pad \"doc\" .W.Doc}} |\n" +
	"| {{dash .W.ID}} | {{dash .W.MinVersion}} | {{dash .W.Stability}} | {{dash .W.AutoSelect}} | {{dash .W.ExperimentalSetting}} | {{dash .W.Doc}} |\n" +
	"{{range .Rows}}| {{pad .ID $.W.ID}} | {{pad .MinVersion $.W.MinVersion}} | {{pad .Stability $.W.Stability}} | {{pad .AutoSelect $.W.AutoSelect}} | {{pad .ExperimentalSetting $.W.ExperimentalSetting}} | {{pad .Doc $.W.Doc}} |\n{{end}}" +
	"{{.End}}"

// optdocExperimentalTSGridSetting is the ClickHouse server setting every
// RequiresExperimentalTSGrid feature needs the engine to co-stamp; rendered
// in the table so the set of features that need it is read from the
// registry flag, never counted by hand.
const optdocExperimentalTSGridSetting = "`allow_experimental_time_series_aggregate_functions`"

// optdocNoExperimentalSetting is the cell for a feature that needs no
// experimental server setting.
const optdocNoExperimentalSetting = "(none)"

// optdocRow is the rendered, structurally-derived view of a registry Feature:
// only the columns a Feature determines on its own, already stringified.
type optdocRow struct {
	ID                  string
	MinVersion          string
	Stability           string
	AutoSelect          string
	ExperimentalSetting string
	Doc                 string
}

// optdocWidths holds the per-column render width (header included).
type optdocWidths struct {
	ID                  int
	MinVersion          int
	Stability           int
	AutoSelect          int
	ExperimentalSetting int
	Doc                 int
}

// optdocRenderBlock builds the marker-delimited generated table from
// chopt.Registry() using only structurally-derivable columns.
func optdocRenderBlock() (string, error) {
	features := chopt.Registry()
	rows := make([]optdocRow, 0, len(features))
	for _, f := range features {
		rows = append(rows, optdocRow{
			ID:                  "`" + f.ID + "`",
			MinVersion:          optdocRenderMinVersion(f.MinVersion),
			Stability:           optdocRenderStability(f.Stability),
			AutoSelect:          optdocRenderAutoSelect(f.AutoSelect),
			ExperimentalSetting: optdocRenderExperimentalSetting(f.RequiresExperimentalTSGrid),
			Doc:                 optdocRenderDoc(f.Doc),
		})
	}

	w := optdocWidthsFor(rows)
	funcs := template.FuncMap{
		"pad":  func(s string, n int) string { return s + optdocSpaces(n-optdocWidth(s)) },
		"dash": func(n int) string { return optdocDashes(n) },
	}

	tmpl, err := template.New("table").Funcs(funcs).Parse(optdocTableBody)
	if err != nil {
		return "", fmt.Errorf("parse table template: %w", err)
	}

	var buf bytes.Buffer
	data := struct {
		Begin string
		End   string
		W     optdocWidths
		Rows  []optdocRow
	}{Begin: optdocBeginMarker, End: optdocEndMarker, W: w, Rows: rows}

	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render table: %w", err)
	}
	return buf.String(), nil
}

// optdocRenderMinVersion stringifies a feature's version floor. The
// AlwaysAvailable zero floor (Version{0,0}) is special-cased to "none" — a
// literal "0.0" would read as a real version requirement.
func optdocRenderMinVersion(v chopt.Version) string {
	if v == chopt.AlwaysAvailable {
		return "none"
	}
	return v.String()
}

// optdocRenderStability maps the Stability enum onto the doc's display vocabulary.
func optdocRenderStability(s chopt.Stability) string {
	if s == chopt.Stable {
		return "stable"
	}
	return "experimental"
}

// optdocRenderAutoSelect stringifies a feature's auto-eligibility.
func optdocRenderAutoSelect(auto bool) string {
	if auto {
		return "yes"
	}
	return "no"
}

// optdocRenderExperimentalSetting names the server setting a feature needs
// co-stamped, from the registry's RequiresExperimentalTSGrid flag.
func optdocRenderExperimentalSetting(requiresExperimentalTSGrid bool) string {
	if requiresExperimentalTSGrid {
		return optdocExperimentalTSGridSetting
	}
	return optdocNoExperimentalSetting
}

// optdocRenderDoc renders a feature's registry Doc as one table cell: a
// pipe would end the cell early, so it is escaped for the markdown table.
func optdocRenderDoc(doc string) string {
	return strings.ReplaceAll(doc, "|", "\\|")
}

// optdocReplaceBlock swaps the content between the BEGIN/END markers for block.
// It is an error for the markers to be missing or out of order: the generator
// owns an existing block, it does not invent the surrounding section.
func optdocReplaceBlock(doc []byte, block string) ([]byte, error) {
	begin := bytes.Index(doc, []byte(optdocBeginMarker))
	if begin < 0 {
		return nil, fmt.Errorf("BEGIN marker %q not found in doc", optdocBeginMarker)
	}
	end := bytes.Index(doc, []byte(optdocEndMarker))
	if end < 0 {
		return nil, fmt.Errorf("END marker %q not found in doc", optdocEndMarker)
	}
	if end < begin {
		return nil, fmt.Errorf("END marker precedes BEGIN marker")
	}
	endStop := end + len(optdocEndMarker)

	var out bytes.Buffer
	out.Write(doc[:begin])
	out.WriteString(block)
	out.Write(doc[endStop:])
	return out.Bytes(), nil
}

// optdocWidthsFor computes per-column render widths from the max of the header
// label and each cell, so columns align under MD060.
func optdocWidthsFor(rows []optdocRow) optdocWidths {
	w := optdocWidths{
		ID: len("id"), MinVersion: len("minVersion"), Stability: len("stability"), AutoSelect: len("autoSelect"),
		ExperimentalSetting: len("experimental setting"), Doc: len("doc"),
	}
	for _, r := range rows {
		if n := optdocWidth(r.ID); n > w.ID {
			w.ID = n
		}
		if n := optdocWidth(r.MinVersion); n > w.MinVersion {
			w.MinVersion = n
		}
		if n := optdocWidth(r.Stability); n > w.Stability {
			w.Stability = n
		}
		if n := optdocWidth(r.AutoSelect); n > w.AutoSelect {
			w.AutoSelect = n
		}
		if n := optdocWidth(r.ExperimentalSetting); n > w.ExperimentalSetting {
			w.ExperimentalSetting = n
		}
		if n := optdocWidth(r.Doc); n > w.Doc {
			w.Doc = n
		}
	}
	return w
}

// optdocWidth is a cell's width as markdownlint's MD060 measures it: in
// characters, not bytes. A doc cell carrying an em dash or an arrow is
// three bytes per character, and byte-based padding leaves its pipe short.
func optdocWidth(s string) int {
	return utf8.RuneCountInString(s)
}

func optdocSpaces(n int) string {
	if n <= 0 {
		return ""
	}
	return string(bytes.Repeat([]byte{' '}, n))
}

func optdocDashes(n int) string {
	if n <= 0 {
		n = 1
	}
	return string(bytes.Repeat([]byte{'-'}, n))
}
