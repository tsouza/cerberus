// Command tempo-compat-driver is a STUB for PR 2 of the Tempo / TraceQL
// compatibility harness rollout (docs/tempo-compliance-plan.md). It
// exists so the docker-compose stack has a well-typed flag surface to
// invoke from day one; PRs 3-4 replace the body with the real seeder +
// differ:
//
//   - PR 3: implement seeder.go (push identical OTLP batches to
//     reference Tempo's :4317 and cerberus's OTLP ingest, wait for
//     replication, smoke that /api/traces/<id> returns equal span
//     counts on both backends).
//   - PR 4: implement corpus.go + differ.go (load TXTAR corpus, fan
//     each query at both backends, write a markdown diff report).
//
// Until then this binary parses the same flags PRs 3-4 will use,
// prints a banner naming the stub, and exits 0. Keeping the flag
// surface stable means scripts/run-tempo-compatibility.sh and
// docker-compose.yml don't need to change when the real driver lands.
//
// Implementation deliberately uses stdlib only (no anthrosphere SDKs,
// no Tempo httpclient yet). PR 3 will pull in the vendored
// harness/tempo-compatibility/upstream/pkg/httpclient as the read
// client and the OTLP gRPC SDK for the write side.
package main

import (
	"flag"
	"fmt"
	"os"
)

// version is stamped on driver-stub output so a reader scanning CI
// logs can tell at a glance whether they're looking at the PR-2 stub
// or the real differ that lands in PR 4.
const version = "stub-pr2"

func main() {
	tempoURL := flag.String("tempo", "http://tempo:3200",
		"reference Tempo HTTP base URL (PR 3-4: read-back endpoint for diffs)")
	cerberusURL := flag.String("cerberus", "http://cerberus-tempo:29092",
		"cerberus-under-test HTTP base URL (PR 3-4: read-back endpoint for diffs)")
	corpusPath := flag.String("corpus", "/corpus/smoke.txtar",
		"TXTAR corpus path (PR 4: query corpus to fan at both backends)")
	reportPath := flag.String("report", "/reports/diff.json",
		"output report path (PR 4: per-query diff results in JSON)")

	flag.Parse()

	// Output is line-prefixed with the binary name so it's grep-able in
	// the compose log stream alongside Tempo / cerberus / ClickHouse.
	_, _ = fmt.Fprintf(os.Stdout, "tempo-compat-driver %s\n", version)
	_, _ = fmt.Fprintf(os.Stdout, "tempo-compat-driver  tempo    = %s\n", *tempoURL)
	_, _ = fmt.Fprintf(os.Stdout, "tempo-compat-driver  cerberus = %s\n", *cerberusURL)
	_, _ = fmt.Fprintf(os.Stdout, "tempo-compat-driver  corpus   = %s\n", *corpusPath)
	_, _ = fmt.Fprintf(os.Stdout, "tempo-compat-driver  report   = %s\n", *reportPath)
	_, _ = fmt.Fprintln(os.Stdout, "tempo-compat-driver stub: no seeder, no differ — PRs 3-4 will fill this in")
	_, _ = fmt.Fprintln(os.Stdout, "tempo-compat-driver exit 0")
}
