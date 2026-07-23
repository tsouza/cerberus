package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"

	"github.com/tsouza/cerberus/internal/migrateinventory"
)

// runInventory probes a LIVE source Prometheus for the runtime cardinality
// facts config can't reveal offline — the head-block series/label cardinality
// that drives OOM risk — ranks the top-N candidates, and writes the report.
// It deliberately refuses to infer from prometheus.yml: the only honest source
// of realized cardinality is the running TSDB. Flags fall back to
// CERBERUS_INVENTORY_* environment variables. A source that 404s the mandatory
// /api/v1/status/tsdb endpoint is a hard error mapped to a non-zero exit.
func runInventory(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate inventory", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		source = fs.String("source", envOr("CERBERUS_INVENTORY_SOURCE", ""),
			"source Prometheus base URL to probe for live cardinality")
		top = fs.Int("top", migrateinventory.DefaultTop,
			"rank the top N metrics/labels by cardinality")
		window = fs.String("window", envOr("CERBERUS_INVENTORY_WINDOW", ""),
			"optional observation window (duration like 1h) recorded as report context")
		asJSON = fs.Bool("json", false, "emit the machine-readable JSON report instead of text")
		out    = fs.String("out", "", "write the inventory here (default: stdout)")
	)
	if handled, err := parseFlags(fs, args, stdout, stderr); err != nil || handled {
		return err
	}
	if *source == "" {
		return errors.New("missing --source (or CERBERUS_INVENTORY_SOURCE): the source Prometheus base URL to probe")
	}

	opts := migrateinventory.Options{Top: *top, Window: *window}
	if err := opts.Validate(); err != nil {
		return err
	}

	inv, err := migrateinventory.NewClient(*source).Probe(context.Background(), opts)
	if err != nil {
		return err
	}
	return writeInventory(stdout, *out, inv, *asJSON)
}

// writeInventory renders the inventory (text or JSON) to --out, or stdout when
// out is empty. When writing to a file it renders into a buffer first, then
// commits with the checked writeOut, so a flush-at-close error surfaces rather
// than truncating the report silently — the same file-output convention every
// other gate-input producer follows.
func writeInventory(stdout io.Writer, out string, inv migrateinventory.Inventory, asJSON bool) error {
	render := func(w io.Writer) error {
		if asJSON {
			return inv.WriteJSON(w)
		}
		return inv.WriteText(w)
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
