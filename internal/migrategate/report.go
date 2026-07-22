package migrategate

import (
	"encoding/json"
	"fmt"
	"io"
)

// Write renders the decision as a scannable go/no-go checklist: one line per
// stage with its verdict and whether it blocks, each stage's reasons indented
// beneath it, then the overall PASS/FAIL verdict last.
func (d Decision) Write(w io.Writer) error {
	bw := &errWriter{w: w}
	bw.printf("# cerberus migrate gate\n")
	bw.printf("#\n")
	bw.printf("# Folds the migration artifacts into one cutover go/no-go decision.\n")
	bw.printf("# A blocking stage (FAIL, or a missing REQUIRED artifact) fails the gate;\n")
	bw.printf("# a WARN is surfaced but does not block. Exit 0 only on overall PASS.\n")
	bw.printf("#\n")

	for _, s := range d.Stages {
		if s.Blocking {
			bw.printf("  %-9s %-7s BLOCKING\n", s.Stage, s.Verdict)
		} else {
			bw.printf("  %-9s %s\n", s.Stage, s.Verdict)
		}
		for _, r := range s.Reasons {
			bw.printf("      %s\n", r)
		}
	}

	if len(d.Missing) > 0 {
		bw.printf("\n# missing artifacts: %v\n", d.Missing)
	}
	bw.printf("\nOVERALL: %s\n", d.Overall)
	return bw.err
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

// errWriter collapses the repeated Fprintf error checks in Write into a single
// short-circuiting sink: once a write fails, later printf calls are no-ops and
// the first error is returned.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}
