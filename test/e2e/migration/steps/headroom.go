package steps

import (
	"fmt"
	"io"
	"os"

	"github.com/tsouza/cerberus/test/e2e/migration/tolerances"
)

// headroomReport is one declared band measured against one live run: the
// observed delta in the band's own unit and the band it was judged by.
// docs/migration-testing.md section 6.2 has every run print this for each
// band, so the registry's shrink-only ratchet has evidence to tighten on
// rather than ossifying at whatever number was first written down.
type headroomReport struct {
	// Story is the MIG-nn the band belongs to; Archetype the fixture it ran
	// against; Subject names the quantity Observed measures.
	Story, Archetype, Subject string
	// Observed is the delta this run measured; Band is the declared band.
	Observed float64
	Band     tolerances.Band
}

// Headroom is observed / declared: the share of the band this run used. A
// value above 1 is a band exceeded; a value far below 1 is a band the
// evidence says can shrink.
func (r headroomReport) Headroom() float64 {
	return r.Observed / r.Band.Value
}

// Line renders the one-line report the lane prints.
func (r headroomReport) Line() string {
	return fmt.Sprintf(
		"%s %s headroom: %s observed %v / declared %v = %.3f of the band",
		r.Story, r.Archetype, r.Subject, r.Observed, r.Band.Value, r.Headroom(),
	)
}

// printHeadroom writes the report to the suite's output (godog writes to
// os.Stdout, so it lands in the CI log of the run that measured it — the
// same sink MIG-08's latency report uses).
func printHeadroom(r headroomReport) error {
	return writeHeadroom(os.Stdout, r)
}

// writeHeadroom is printHeadroom with the sink injected, so the rendering
// is testable without capturing the process's stdout.
func writeHeadroom(w io.Writer, r headroomReport) error {
	if _, err := fmt.Fprintln(w, r.Line()); err != nil {
		return fmt.Errorf("migration harness: print the %s headroom report: %w", r.Story, err)
	}
	return nil
}
