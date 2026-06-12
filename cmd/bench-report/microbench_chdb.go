//go:build chdb

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// microResult is one Go micro-benchmark's parsed metrics. allocs/op and
// B/op are deterministic; ns/op is timing (reported as indicative).
type microResult struct {
	Pkg      string // short package label, e.g. "promql"
	Name     string // benchmark name without the leading "Benchmark"
	NsPerOp  float64
	AllocsOp int64
	BytesOp  int64
}

// microPackages is the fixed set of per-stage benchmark packages, in
// pipeline order (parse/lower heads -> IR -> SQL -> driver -> API).
var microPackages = []struct{ short, path string }{
	{"promql", "./internal/promql/..."},
	{"logql", "./internal/logql/..."},
	{"traceql", "./internal/traceql/..."},
	{"chplan", "./internal/chplan/..."},
	{"optimizer", "./internal/optimizer/..."},
	{"chsql", "./internal/chsql/..."},
	{"chclient", "./internal/chclient/..."},
	{"api/prom", "./internal/api/prom/..."},
	{"api/format", "./internal/api/format/..."},
}

// runMicroBenchmarks invokes `go test -bench` for each package and parses
// the results. benchtime controls the sample count (e.g. "10x" for a
// fixed iteration count — keeps the run bounded and ns/op stable-ish).
func runMicroBenchmarks(goBin, benchtime string) ([]microResult, error) {
	var out []microResult
	for _, p := range microPackages {
		res, err := runBenchPackage(goBin, benchtime, p.short, p.path)
		if err != nil {
			return nil, fmt.Errorf("bench %s: %w", p.short, err)
		}
		out = append(out, res...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pkg != out[j].Pkg {
			return out[i].Pkg < out[j].Pkg
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func runBenchPackage(goBin, benchtime, short, path string) ([]microResult, error) {
	//nolint:gosec // fixed args, no user input — measurement harness
	cmd := exec.Command(goBin, "test", "-run", "^$", "-bench", ".",
		"-benchmem", "-benchtime", benchtime, "-tags", "chdb", path)
	stdout, err := cmd.Output()
	if err != nil {
		// A package with no benchmarks exits 0 with no Benchmark lines;
		// a real failure surfaces here. Include stderr for diagnosis.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("%w\n%s", err, ee.Stderr)
		}
		return nil, err
	}
	return parseBenchOutput(short, string(stdout)), nil
}

// parseBenchOutput extracts micro-results from `go test -benchmem` output.
// A benchmark line looks like:
//
//	BenchmarkLower/instant-8   	  123456	      9876 ns/op	    1024 B/op	      12 allocs/op
func parseBenchOutput(short, output string) []microResult {
	var out []microResult
	sc := bufio.NewScanner(strings.NewReader(output))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "Benchmark") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		r := microResult{Pkg: short, Name: benchName(fields[0])}
		for i := 0; i < len(fields)-1; i++ {
			switch fields[i+1] {
			case "ns/op":
				r.NsPerOp = atof(fields[i])
			case "allocs/op":
				r.AllocsOp = atoiTrunc(fields[i])
			case "B/op":
				r.BytesOp = atoiTrunc(fields[i])
			}
		}
		if r.NsPerOp > 0 || r.AllocsOp > 0 {
			out = append(out, r)
		}
	}
	return out
}

// benchName strips the leading "Benchmark" and the trailing "-N" CPU
// suffix Go appends.
func benchName(s string) string {
	s = strings.TrimPrefix(s, "Benchmark")
	if i := strings.LastIndex(s, "-"); i > 0 {
		if _, err := strconv.Atoi(s[i+1:]); err == nil {
			s = s[:i]
		}
	}
	return s
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func atoiTrunc(s string) int64 {
	f, _ := strconv.ParseFloat(s, 64)
	return int64(f)
}
