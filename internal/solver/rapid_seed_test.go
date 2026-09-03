package solver

import (
	"flag"
	"fmt"
	"os"
)

// rapidSeedEnv names the PRNG seed the coverage lanes pin for
// pgregory.net/rapid. Only those lanes set it: `just property`, property.yml
// and a bare `go test` leave it unset and keep drawing a fresh random sample,
// which is where new counterexamples are actually found.
//
// The coverage ledger is a ratchet — it raises floors and never lowers one — so
// it cannot be fed a measurement that moves. rapid's seed defaults to random,
// and which branches of a generator-driven test execute moves the profile by
// whole statements between runs on an identical tree, so a floor enrolled from
// a lucky draw is never corrected by the unlucky one that follows.
//
// The pin is per test binary because `-rapid.seed` is registered by rapid's
// own init, so only a binary that links rapid carries the flag at all: a
// lane-wide `go test -rapid.seed=N ./...` would abort every package that does
// not. test/regression/rapid_seed_pin_test.go derives the set of packages that
// owe this pin from the import graph, so a new rapid-driven test cannot quietly
// rejoin the unpinned set. See docs/toolchain.md.
const rapidSeedEnv = "CERBERUS_RAPID_SEED"

// init runs before flag.Parse, so an explicit `-rapid.seed` on the command line
// still wins: parsing assigns only the flags actually present in the arguments.
func init() {
	seed, ok := os.LookupEnv(rapidSeedEnv)
	if !ok || seed == "" {
		return
	}
	f := flag.Lookup("rapid.seed")
	if f == nil {
		// This binary does not link rapid under the tags it was built with.
		// The coverage lanes export the variable to every package in the
		// sweep, so that is the ordinary case, not an error.
		return
	}
	if err := f.Value.Set(seed); err != nil {
		fmt.Fprintf(os.Stderr, "%s=%q is not a seed rapid accepts: %v\n", rapidSeedEnv, seed, err)
		os.Exit(1)
	}
}
