package corpus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestUpstreamQueriesDir_ResolvesThePinnedCorpus fetches the pinned
// module and checks that the directory it returns is a corpus root the
// loader reads: all three suites present, with definitions in each.
func TestUpstreamQueriesDir_ResolvesThePinnedCorpus(t *testing.T) {
	t.Parallel()
	dir, err := UpstreamQueriesDir()
	if err != nil {
		t.Fatalf("UpstreamQueriesDir: %v", err)
	}
	if !strings.Contains(filepath.ToSlash(dir), UpstreamModule+"@"+UpstreamVersion+"/"+UpstreamQueriesSubdir) {
		t.Fatalf("UpstreamQueriesDir = %s, want a path inside %s@%s", dir, UpstreamModule, UpstreamVersion)
	}
	for _, s := range AllSuites {
		defs, err := Load(dir, s)
		if err != nil {
			t.Fatalf("Load(%s): %v", s, err)
		}
		if len(defs) == 0 {
			t.Fatalf("suite %s of the pinned corpus has no definitions", s)
		}
	}
}

func TestOpaqueID(t *testing.T) {
	t.Parallel()
	a := OpaqueID("fast/a.yaml#some description")
	if a != OpaqueID("fast/a.yaml#some description") {
		t.Fatal("OpaqueID is not deterministic")
	}
	if a == OpaqueID("fast/a.yaml#some description.") {
		t.Fatal("two readable identities share an opaque ID")
	}
	if !strings.HasPrefix(a, opaqueIDPrefix) || len(a) != len(opaqueIDPrefix)+opaqueIDHexDigits {
		t.Fatalf("OpaqueID = %q, want %s followed by %d hex digits", a, opaqueIDPrefix, opaqueIDHexDigits)
	}
	if strings.Contains(a, "description") {
		t.Fatalf("OpaqueID %q carries its input", a)
	}
}

// cerberusQueriesDir is the cerberus-owned corpus root, which carries the
// file-format schema.
const cerberusQueriesDir = "../cerberus-queries"

// TestSchema_MatchesTheLoader keeps schema.json and the loader in step:
// the properties the schema allows are exactly the YAML keys the loader
// decodes (it rejects any other key), and the enumerations are the
// values the loader accepts.
func TestSchema_MatchesTheLoader(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(cerberusQueriesDir, "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	type node struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Enum       []string                   `json:"enum"`
	}
	var schema struct {
		node
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	decode := func(msg json.RawMessage) node {
		var n node
		if err := json.Unmarshal(msg, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	def := decode(schema.Defs["definition"])
	checkKeys := func(what string, props map[string]json.RawMessage, ty reflect.Type) {
		var want []string
		for i := range ty.NumField() {
			name, _, _ := strings.Cut(ty.Field(i).Tag.Get("yaml"), ",")
			if name != "-" {
				want = append(want, name)
			}
		}
		got := make([]string, 0, len(props))
		for k := range props {
			got = append(got, k)
		}
		sort.Strings(want)
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Fatalf("schema %s properties = %v, loader keys = %v", what, got, want)
		}
	}
	checkKeys("file", schema.Properties, reflect.TypeFor[queryFile]())
	checkKeys("definition", def.Properties, reflect.TypeFor[Definition]())
	checkKeys("time_range", decode(def.Properties["time_range"]).Properties, reflect.TypeFor[TimeRange]())
	requires := decode(def.Properties["requires"])
	checkKeys("requires", requires.Properties, reflect.TypeFor[Requirements]())

	checkEnum := func(what string, got, want []string) {
		sort.Strings(got)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Fatalf("schema %s enum = %v, loader accepts %v", what, got, want)
		}
	}
	checkEnum("kind", decode(def.Properties["kind"]).Enum, []string{KindLog, KindMetric})
	checkEnum("directions", decode(def.Properties["directions"]).Enum,
		[]string{string(DirectionsForward), string(DirectionsBackward), string(DirectionsBoth)})
	checkEnum("log_format", decode(requires.Properties["log_format"]).Enum,
		[]string{string(LogFormatJSON), string(LogFormatLogfmt), string(LogFormatUnstructured)})
}

// TestCerberusQueries_PointAtTheSchema — every cerberus-owned query file
// names the cerberus schema for editor validation, and loads.
func TestCerberusQueries_PointAtTheSchema(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join(cerberusQueriesDir, "*", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no cerberus-owned query files found")
	}
	const directive = "# yaml-language-server: $schema=../schema.json"
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // test fixture path
		if err != nil {
			t.Fatal(err)
		}
		if first, _, _ := strings.Cut(string(raw), "\n"); first != directive {
			t.Fatalf("%s starts with %q, want %q", f, first, directive)
		}
	}
	if _, err := Load(cerberusQueriesDir, AllSuites...); err != nil {
		t.Fatalf("Load(cerberus-queries): %v", err)
	}
}
