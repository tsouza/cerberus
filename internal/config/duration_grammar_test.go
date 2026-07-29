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
