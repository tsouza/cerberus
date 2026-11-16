// Command calibrate combines the rounds files of a perf-nightly calibration's
// runners into the sentinels' committed bounds (nightly.CombineCalibrationFiles).
//
// Usage, from the repository root:
//
//	go run ./test/perf/nightly/calibrate [-baseline <committed.json>] [-out <path>] <rounds.json>...
//
// -baseline and -out both default to the committed bound file. -out is
// written when every rounds file was read and every sentinel has a bound: a
// sentinel the derivation refused keeps its committed bound and is reported
// with an ::error:: line. It exits exitCombined when the file was written with
// no refusal, and exitFailed otherwise — whether -out was written then is
// told by whether it exists.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tsouza/cerberus/test/perf/nightly"
)

// committedBaseline is the committed bound file, relative to the repository
// root.
const committedBaseline = "test/perf/nightly-baseline.json"

const (
	exitCombined = 0
	exitFailed   = 1
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("calibrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	baseline := flags.String("baseline", committedBaseline, "the committed bound file the combine starts from")
	out := flags.String("out", committedBaseline, "where the combined bound file is written")
	if err := flags.Parse(args); err != nil {
		return exitFailed
	}
	if flags.NArg() == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: calibrate [-baseline <committed.json>] [-out <path>] <rounds.json>...")
		return exitFailed
	}
	refusals, err := nightly.CombineCalibrationFiles(*baseline, *out, flags.Args())
	for _, refusal := range refusals {
		_, _ = fmt.Fprintf(stdout, "::error::%v — the sentinel keeps its committed bound\n", refusal)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "::error::no bound file written: %v\n", err)
		return exitFailed
	}
	_, _ = fmt.Fprintf(stdout, "combined %d calibration rounds files into %s\n", flags.NArg(), *out)
	if len(refusals) > 0 {
		return exitFailed
	}
	return exitCombined
}
