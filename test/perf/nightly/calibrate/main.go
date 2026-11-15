// Command calibrate combines the rounds files of a perf-nightly calibration's
// runners into the sentinels' committed bounds (nightly.CombineCalibrationFiles).
//
// Usage:
//
//	go run ./test/perf/nightly/calibrate -baseline <committed.json> -out <path> <rounds.json>...
//
// -out may name the committed file itself. It is written when every rounds
// file was read and every sentinel has a bound: a sentinel the derivation
// refused keeps its committed bound and is reported with an ::error:: line.
// It exits 0 when the file was written with no refusal, and 1 otherwise —
// whether -out was written then is told by whether it exists.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/tsouza/cerberus/test/perf/nightly"
)

func main() {
	baseline := flag.String("baseline", "", "the committed nightly-baseline.json")
	out := flag.String("out", "", "where the combined bound file is written")
	flag.Parse()
	if *baseline == "" || *out == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: calibrate -baseline <committed.json> -out <path> <rounds.json>...")
		os.Exit(1)
	}
	refusals, err := nightly.CombineCalibrationFiles(*baseline, *out, flag.Args())
	for _, refusal := range refusals {
		fmt.Printf("::error::%v — the sentinel keeps its committed bound\n", refusal)
	}
	if err != nil {
		fmt.Printf("::error::no bound file written: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("combined %d calibration rounds files into %s\n", flag.NArg(), *out)
	if len(refusals) > 0 {
		os.Exit(1)
	}
}
