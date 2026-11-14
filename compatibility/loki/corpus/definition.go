// Package corpus loads the LogQL differential corpus the Loki compatibility
// harness runs: query-definition YAML files grouped into suites, the dataset
// metadata that describes the seeded streams, and the seeded expansion of each
// definition's template placeholders into concrete test cases.
//
// The same loader reads two corpora: the upstream query files, read in place
// from the checksum-pinned Go module (see [UpstreamQueriesDir]), and the
// cerberus-owned files under compatibility/loki/cerberus-queries/. Both follow
// the file format schema.json in this directory describes.
package corpus

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Suite names one subdirectory of a corpus root.
type Suite string

// The three suites every corpus root carries, in load order.
const (
	SuiteFast       Suite = "fast"
	SuiteRegression Suite = "regression"
	SuiteExhaustive Suite = "exhaustive"
)

// AllSuites lists every suite in load order.
var AllSuites = []Suite{SuiteFast, SuiteRegression, SuiteExhaustive}

// Query kinds a definition declares (or has inferred, see [InferKind]).
const (
	KindLog    = "log"
	KindMetric = "metric"
)

// Directions selects which log-query directions a log definition expands
// into. A metric definition always expands to one forward case.
type Directions string

// The accepted `directions:` values.
const (
	DirectionsForward  Directions = "forward"
	DirectionsBackward Directions = "backward"
	DirectionsBoth     Directions = "both"
)

// TimeRange is a definition's `time_range:` block: the window length and,
// for metric queries, the step.
type TimeRange struct {
	Length string `yaml:"length"`
	Step   string `yaml:"step,omitempty"`
}

// Requirements is a definition's `requires:` block: the stream properties a
// `${SELECTOR}` substitution must satisfy. Every listed value narrows the
// candidate set.
type Requirements struct {
	LogFormat          string   `yaml:"log_format,omitempty"`
	UnwrappableFields  []string `yaml:"unwrappable_fields,omitempty"`
	StructuredMetadata []string `yaml:"structured_metadata,omitempty"`
	DetectedFields     []string `yaml:"detected_fields,omitempty"`
	Labels             []string `yaml:"labels,omitempty"`
	Keywords           []string `yaml:"keywords,omitempty"`
}

// Definition is one entry of a query file's `queries:` list.
type Definition struct {
	Description string       `yaml:"description"`
	Query       string       `yaml:"query"`
	Kind        string       `yaml:"kind,omitempty"`
	Skip        bool         `yaml:"skip,omitempty"`
	TimeRange   TimeRange    `yaml:"time_range"`
	Directions  Directions   `yaml:"directions,omitempty"`
	Requires    Requirements `yaml:"requires,omitempty"`
	Tags        []string     `yaml:"tags,omitempty"`
	Notes       string       `yaml:"notes,omitempty"`

	// Source is `<suite>/<file>:<line>`, the position of the entry in its
	// file. It is set by the loader, never read from YAML.
	Source string `yaml:"-"`
}

// File returns the definition's `<suite>/<file>` without the line suffix.
func (d Definition) File() string {
	file, _, _ := strings.Cut(d.Source, ":")
	return file
}

// length parses the definition's window length.
func (d Definition) length() (time.Duration, error) {
	return time.ParseDuration(d.TimeRange.Length)
}

// step parses the definition's step; an absent step is zero.
func (d Definition) step() (time.Duration, error) {
	if d.TimeRange.Step == "" {
		return 0, nil
	}
	return time.ParseDuration(d.TimeRange.Step)
}

// queryFile is the top-level shape of one YAML file.
type queryFile struct {
	Queries []Definition `yaml:"queries"`
}

// yamlExtensions are the file suffixes a suite directory is scanned for.
var yamlExtensions = []string{".yaml", ".yml"}

// Load reads every query file of the given suites under root. Definitions
// come back in a fixed order: suite order as given, then file name, then
// position within the file. Each suite directory must exist, so a mistyped
// root fails instead of contributing nothing.
//
// Skipped definitions (`skip: true`) are included; callers partition them
// with [Partition].
func Load(root string, suites ...Suite) ([]Definition, error) {
	var defs []Definition
	for _, suite := range suites {
		dir := filepath.Join(root, string(suite))
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("suite %s: %w", suite, err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() && hasYAMLExtension(entry.Name()) {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			fileDefs, err := loadFile(filepath.Join(dir, name), string(suite)+"/"+name)
			if err != nil {
				return nil, err
			}
			defs = append(defs, fileDefs...)
		}
	}
	return defs, nil
}

func hasYAMLExtension(name string) bool {
	for _, ext := range yamlExtensions {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// loadFile decodes one query file, records each entry's line, and applies
// the defaults and checks in [normalise]. label is the `<suite>/<file>`
// prefix of every Source.
func loadFile(path, label string) ([]Definition, error) {
	data, err := os.ReadFile(path) //nolint:gosec // corpus path chosen by the harness operator
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}

	// An empty file is zero definitions, not an error (io.EOF).
	var file queryFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", label, err)
	}

	lines, err := entryLines(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if len(lines) != len(file.Queries) {
		return nil, fmt.Errorf("%s: found %d query entries but decoded %d", label, len(lines), len(file.Queries))
	}

	for i := range file.Queries {
		def := &file.Queries[i]
		def.Source = fmt.Sprintf("%s:%d", label, lines[i])
		if err := normalise(def); err != nil {
			return nil, fmt.Errorf("%s: %w", def.Source, err)
		}
	}
	return file.Queries, nil
}

// entryLines returns the 1-based line of every item of the top-level
// `queries:` sequence, in order.
func entryLines(data []byte) ([]int, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	top := doc.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != "queries" {
			continue
		}
		items := top.Content[i+1].Content
		lines := make([]int, len(items))
		for j, item := range items {
			lines[j] = item.Line
		}
		return lines, nil
	}
	return nil, nil
}

// normalise validates a decoded definition and fills in its defaults:
//
//   - kind, when absent, is inferred from the query shape ([InferKind]);
//   - directions, when absent on a log definition, is both;
//   - time_range.length is required, and both durations must parse.
func normalise(def *Definition) error {
	switch def.Kind {
	case "":
		def.Kind = InferKind(def.Query)
	case KindLog, KindMetric:
	default:
		return fmt.Errorf("query %q: kind %q is not %q or %q", def.Description, def.Kind, KindLog, KindMetric)
	}

	switch def.Directions {
	case "":
		if def.Kind == KindLog {
			def.Directions = DirectionsBoth
		}
	case DirectionsForward, DirectionsBackward, DirectionsBoth:
	default:
		return fmt.Errorf("query %q: directions %q is not forward, backward or both", def.Description, def.Directions)
	}

	if def.TimeRange.Length == "" {
		return fmt.Errorf("query %q: time_range.length is required", def.Description)
	}
	if _, err := def.length(); err != nil {
		return fmt.Errorf("query %q: time_range.length: %w", def.Description, err)
	}
	if _, err := def.step(); err != nil {
		return fmt.Errorf("query %q: time_range.step: %w", def.Description, err)
	}
	return nil
}

// Partition splits definitions into the runnable ones and the ones the
// corpus marks `skip: true`, preserving order within each.
func Partition(defs []Definition) (runnable, skipped []Definition) {
	for _, def := range defs {
		if def.Skip {
			skipped = append(skipped, def)
		} else {
			runnable = append(runnable, def)
		}
	}
	return runnable, skipped
}
