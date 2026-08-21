//go:build chdb

// Command perf-profile is the corpus-wide perf profiler entrypoint
// (Component B of the perf-assessment framework). It walks every
// executable TXTAR fixture under test/spec/** and emits one structured
// fan-out profile per fixture.
//
// It is built with the `chdb` tag (requires libchdb.so — `just
// chdb-install`) because the profiling work runs in-process against an
// embedded chDB engine. Without the tag, the build falls back to the
// stub in main_nochdb.go, which still supports `-merge` (see below) but
// otherwise prints an instruction and exits non-zero.
//
// Usage:
//
//	perf-profile -spec test/spec -out profile.json [-top 25]
//	perf-profile -merge 'shards/*.json' -out profile.json [-top 25]
//
// Flags:
//
//	-spec        root directory of the TXTAR corpus (default "test/spec")
//	-out         path to write the JSON profile array (default stdout)
//	-md          path to append a markdown top-fan_factor table (for GITHUB_STEP_SUMMARY)
//	-top         print the top-N fan_factor fixtures to stderr as a table
//	             (default 25; 0 disables)
//	-fail-over   exit non-zero if any fixture's fan_factor exceeds this
//	             threshold (default 0 = never fail; nightly lane reports,
//	             it does not gate)
//	-merge          glob of shard JSON files to combine into one report
//	                INSTEAD of profiling (see report.go). Needs no chDB —
//	                works identically in the tag-free build.
//	-expect-shards  (-merge only) fail unless exactly this many files
//	                matched -merge (0 = don't check)
//
// # Sharding
//
// A single process profiles only the corpus slice named by the
// environment: PERF_SHARD_INDEX / PERF_SHARD_COUNT
// (test/perf/profile/shard.go). Both unset — the local default — is the
// WHOLE corpus, exactly as before sharding existed. perf-profile.yml's
// `profile-shard` matrix job sets both, one leg per slice, and the
// `profile` aggregator job then runs this same binary with `-merge` to
// fold the legs' JSON outputs back into one combined report — mirroring
// `perf-guards-shard` / `perf-guards` in chdb.yml, which shards this
// exact package's ProfileCorpusShard call for a different (pass/fail
// assertion) consumer.
//
// The JSON output is an array of profile.Record. The nightly
// perf-profile.yml lane uploads it as an artifact and renders the
// top-fan_factor table into the job step summary.
package main

import (
	"fmt"
	"os"

	"github.com/tsouza/cerberus/test/perf/profile"
)

func main() {
	f := parseFlags()

	if f.mergeGlob != "" {
		os.Exit(runMerge(f))
	}

	shard, err := profile.ShardFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf-profile: %v\n", err)
		os.Exit(1)
	}

	recs, err := profile.ProfileCorpusShard(f.specDir, shard)
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf-profile: %v\n", err)
		os.Exit(1)
	}

	os.Exit(emitReport(recs, f.outPath, f.mdPath, f.top, f.failOver))
}
