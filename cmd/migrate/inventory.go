package main

import (
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
	)
	if err := fs.Parse(args); err != nil {
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
	if *asJSON {
		return inv.WriteJSON(stdout)
	}
	return inv.WriteText(stdout)
}
