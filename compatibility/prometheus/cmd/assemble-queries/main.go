// Command assemble-queries materializes the fragmented PromQL compatibility
// corpus as the one YAML document expected by promql-compliance-tester.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tsouza/cerberus/compatibility/prometheus/querycorpus"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("assemble-queries", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", "compatibility/prometheus/query-corpus", "query corpus directory")
	output := flags.String("output", "", "assembled YAML output path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *output == "" {
		fmt.Fprintln(stderr, "-output is required")
		return 2
	}

	raw, _, err := querycorpus.Load(*source)
	if err != nil {
		fmt.Fprintf(stderr, "assemble PromQL compatibility corpus: %v\n", err)
		return 1
	}
	if err := os.WriteFile(*output, raw, 0o600); err != nil {
		fmt.Fprintf(stderr, "write assembled PromQL compatibility corpus: %v\n", err)
		return 1
	}
	return 0
}
