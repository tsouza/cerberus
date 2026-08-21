//go:build !chdb

// Command perf-profile requires the `chdb` build tag (libchdb.so) to
// PROFILE fixtures — that work runs in-process against an embedded chDB
// engine. This build supports only `-merge` (folding N shard JSON
// outputs, produced elsewhere by the chdb-tagged binary, into one
// combined report — see report.go, which never touches chDB); any other
// invocation prints an instruction and exits non-zero.
//
// perf-profile.yml's `profile` aggregator job runs THIS build: it only
// merges the `profile-shard` matrix's per-leg JSON artifacts, so it pays
// for neither `just chdb-install` nor the `chdb` tag.
package main

import (
	"fmt"
	"os"
)

func main() {
	f := parseFlags()

	if f.mergeGlob != "" {
		os.Exit(runMerge(f))
	}

	fmt.Fprintln(os.Stderr,
		"perf-profile: built without the `chdb` tag — rebuild with "+
			"`go build -tags chdb ./cmd/perf-profile` (requires libchdb.so; "+
			"see `just chdb-install`) to profile fixtures, or pass -merge to "+
			"combine existing shard outputs (no chDB needed).")
	os.Exit(1)
}
