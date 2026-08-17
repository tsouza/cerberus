package migrategate

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/tsouza/cerberus/internal/migrate"
)

// Write renders the decision as a scannable go/no-go checklist: one line per
// stage with its verdict and whether it blocks, each stage's reasons indented
// beneath it, then the overall PASS/FAIL verdict last.
func (d Decision) Write(w io.Writer) error {
	bw := migrate.NewErrWriter(w)
	bw.Printf("# cerberus migrate gate\n")
	bw.Printf("#\n")
	bw.Printf("# Folds the migration artifacts into one cutover go/no-go decision.\n")
	bw.Printf("# A blocking stage (FAIL, or a missing REQUIRED artifact) fails the gate;\n")
	bw.Printf("# a WARN is surfaced but does not block. Exit 0 only on overall PASS.\n")
	bw.Printf("#\n")

	for _, s := range d.Stages {
		if s.Blocking {
			bw.Printf("  %-9s %-7s BLOCKING\n", s.Stage, s.Verdict)
		} else {
			bw.Printf("  %-9s %s\n", s.Stage, s.Verdict)
		}
		for _, r := range s.Reasons {
			bw.Printf("      %s\n", r)
		}
	}

	if len(d.Missing) > 0 {
		bw.Printf("\n# missing artifacts: %v\n", d.Missing)
	}
	bw.Printf("\nOVERALL: %s\n", d.Overall)
	return bw.Err
}

// WriteJSON renders the decision as deterministic, indented JSON with a
// trailing newline. Nil slices become empty slices so the decision always
// carries `[]` rather than `null`, matching the other blocks' JSON convention.
func (d Decision) WriteJSON(w io.Writer) error {
	if d.Stages == nil {
		d.Stages = []StageResult{}
	}
	if d.Missing == nil {
		d.Missing = []string{}
	}
	for i := range d.Stages {
		if d.Stages[i].Reasons == nil {
			d.Stages[i].Reasons = []string{}
		}
	}
	data, err := json.MarshalIndent(d, "", jsonIndent)
	if err != nil {
		return fmt.Errorf("migrategate: marshal decision: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("migrategate: write decision: %w", err)
	}
	return nil
}
