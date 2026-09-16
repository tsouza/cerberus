package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tsouza/cerberus/test/property/gen"
)

// wantRosterCalls mirrors the six RosterCall values
// test/regression/property_live_roster_floor_test.go pins for its own
// live-roster bindings — this test's own independent proof that run()'s
// output names exactly those six, not a hand-copied guess at what they
// should be.
var wantRosterCalls = []string{
	"gen.PromQLShapeIDs",
	"gen.PromQLRangeShapeIDs",
	"gen.ExpHistogramShapeIDs",
	"gen.LogQLShapeIDs",
	"gen.TraceQLShapeIDs",
	"gen.InstantWindowShapeIDs",
}

func TestRunWritesTheSixRosters(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatalf("run: %v", err)
	}

	var doc export
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}

	if doc.SchemaVersion != schemaVersion {
		t.Fatalf("schema_version = %d, want %d", doc.SchemaVersion, schemaVersion)
	}

	if len(doc.Rosters) != len(wantRosterCalls) {
		t.Fatalf("rosters has %d entries, want %d: %v", len(doc.Rosters), len(wantRosterCalls), doc.Rosters)
	}
	for _, name := range wantRosterCalls {
		ids, ok := doc.Rosters[name]
		if !ok {
			t.Errorf("rosters is missing %q", name)
			continue
		}
		if len(ids) == 0 {
			t.Errorf("rosters[%q] is empty", name)
		}
	}
}

// TestRunMatchesGenDirectly proves run()'s output is a pure reflection of
// gen's own exported functions — never a copy that could drift from them —
// by comparing the decoded roster against a DIRECT call to the same
// function, for every one of the six.
func TestRunMatchesGenDirectly(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	var doc export
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	cases := []struct {
		name string
		want []gen.ShapeID
	}{
		{"gen.PromQLShapeIDs", gen.PromQLShapeIDs()},
		{"gen.PromQLRangeShapeIDs", gen.PromQLRangeShapeIDs()},
		{"gen.ExpHistogramShapeIDs", gen.ExpHistogramShapeIDs()},
		{"gen.LogQLShapeIDs", gen.LogQLShapeIDs()},
		{"gen.TraceQLShapeIDs", gen.TraceQLShapeIDs()},
		{"gen.InstantWindowShapeIDs", gen.InstantWindowShapeIDs()},
	}
	for _, c := range cases {
		got := doc.Rosters[c.name]
		want := shapeIDStrings(c.want)
		if len(got) != len(want) {
			t.Errorf("%s: got %d ids, want %d", c.name, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s[%d] = %q, want %q", c.name, i, got[i], want[i])
			}
		}
	}
}

func TestRunIsDeterministic(t *testing.T) {
	var first, second bytes.Buffer
	if err := run(&first); err != nil {
		t.Fatalf("run (first): %v", err)
	}
	if err := run(&second); err != nil {
		t.Fatalf("run (second): %v", err)
	}
	if first.String() != second.String() {
		t.Fatalf("run() is not deterministic:\nfirst:  %s\nsecond: %s", first.String(), second.String())
	}
}

// failingWriter always errors, exercising run()'s error-propagation path
// (json.Encoder.Encode surfaces a write failure as a non-nil error) without
// needing a real broken file descriptor.
type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestRunPropagatesWriteErrors(t *testing.T) {
	wantErr := errors.New("boom")
	if err := run(failingWriter{err: wantErr}); !errors.Is(err, wantErr) {
		t.Fatalf("run() error = %v, want %v", err, wantErr)
	}
}

func TestShapeIDStrings(t *testing.T) {
	got := shapeIDStrings([]gen.ShapeID{"a", "b", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("shapeIDStrings length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("shapeIDStrings[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestShapeIDStringsEmpty(t *testing.T) {
	got := shapeIDStrings(nil)
	if len(got) != 0 {
		t.Fatalf("shapeIDStrings(nil) = %v, want empty", got)
	}
}
