// Command tempo-compat-driver is the dispatch entry point for the
// Tempo / TraceQL compatibility harness. It exists so the docker-compose
// stack has a single binary with a stable flag surface; behaviour is
// selected via the first positional argument (the "subcommand"):
//
//   - `seed`    (PR 3) writes the same deterministic OTLP batch to
//     the reference Tempo (via OTLP gRPC :4317) and to ClickHouse's
//     `otel_traces` table (the read path cerberus uses to answer Tempo
//     HTTP queries). After both writes complete it polls
//     `/api/traces/<first-id>` on both backends and reports the per-
//     backend span count. The contract is: stack comes up, seed
//     pushes to both targets, the smoke trace-id resolves on both.
//   - `diff`    (PR 4 — this PR) reads a TXTAR corpus, runs every
//     TraceQL query through both backends via /api/search (and
//     /api/traces/<id> for the per-id smoke cases), applies per-side
//     assertions, computes the structural diff, and emits a markdown
//     report under /reports/. Returns 0 (informational baseline) by
//     default; pass --fail-on-diff to make any diff non-zero.
//
// The two-way "OTLP into Tempo, INSERT into CH" split is documented in
// docs/tempo-compliance-plan.md's "Open question 1": cerberus is
// **read-only over OTLP**. The HTTP layer answers Prom / Loki / Tempo
// queries by reading from a CH instance whose tables are populated by
// the OTel-CH exporter in a real deployment. The compatibility harness
// can't run a full collector → exporter pipeline just to seed (that
// would re-test the exporter, not cerberus's read path), so it inserts
// directly into `otel_traces` with the same shape the exporter would
// produce. The Tempo side, by contrast, has no out-of-band ingest path
// and must take OTLP. Both writes are derived from one in-memory
// fixture so per-span fields stay 1:1 between the two read paths.
//
// Single binary, multiple subcommands keeps the docker-compose flag
// surface stable: the same image gets re-invoked across PRs by changing
// the `command:` array, not by swapping images.
package main

import (
	"fmt"
	"os"
)

// version is stamped on driver output so a reader scanning CI logs can
// tell at a glance which PR's behaviour they're looking at.
const version = "pr4-differ"

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
          markdown diff report under /reports/. Default exit code is
          0 (informational); pass --fail-on-diff to bubble per-case
          regressions up to a non-zero rc.
`, version)
}
