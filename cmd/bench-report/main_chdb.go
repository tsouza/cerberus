//go:build chdb

// Command bench-report measures cerberus's performance LIVE against an
// embedded chDB engine and writes a publishable, regenerable benchmark
// document (docs/benchmarks.md). It is the source for `just bench-report`.
//
// It covers five dimensions:
//
//  1. Headline before/after wins — each of cerberus's key optimizations,
//     measured by constructing both the NAIVE pre-fix SQL and the
//     OPTIMIZED shape the real lowering emits, running both on chDB over a
//     seeded dataset, and reporting the speedup × (plus a deterministic
//     structural ratio where one exists).
//  2. Sharded-solver dimension — the OOM-class range query route A cannot
//     serve under the 1 GiB per-query cap, that route B (the sharded
//     solver, internal/solver) clears: the measured expanded (sample,
//     anchor) pair count drives ~1/K modeled memory per shard.
//  3. Scaling curves — wall + fan_factor vs the real fan-out multiplier
//     for the registered scaling constructs, showing wall stays
//     sub-linear and intermediate cardinality stays bounded.
//  4. Micro-benchmarks — ns/op + allocs/op for the Go per-stage
//     benchmarks across the pipeline.
//  5. End-to-end query latency — representative queries lowered + emitted
//     through the real pipeline and executed on a LARGE synthetic dataset
//     (millions of rows, generated server-side via numbers(N)).
//
// # Determinism
//
// Structural metrics (fan_factor, peak intermediate cardinality, granules
// selected, allocs/op) are DETERMINISTIC and committed verbatim. Timing
// (ns/op, wall, e2e latency) is machine-dependent: it is presented as
// SPEEDUP RATIOS wherever a before/after exists (stable), and absolute
// timings are clearly labelled "indicative, measured on reference
// hardware". The document is a MANUALLY-regenerated artifact, NOT a CI
// gate — there is no test that fails on timing drift (the cardinality
// ratchet at test/perf/cardinality_ratchet_test.go already guards
// structural regressions).
//
// Built with the `chdb` tag (requires libchdb.so — `just chdb-install`).
// Without the tag, the stub in main_nochdb.go prints an instruction and
// exits non-zero.
//
// Usage:
//
//	bench-report -out docs/benchmarks.md [-iters 9] [-benchtime 10x]
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"
)

func main() {
	out := flag.String("out", "docs/benchmarks.md", "path to write the generated benchmark document")
	iters := flag.Int("iters", 9, "best-of-N repetition count for each chDB wall timing")
	benchtime := flag.String("benchtime", "10x", "go test -benchtime for the micro-benchmark lane")
	goBin := flag.String("go", "go", "go binary to invoke for the micro-benchmark lane")
	flag.Parse()

	if err := run(*out, *iters, *benchtime, *goBin); err != nil {
		fmt.Fprintf(os.Stderr, "bench-report: %v\n", err)
		os.Exit(1)
	}
}

func run(outPath string, iters int, benchtime, goBin string) error {
	started := time.Now()
	fmt.Fprintln(os.Stderr, "bench-report: opening chDB session…")
	s, err := newSession()
	if err != nil {
		return err
	}
	defer s.close()

	fmt.Fprintln(os.Stderr, "bench-report: measuring headline wins…")
	wins, err := measureHeadlines(s, iters)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "bench-report: measuring scaling curves…")
	curves, err := measureScalingCurves(s, iters)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "bench-report: measuring sharded-solver dimension (OOM-class)…")
	sharded, err := measureSharded(s, iters)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "bench-report: seeding + measuring end-to-end latency (large dataset)…")
	e2e, metricRows, logRows, traceRows, err := measureE2E(s, iters)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "bench-report: measuring execution-route × native-rate matrix…")
	matrix, err := measureMatrix(s, iters)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "bench-report: running Go micro-benchmarks…")
	micro, err := runMicroBenchmarks(goBin, benchtime)
	if err != nil {
		return err
	}

	doc, err := renderDoc(docInput{
		wins:       wins,
		curves:     curves,
		sharded:    sharded,
		e2e:        e2e,
		matrix:     matrix,
		micro:      micro,
		iters:      iters,
		benchtime:  benchtime,
		metricRows: metricRows,
		logRows:    logRows,
		traceRows:  traceRows,
		goVersion:  runtime.Version(),
		goarch:     runtime.GOARCH,
		numCPU:     runtime.NumCPU(),
		outPath:    outPath,
	})
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}

	if err := os.WriteFile(outPath, []byte(doc), 0o644); err != nil { //nolint:gosec // doc artifact
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Fprintf(os.Stderr, "bench-report: wrote %s (%d wins, sharded K=%d, %d curves, %d e2e, %d matrix cells, %d micro) in %s\n",
		outPath, len(wins), sharded.K, len(curves), len(e2e), len(matrix.Cells), len(micro), time.Since(started).Round(time.Second))
	return nil
}

// --- ratio helpers (shared) ---------------------------------------------

func ratioDur(a, b time.Duration) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func ratioI(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}
