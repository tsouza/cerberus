// Command tempo-compat-driver is the dispatch entry point for the
// Tempo / TraceQL compatibility harness. It exists so the docker-compose
// stack has a single binary with a stable flag surface; behaviour is
// selected via the first positional argument (the "subcommand"):
//
//   - `seed` writes the same deterministic OTLP batch to the reference
//     Tempo (via OTLP gRPC :4317) and to ClickHouse's `otel_traces`
//     table (the read path cerberus uses to answer Tempo HTTP queries).
//     After both writes complete it polls `/api/traces/<first-id>` on
//     both backends and reports the per-backend span count.
//   - `diff` reads a TXTAR corpus, runs every TraceQL query through
//     both backends via /api/search, /api/traces/<id>,
//     /api/metrics/query_range, and /api/metrics/query, applies per-
//     side assertions + semantic-consistency invariants, computes the
//     structural diff, and emits a markdown report under /reports/
//     plus a shields.io endpoint-badge score JSON. Report-only: exit
//     code is 0 on parity drift; only driver-wide hard errors (corpus
//     load, report write) escalate to a non-zero rc.
//
// Cerberus is read-only over OTLP — its ingest is the OTel-CH exporter
// writing to CH. The harness therefore writes to Tempo via OTLP and to
// cerberus's CH directly, derived from one in-memory fixture so
// per-span fields stay 1:1 between the two read paths.
package main

import (
	"fmt"
	"os"
)

// version is stamped on driver output so a reader scanning CI logs can
// tell at a glance which PR's behaviour they're looking at.
const version = "pr5-metrics"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "tempo-compat-driver: missing subcommand")
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]
	switch sub {
	case "seed":
		if err := runSeed(args); err != nil {
			fmt.Fprintf(os.Stderr, "tempo-compat-driver seed: %v\n", err)
			os.Exit(1)
		}
	case "diff":
		if err := runDiff(args); err != nil {
			fmt.Fprintf(os.Stderr, "tempo-compat-driver diff: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "tempo-compat-driver: unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}
}

// usage prints the subcommand list. Kept terse: the harness is
// driver-internal infrastructure, not a user CLI.
func usage() {
	fmt.Fprintf(os.Stderr, `tempo-compat-driver %s

usage: tempo-compat-driver <subcommand> [flags]

subcommands:
  seed    push a deterministic OTLP batch to Tempo (:4317) AND insert
          equivalent rows into ClickHouse otel_traces; smoke-poll
          /api/traces/<id> on both backends.
  diff    run the TraceQL corpus through both backends, emit a
          markdown diff report under /reports/ and a shields.io
          endpoint-badge score JSON. Report-only: exit code is 0 on
          parity drift; only driver-wide hard errors (corpus load,
          report write) escalate to a non-zero rc.
`, version)
}
