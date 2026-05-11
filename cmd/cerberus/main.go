// Command cerberus is the three-headed query gateway server.
package main

import (
	"fmt"
	"os"
)

// Version is set at build time by goreleaser.
var Version = "dev"

func main() {
	fmt.Fprintf(os.Stdout, "cerberus %s — PromQL / LogQL / TraceQL → ClickHouse\n", Version)
	fmt.Fprintln(os.Stdout, "HTTP server wiring lands in seed PR7. See README.md.")
}
