//go:build !chdb

// Command perf-profile requires the `chdb` build tag (libchdb.so) to do
// any work — it profiles fixtures in-process against an embedded chDB
// engine. This stub keeps `go build ./...` green on the default
// CGO-free lane while making the requirement explicit at runtime.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"perf-profile: built without the `chdb` tag — rebuild with "+
			"`go build -tags chdb ./cmd/perf-profile` (requires libchdb.so; "+
			"see `just chdb-install`).")
	os.Exit(1)
}
