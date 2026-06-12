//go:build !chdb

// Command bench-report requires the `chdb` build tag (libchdb.so) to do
// any work — it measures cerberus's optimizer wins, scaling curves, and
// end-to-end query latency in-process against an embedded chDB engine.
// This stub keeps `go build ./...` green on the default CGO-free lane
// while making the requirement explicit at runtime.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"bench-report: built without the `chdb` tag — rebuild with "+
			"`go build -tags chdb ./cmd/bench-report` (requires libchdb.so; "+
			"see `just chdb-install`), or run `just bench-report`.")
	os.Exit(1)
}
