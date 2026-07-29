package config

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
)

// durationDocType is the EnvDoc.Type every duration-valued knob carries. The
// class gate below drives itself from that metadata rather than a hand-kept
// list, so a duration knob added later is covered the moment it is documented.
const durationDocType = "duration"

// TestDurationKnobs_ShareOneGrammar is the class gate behind the rule that
// every CERBERUS_* duration knob answers to ONE grammar. It sets each knob in
// turn to two spellings of the same duration — one only Go duration syntax
// can express the other way round, one only the retention units can spell —
// and requires both to be accepted, then requires a value in neither grammar
// to be rejected with the knob's name in the error.
//
// A knob wired to a single-grammar parser fails here: `90d` is rejected by
// Go's time.ParseDuration and `1.5h` by the retention grammar, so a divergent
// knob cannot pass both halves. That is the property that keeps an operator
// from ever having to ask which spelling a particular setting takes.
func TestDurationKnobs_ShareOneGrammar(t *testing.T) {
	const (
		retentionSpelling = "90d"   // only the retention units spell this
		goSpelling        = "1.5h"  // only Go duration syntax spells this
		sharedSpelling    = "2160h" // both halves spell this, and 90d equals it
		notADuration      = "2 weeks"
	)

	covered := 0
	for _, d := range envDocs {
		if d.Type != durationDocType {
			continue
		}
		covered++
		t.Run(d.Key, func(t *testing.T) {
			for _, val := range []string{retentionSpelling, goSpelling, sharedSpelling} {
				t.Setenv(d.Key, val)
				if _, err := FromEnv(); err != nil {
					t.Errorf("FromEnv with %s=%s: %v", d.Key, val, err)
				}
			}
			t.Setenv(d.Key, notADuration)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %s=%s; want a fail-fast error", d.Key, notADuration)
			}
			if !strings.Contains(err.Error(), d.Key) {
				t.Errorf("error %q does not name %s", err, d.Key)
			}
		})
	}
	if covered == 0 {
		t.Fatalf("no EnvDoc carries Type %q — the class gate is inspecting nothing", durationDocType)
	}
}

// TestDurationKnobs_RejectNegative is the RANGE half of the class gate, and
// the reason the grammar half is safe to widen. Merging the two grammars means
// every duration knob now inherits Go duration syntax's sign, so `-1h` reaches
// the loader as a well-formed value on all of them — including the four that
// previously got their sign rejection for free from the retention regex. No
// knob on this surface has a meaning below zero (they are timeouts, intervals,
// lifetimes, retentions and lookbacks), so the invariant is flat: a negative
// duration is a configuration error on every one of them, stated at the call
// site rather than inherited from a parser.
//
// Driving it off envDocs is what makes it a class gate instead of a checklist:
// a duration knob added later is covered the moment it is documented, and one
// wired to a getter with no range check cannot pass.
func TestDurationKnobs_RejectNegative(t *testing.T) {
	const negative = "-1h"
	// The range check is only reachable because the shared grammar accepts a
	// sign. Assert that first, so this test cannot pass on a parse error.
	if _, err := parseDuration(negative); err != nil {
		t.Fatalf("parseDuration(%q) = %v; the range guards are unreachable", negative, err)
	}
	covered := 0
	for _, d := range envDocs {
		if d.Type != durationDocType {
			continue
		}
		covered++
		t.Run(d.Key, func(t *testing.T) {
			t.Setenv(d.Key, negative)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %s=%s; want a range error", d.Key, negative)
			}
			if !strings.Contains(err.Error(), d.Key) {
				t.Errorf("error %q does not name %s", err, d.Key)
			}
		})
	}
	if covered == 0 {
		t.Fatalf("no EnvDoc carries Type %q — the class gate is inspecting nothing", durationDocType)
	}
}

// zeroDocMarker is the inline-code spelling of zero every duration knob that
// ACCEPTS `0` must carry in its documented description. Matching the rendered
// markdown rather than a bare "0" keeps the gate from being satisfied by an
// unrelated digit in a default or a range ("`0..12`" is not an explanation of
// what setting the knob to zero does).
const zeroDocMarker = "`0`"

// TestDurationKnobs_ZeroIsRejectedOrDocumented is the ZERO half of the class
// gate, and the assertion behind the generated reference's promise that "where
// `0` is meaningful it is documented per knob ... where it would leave a
// feature switched on but inert, it is rejected too".
//
// Zero is the one value on this surface that never means what it says: it is a
// sentinel, and which sentinel depends entirely on what consumes the knob —
// Go's `http.Server` falls back to another field, clickhouse-go substitutes its
// own 30s default, the OTLP exporters keep the SDK's, the schema TTLs inherit
// the global, and the reconciler would run enabled-but-inert. An operator
// cannot guess which, so the invariant is: a duration knob either REJECTS zero
// at startup, or its description says what zero does. Silence is the failure.
//
// Driving it off envDocs is what makes it a class gate rather than a checklist:
// a duration knob added later is covered the moment it is documented.
func TestDurationKnobs_ZeroIsRejectedOrDocumented(t *testing.T) {
	const zero = "0s"
	// The sentinel is only reachable because the shared grammar parses it.
	// Assert that first, so this test cannot pass on a parse error.
	if _, err := parseDuration(zero); err != nil {
		t.Fatalf("parseDuration(%q) = %v; the zero arms are unreachable", zero, err)
	}
	covered := 0
	for _, d := range envDocs {
		if d.Type != durationDocType {
			continue
		}
		covered++
		t.Run(d.Key, func(t *testing.T) {
			t.Setenv(d.Key, zero)
			_, err := FromEnv()
			if err != nil {
				if !strings.Contains(err.Error(), d.Key) {
					t.Errorf("error %q rejects zero without naming %s", err, d.Key)
				}
				return
			}
			if !strings.Contains(d.Desc, zeroDocMarker) {
				t.Fatalf("%s accepts %s but its EnvDoc.Desc never says what %s means: %q",
					d.Key, zero, zeroDocMarker, d.Desc)
			}
		})
	}
	if covered == 0 {
		t.Fatalf("no EnvDoc carries Type %q — the class gate is inspecting nothing", durationDocType)
	}
}

// TestParseDuration_GrammarsAgreeOnSharedUnits pins the property that makes
// merging the two grammars safe: on the units both halves accept (ms/s/m/h)
// they resolve a spelling to the SAME duration. If they disagreed anywhere,
// which half happened to parse first would silently change an operator's
// value — the exact class of bug a shared parser is supposed to remove.
func TestParseDuration_GrammarsAgreeOnSharedUnits(t *testing.T) {
	for _, val := range []string{"0", "500ms", "30s", "90m", "2160h", "1h30m", "1h0m0s"} {
		t.Run(val, func(t *testing.T) {
			goDur, err := time.ParseDuration(val)
			if err != nil {
				t.Fatalf("time.ParseDuration(%q): %v", val, err)
			}
			promDur, err := model.ParseDuration(val)
			if err != nil {
				t.Fatalf("model.ParseDuration(%q): %v", val, err)
			}
			if goDur != time.Duration(promDur) {
				t.Fatalf("grammars disagree on %q: Go %s vs retention %s", val, goDur, time.Duration(promDur))
			}
			got, err := parseDuration(val)
			if err != nil {
				t.Fatalf("parseDuration(%q): %v", val, err)
			}
			if got != goDur {
				t.Errorf("parseDuration(%q) = %s; want %s", val, got, goDur)
			}
		})
	}
}
