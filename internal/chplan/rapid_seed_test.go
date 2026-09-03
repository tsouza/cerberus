package chplan_test

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
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

// pinChildEnv marks the re-executed copy of this test binary that reports what
// the init above did to the flag.
const pinChildEnv = "CERBERUS_RAPID_SEED_PIN_CHILD"

// TestRapidSeedPinAppliesTheEnvironmentSeed proves the init is load-bearing.
//
// It cannot be asserted in-process: init has already run by the time any test
// body executes, with whatever environment the whole binary was started in. So
// the test re-executes THIS binary — the same one, so rapid is linked and the
// flag exists — once per case and reads back the seed the child ended up with.
//
// The negative control is the point. Without it, a test that passes because
// rapid's default happens to equal the value under test would look identical to
// one that passes because the pin worked.
func TestRapidSeedPinAppliesTheEnvironmentSeed(t *testing.T) {
	if os.Getenv(pinChildEnv) == "1" {
		fmt.Printf("PINNED-SEED=%s\n", flag.Lookup("rapid.seed").Value.String())
		return
	}
	t.Parallel()

	for _, tc := range []struct {
		name     string
		seed     string
		wantSeed string
		wantExit int
	}{
		{name: "pinned", seed: "4242", wantSeed: "4242"},
		{name: "pinned to a different value", seed: "17", wantSeed: "17"},
		// rapid's own default: 0, which it reads as "draw a random one".
		{name: "unset", seed: "", wantSeed: "0"},
		{name: "not a seed", seed: "twelve", wantExit: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.Command(os.Args[0], "-test.run=^TestRapidSeedPinAppliesTheEnvironmentSeed$")
			env := []string{pinChildEnv + "=1"}
			for _, kv := range os.Environ() {
				// The coverage lanes export this to the whole sweep, so the
				// "unset" case has to strip it rather than merely not add it.
				if !strings.HasPrefix(kv, rapidSeedEnv+"=") && !strings.HasPrefix(kv, pinChildEnv+"=") {
					env = append(env, kv)
				}
			}
			if tc.seed != "" {
				env = append(env, rapidSeedEnv+"="+tc.seed)
			}
			cmd.Env = env

			out, err := cmd.CombinedOutput()
			exit := 0
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exit = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("re-executing the test binary: %v\n%s", err, out)
			}
			if exit != tc.wantExit {
				t.Fatalf("child exited %d, want %d\n%s", exit, tc.wantExit, out)
			}
			if tc.wantExit != 0 {
				if !strings.Contains(string(out), rapidSeedEnv) {
					t.Fatalf("a rejected seed must name the variable it came from; got:\n%s", out)
				}
				return
			}
			if want := "PINNED-SEED=" + tc.wantSeed + "\n"; !strings.Contains(string(out), want) {
				t.Fatalf("child reported the wrong seed, want %q in:\n%s", want, out)
			}
		})
	}
}
