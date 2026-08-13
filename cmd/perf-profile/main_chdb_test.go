//go:build chdb

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/perf/profile"
)

// fanFactorLabel is the one place the tool decides how to render a fixture
// it could NOT measure. Collapsing nil to a number would put a fabricated
// fan factor in the nightly summary, indistinguishable from a measured one.
func TestFanFactorLabelSpellsOutAnUnmeasuredFixture(t *testing.T) {
	t.Parallel()

	if got := fanFactorLabel(nil); got != "unmeasured" {
		t.Fatalf("fanFactorLabel(nil) = %q, want %q", got, "unmeasured")
	}
	f := 12.345
	if got := fanFactorLabel(&f); got != "12.35" {
		t.Fatalf("fanFactorLabel(&12.345) = %q, want %q", got, "12.35")
	}
	zero := 0.0
	if got := fanFactorLabel(&zero); got != "0.00" {
		t.Fatalf("a measured zero rendered as %q, want %q — it is a measurement, not a gap", got, "0.00")
	}
}

func TestTruncateKeepsTheWidthItPromises(t *testing.T) {
	t.Parallel()

	if got := truncate("short", 48); got != "short" {
		t.Fatalf("truncate did not leave a short string alone: %q", got)
	}
	got := truncate(strings.Repeat("x", 60), 10)
	if len([]rune(got)) != 10 {
		t.Fatalf("truncate(60 chars, 10) = %q (%d runes), want 10 runes", got, len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a truncated fixture name is not marked as such: %q", got)
	}
	// n <= 1 leaves no room for the ellipsis and must not index past the end.
	if got := truncate("abc", 1); got != "a" {
		t.Fatalf("truncate(\"abc\", 1) = %q, want %q", got, "a")
	}
}

func TestYesNoRenderersAreDistinguishable(t *testing.T) {
	t.Parallel()

	if yesno(true) == yesno(false) {
		t.Fatal("yesno renders true and false identically")
	}
	if mdYesNo(true) == mdYesNo(false) {
		t.Fatal("mdYesNo renders true and false identically")
	}
	if mdYesNo(false) != "" {
		t.Fatalf("mdYesNo(false) = %q, want an empty markdown cell", mdYesNo(false))
	}
}

func TestWriteJSONRoundTripsTheRecordsItWasGiven(t *testing.T) {
	t.Parallel()

	fan := 3.5
	recs := []profile.Record{{Fixture: "promql/rate_basic.txtar", FanFactor: &fan, ScanRows: 1000}}
	out := filepath.Join(t.TempDir(), "profile.json")
	if err := writeJSON(out, recs); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got []profile.Record
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("the artifact is not valid JSON: %v", err)
	}
	if len(got) != 1 || got[0].Fixture != recs[0].Fixture || got[0].ScanRows != recs[0].ScanRows {
		t.Fatalf("round-tripped %+v, want %+v", got, recs)
	}
}

// writeMarkdown opens its target in APPEND mode so it can be pointed
// straight at $GITHUB_STEP_SUMMARY. Truncating instead would silently drop
// whatever earlier steps had already written there.
func TestWriteMarkdownAppendsRatherThanTruncates(t *testing.T) {
	t.Parallel()

	out := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(out, []byte("## an earlier step\n"), 0o600); err != nil {
		t.Fatalf("seed the summary: %v", err)
	}
	fan := 9.5
	recs := []profile.Record{{Fixture: "traceql/spanset_basic.txtar", FanFactor: &fan}}
	if err := writeMarkdown(out, recs, 1, 1, 0, 0, fan); err != nil {
		t.Fatalf("writeMarkdown: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "an earlier step") {
		t.Fatalf("writeMarkdown truncated the summary it was appending to:\n%s", body)
	}
	if !strings.Contains(body, recs[0].Fixture) {
		t.Fatalf("the profiled fixture is missing from the summary:\n%s", body)
	}
}
