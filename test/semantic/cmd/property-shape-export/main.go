// Command property-shape-export prints the exact, current property-test
// shape rosters (test/property/gen, see cerberus issue #3426's semantic
// contract model and issue #3428's evidence resolvers) as JSON on stdout.
//
// WHY THIS EXISTS. A property-test ShapeID roster (gen.PromQLShapeIDs() and
// its five siblings) is a Go compile-time value with no serialized form: it
// is built from unexported const/var declarations in test/property/gen and
// exposed only through exported functions. Every other identity system the
// semantic evidence adapter resolves against — TXTAR fixtures, the
// surface-parity inventory, the rejection-parity catalogue, the
// test/oracle/inventory feature tables — already has a form the Node-side
// adapter (.github/scripts/lib/semantic-evidence-adapter.mjs) can read
// directly off disk. This is the one case invariant 9 and issue #3428's own
// scope call out as the exception: "write a small Go export helper ONLY
// where the existing Go representation genuinely requires one to be read
// from Node". This binary is that helper, and nothing else — it calls the
// SAME exported roster functions test/property/*_test.go calls, so the
// adapter reads the real current source truth rather than a hand-copied
// second roster that could silently drift from it.
//
// It is read-only (it only calls the exported gen.*ShapeIDs functions and
// prints their result; it writes nothing to disk, checked-in or otherwise)
// and deterministic (a roster function returns a fixed, ordered slice built
// from package-level const/var declarations — no randomness, no clock, no
// filesystem or environment input). It carries no build tag, so a plain `go
// run ./test/semantic/cmd/property-shape-export` from the repository root
// always executes it for real rather than silently compiling an empty
// binary under an unset tag.
//
// Output shape:
//
//	{
//	  "schema_version": 1,
//	  "rosters": {
//	    "gen.PromQLShapeIDs": ["promql.instant.selector", ...],
//	    "gen.PromQLRangeShapeIDs": [...],
//	    "gen.ExpHistogramShapeIDs": [...],
//	    "gen.LogQLShapeIDs": [...],
//	    "gen.TraceQLShapeIDs": [...],
//	    "gen.InstantWindowShapeIDs": [...]
//	  }
//	}
//
// The map keys are exactly the `RosterCall` values
// test/regression/property_live_roster_floor_test.go pins for its own six
// live-roster bindings, so a caller can cross-reference the two without
// inventing a second naming scheme.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/tsouza/cerberus/test/property/gen"
)

// schemaVersion is bumped whenever the output shape below changes in a way
// a Node-side reader must branch on.
const schemaVersion = 1

// export is the top-level JSON document this command prints.
type export struct {
	SchemaVersion int                 `json:"schema_version"`
	Rosters       map[string][]string `json:"rosters"`
}

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "property-shape-export:", err)
		os.Exit(1)
	}
}

func run(out *os.File) error {
	rosters := map[string][]string{
		"gen.PromQLShapeIDs":        shapeIDStrings(gen.PromQLShapeIDs()),
		"gen.PromQLRangeShapeIDs":   shapeIDStrings(gen.PromQLRangeShapeIDs()),
		"gen.ExpHistogramShapeIDs":  shapeIDStrings(gen.ExpHistogramShapeIDs()),
		"gen.LogQLShapeIDs":         shapeIDStrings(gen.LogQLShapeIDs()),
		"gen.TraceQLShapeIDs":       shapeIDStrings(gen.TraceQLShapeIDs()),
		"gen.InstantWindowShapeIDs": shapeIDStrings(gen.InstantWindowShapeIDs()),
	}

	doc := export{SchemaVersion: schemaVersion, Rosters: rosters}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func shapeIDStrings(ids []gen.ShapeID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}
