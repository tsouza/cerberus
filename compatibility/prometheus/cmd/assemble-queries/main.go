// Command assemble-queries materializes the fragmented PromQL compatibility
// corpus as the one YAML document expected by promql-compliance-tester.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/tsouza/cerberus/compatibility/prometheus/querycorpus"
)

func main() {
	source := flag.String("source", "compatibility/prometheus/query-corpus", "query corpus directory")
	output := flag.String("output", "", "assembled YAML output path")
	flag.Parse()
	if *output == "" {
		fmt.Fprintln(os.Stderr, "-output is required")
		os.Exit(2)
	}

	raw, _, err := querycorpus.Load(*source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "assemble PromQL compatibility corpus: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*output, raw, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write assembled PromQL compatibility corpus: %v\n", err)
		os.Exit(1)
	}
}
