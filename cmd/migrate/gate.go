package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"

	"github.com/tsouza/cerberus/internal/migrategate"
)

// gateExitFail is the process exit code when the cutover gate returns FAIL (a
// blocking stage said no-go). It is distinct from the code Go uses for an
// internal tool error, and from verify's own gate code, so a no-go reads as
// "the gate did its job", not "the tool broke".
const gateExitFail = 3

// runGate folds the JSON artifacts the other migration blocks emit — verify,
// classify, inventory, rulegraph — into a single cutover go/no-go decision. It
// is a pure, offline aggregator: it reads the supplied artifact files, applies
// the conservative gate rules, prints a per-stage checklist (text or --json),
// and returns a gateFailedError (mapped to a non-zero exit) on any blocking
// stage. Every artifact flag is optional, but a MISSING required artifact
// blocks: the gate cannot prove safety for a stage it never saw.
func runGate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate gate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		verify    = fs.String("verify", "", "verify.json produced by `migrate verify --json`")
		classify  = fs.String("classify", "", "classify.json produced by `migrate classify --json`")
		inventory = fs.String("inventory", "", "inventory.json produced by `migrate inventory --json`")
		rulegraph = fs.String("rulegraph", "", "rulegraph.json produced by `migrate rulegraph --json`")
		out       = fs.String("out", "", "write the decision here (default: stdout)")
		asJSON    = fs.Bool("json", false, "emit the decision as JSON instead of text")
		highCard  = fs.Int64("high-card-series", migrategate.DefaultHighCardSeries,
			"WARN when a metric's head series count reaches this threshold")
		highCardLabels = fs.Int64("high-card-label-values", migrategate.DefaultHighCardLabelValues,
			"WARN when a label's distinct-value count reaches this threshold")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	in := migrategate.Inputs{
		Verify:    *verify,
		Classify:  *classify,
		Inventory: *inventory,
		RuleGraph: *rulegraph,
	}
	opts := migrategate.Options{
		HighCardSeries:      *highCard,
		HighCardLabelValues: *highCardLabels,
	}
	dec, err := migrategate.Evaluate(in, opts)
	if err != nil {
		return err
	}

	if err := writeGate(stdout, *out, dec, *asJSON); err != nil {
		return err
	}
	if !dec.Pass {
		return gateFailedError{overall: dec.Overall}
	}
	return nil
}

// writeGate renders the decision (text or JSON) to --out, or stdout when out is
// empty. When writing to a file it renders into a buffer first, then commits
// with the checked writeOut, so a flush-at-close error surfaces rather than
// truncating the decision silently.
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

// gateFailedError signals a no-go cutover decision: the gate ran and the
// checklist was written, but a blocking stage failed. main maps it to a
// dedicated non-zero exit code so a no-go is distinguishable from a tool error.
type gateFailedError struct {
	overall string
}

func (e gateFailedError) Error() string {
	return fmt.Sprintf("cutover gate failed: overall %s (a blocking stage said no-go)", e.overall)
}
