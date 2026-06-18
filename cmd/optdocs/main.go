// Command optdocs regenerates the structurally-derivable feature table in
// docs/clickhouse-optimizations.md from the single source of truth,
// chopt.Registry(). It renders ONLY the columns that can be derived from a
// registry entry without human judgement -- id, minVersion, stability -- into
// a marked-off block:
//
//	<!-- BEGIN GENERATED: chopt-feature-table (do not edit; ...) -->
//	... generated table ...
//	<!-- END GENERATED: chopt-feature-table -->
//
// The rich, hand-authored columns (experimental setting, effect prose) and the
// per-feature Notes live OUTSIDE the markers and are never touched here. Adding
// a feature to internal/chopt/registry.go therefore lands in the doc the next
// time `just gen-opt-docs` runs; the CI drift gate (ci.yml) fails any PR whose
// generated block is stale, so a registry feature can never go missing from the
// table (the ts_grid_changes / ts_grid_resets audit gap, made structurally
// impossible).
//
// Run via `just gen-opt-docs` or the go:generate directive in registry.go.
// `-check` regenerates into memory and exits non-zero on drift without writing
// (handy for a local pre-commit; CI uses `git diff --exit-code`).
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"text/template"

	"github.com/tsouza/cerberus/internal/chopt"
)

const (
	beginMarker = "<!-- BEGIN GENERATED: chopt-feature-table (do not edit; regenerate with `just gen-opt-docs`) -->"
	endMarker   = "<!-- END GENERATED: chopt-feature-table -->"
)

// tableBody is the structural feature table template. Columns are padded to a
// fixed width so the emitted markdown is MD060-aligned (the repo's
// .markdownlint.yaml pins table-column-style: aligned); widthsFor computes the
// padding from the actual rows so the block stays lint-clean as ids grow.
const tableBody = "{{.Begin}}\n" +
	"| {{pad \"id\" .W.ID}} | {{pad \"minVersion\" .W.MinVersion}} | {{pad \"stability\" .W.Stability}} |\n" +
	"| {{dash .W.ID}} | {{dash .W.MinVersion}} | {{dash .W.Stability}} |\n" +
	"{{range .Rows}}| {{pad .ID $.W.ID}} | {{pad .MinVersion $.W.MinVersion}} | {{pad .Stability $.W.Stability}} |\n{{end}}" +
	"{{.End}}"

// row is the rendered, structurally-derived view of a registry Feature: only
// the three columns a Feature determines on its own, already stringified by the
// render rules below.
type row struct {
	ID         string
	MinVersion string
	Stability  string
}

// widths holds the per-column render width (header included).
type widths struct {
	ID         int
	MinVersion int
	Stability  int
}

func main() {
	docPath := flag.String("doc", "docs/clickhouse-optimizations.md", "path to the optimizations doc")
	check := flag.Bool("check", false, "exit non-zero on drift without writing")
	flag.Parse()

	if err := run(*docPath, *check); err != nil {
		fmt.Fprintln(os.Stderr, "optdocs:", err)
		os.Exit(1)
	}
}

func run(docPath string, check bool) error {
	original, err := os.ReadFile(docPath) //nolint:gosec // doc path is a fixed flag default, not attacker-controlled
	if err != nil {
		return fmt.Errorf("read %s: %w", docPath, err)
	}

	block, err := renderBlock()
	if err != nil {
		return err
	}

	updated, err := replaceBlock(original, block)
	if err != nil {
		return err
	}

	if bytes.Equal(original, updated) {
		return nil
	}
	if check {
		return fmt.Errorf("%s is stale: run `just gen-opt-docs` and commit the result", docPath)
	}
	if err := os.WriteFile(docPath, updated, 0o644); err != nil { //nolint:gosec // doc file is world-readable source, 0644 is intentional
		return fmt.Errorf("write %s: %w", docPath, err)
	}
	return nil
}

// renderBlock builds the marker-delimited generated table from chopt.Registry()
// using only structurally-derivable columns.
func renderBlock() (string, error) {
	features := chopt.Registry()
	rows := make([]row, 0, len(features))
	for _, f := range features {
		rows = append(rows, row{
			ID:         "`" + f.ID + "`",
			MinVersion: renderMinVersion(f.MinVersion),
			Stability:  renderStability(f.Stability),
		})
	}

	w := widthsFor(rows)
	funcs := template.FuncMap{
		"pad":  func(s string, n int) string { return s + spaces(n-len(s)) },
		"dash": func(n int) string { return dashes(n) },
	}

	tmpl, err := template.New("table").Funcs(funcs).Parse(tableBody)
	if err != nil {
		return "", fmt.Errorf("parse table template: %w", err)
	}

	var buf bytes.Buffer
	data := struct {
		Begin string
		End   string
		W     widths
		Rows  []row
	}{Begin: beginMarker, End: endMarker, W: w, Rows: rows}

	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render table: %w", err)
	}
	return buf.String(), nil
}

// renderMinVersion stringifies a feature's version floor. The AlwaysAvailable
// zero floor (Version{0,0}) is special-cased to "none" -- a literal "0.0" would
// read as a real version requirement; "none" matches the doc's existing
// hand-authored wording for columnar_result_decode.
func renderMinVersion(v chopt.Version) string {
	if v == chopt.AlwaysAvailable {
		return "none"
	}
	return v.String()
}

// renderStability maps the Stability enum onto the doc's display vocabulary.
// Stable -> "stable". Experimental -> "experimental": the doc historically
// shows the opt-in/perf-tradeoff columnar_result_decode as "opt-in", but that
// is a hand-authored nuance over the same underlying Experimental class; the
// generated structural column reports the registry's actual class ("stable" /
// "experimental"), and the opt-in framing stays in the hand-authored Notes.
func renderStability(s chopt.Stability) string {
	if s == chopt.Stable {
		return "stable"
	}
	return "experimental"
}

// replaceBlock swaps the content between the BEGIN/END markers for block. It is
// an error for the markers to be missing or out of order: the generator owns an
// existing block, it does not invent the surrounding section.
func replaceBlock(doc []byte, block string) ([]byte, error) {
	begin := bytes.Index(doc, []byte(beginMarker))
	if begin < 0 {
		return nil, fmt.Errorf("BEGIN marker %q not found in doc", beginMarker)
	}
	end := bytes.Index(doc, []byte(endMarker))
	if end < 0 {
		return nil, fmt.Errorf("END marker %q not found in doc", endMarker)
	}
	if end < begin {
		return nil, fmt.Errorf("END marker precedes BEGIN marker")
	}
	endStop := end + len(endMarker)

	var out bytes.Buffer
	out.Write(doc[:begin])
	out.WriteString(block)
	out.Write(doc[endStop:])
	return out.Bytes(), nil
}

// widthsFor computes per-column render widths as the max of the header label
// and every cell, so columns align under MD060.
func widthsFor(rows []row) widths {
	w := widths{ID: len("id"), MinVersion: len("minVersion"), Stability: len("stability")}
	for _, r := range rows {
		if n := len(r.ID); n > w.ID {
			w.ID = n
		}
		if n := len(r.MinVersion); n > w.MinVersion {
			w.MinVersion = n
		}
		if n := len(r.Stability); n > w.Stability {
			w.Stability = n
		}
	}
	return w
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	return string(bytes.Repeat([]byte{' '}, n))
}

func dashes(n int) string {
	if n <= 0 {
		n = 1
	}
	return string(bytes.Repeat([]byte{'-'}, n))
}
