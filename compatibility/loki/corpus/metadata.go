package corpus

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// MetadataFileName is the dataset-metadata file read from a metadata
// directory.
const MetadataFileName = "dataset_metadata.json"

// MetadataVersion is the only `version` [LoadMetadata] accepts.
const MetadataVersion = "1.0"

// LogFormat is a stream's line encoding, as the `by_format` index and a
// definition's `requires.log_format` name it.
type LogFormat string

// The line encodings the seeded dataset carries.
const (
	LogFormatJSON         LogFormat = "json"
	LogFormatLogfmt       LogFormat = "logfmt"
	LogFormatUnstructured LogFormat = "unstructured"
)

// Window is the dataset's time span.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// StreamRanges are the per-stream minimum `${RANGE}` durations, in
// nanoseconds on the wire: the shortest window that still holds enough
// samples for a range query and for an instant query.
type StreamRanges struct {
	MinRange        time.Duration `json:"min_range"`
	MinInstantRange time.Duration `json:"min_instant_range"`
}

// Metadata describes the seeded dataset: every stream selector, indexes
// from a stream property to the selectors that have it, the time span, and
// the per-stream range bounds. Every list is a set of selectors.
// [LoadMetadata] sorts each one, so a read by position — the resolver's
// seeded draw over `all_selectors`, a pass that takes a service's first
// selector — depends on the set the file declares and not on its layout.
// Metadata built in code is read as given.
type Metadata struct {
	AllSelectors         []string                 `json:"all_selectors"`
	ByFormat             map[LogFormat][]string   `json:"by_format"`
	ByServiceName        map[string][]string      `json:"by_service_name"`
	ByUnwrappableField   map[string][]string      `json:"by_unwrappable_field"`
	ByDetectedField      map[string][]string      `json:"by_detected_field"`
	ByStructuredMetadata map[string][]string      `json:"by_structured_metadata"`
	ByLabelKey           map[string][]string      `json:"by_label_key"`
	ByKeyword            map[string][]string      `json:"by_keyword"`
	RangesBySelector     map[string]*StreamRanges `json:"metadata_by_selector"`
	TimeRange            Window                   `json:"time_range"`
	Version              string                   `json:"version"`
}

// LoadMetadata reads [MetadataFileName] from dir, rejects a list that
// repeats a selector or an index entry or range bound for a selector
// `all_selectors` does not carry, and sorts every list ascending, so the
// stream a seeded draw picks depends on the set of streams the file
// declares and never on the order it lists them in.
func LoadMetadata(dir string) (*Metadata, error) {
	path := filepath.Join(dir, MetadataFileName)
	data, err := os.ReadFile(path) //nolint:gosec // metadata directory chosen by the harness operator
	if err != nil {
		return nil, fmt.Errorf("dataset metadata: %w", err)
	}
	var md Metadata
	if err := json.Unmarshal(data, &md); err != nil {
		return nil, fmt.Errorf("dataset metadata %s: %w", path, err)
	}
	if md.Version != MetadataVersion {
		return nil, fmt.Errorf("dataset metadata %s: version %q, want %q", path, md.Version, MetadataVersion)
	}
	if err := md.canonicalise(); err != nil {
		return nil, fmt.Errorf("dataset metadata %s: %w", path, err)
	}
	return &md, nil
}

// canonicalise validates md's lists as sets of `all_selectors` members and
// sorts each one in place.
func (md *Metadata) canonicalise() error {
	slices.Sort(md.AllSelectors)
	if i := firstRepeat(md.AllSelectors); i >= 0 {
		return fmt.Errorf("all_selectors lists %s twice", md.AllSelectors[i])
	}
	for sel := range md.RangesBySelector {
		if _, found := slices.BinarySearch(md.AllSelectors, sel); !found {
			return fmt.Errorf("metadata_by_selector has range bounds for %s, which all_selectors does not carry", sel)
		}
	}
	for _, ix := range md.indexes() {
		for _, name := range slices.Sorted(maps.Keys(ix.lists)) {
			sels := ix.lists[name]
			slices.Sort(sels)
			if i := firstRepeat(sels); i >= 0 {
				return fmt.Errorf("%s[%q] lists %s twice", ix.key, name, sels[i])
			}
			for _, sel := range sels {
				if _, found := slices.BinarySearch(md.AllSelectors, sel); !found {
					return fmt.Errorf("%s[%q] lists %s, which all_selectors does not carry", ix.key, name, sel)
				}
			}
		}
	}
	return nil
}

// metadataIndex is one stream-property index of [Metadata]: its key in
// the file and its lists, keyed by property value.
type metadataIndex struct {
	key   string
	lists map[string][]string
}

// indexes returns every stream-property index of md. The lists share
// their backing arrays with md, so sorting one sorts md's.
func (md *Metadata) indexes() []metadataIndex {
	formats := make(map[string][]string, len(md.ByFormat))
	for f, sels := range md.ByFormat {
		formats[string(f)] = sels
	}
	return []metadataIndex{
		{"by_format", formats},
		{"by_service_name", md.ByServiceName},
		{"by_unwrappable_field", md.ByUnwrappableField},
		{"by_detected_field", md.ByDetectedField},
		{"by_structured_metadata", md.ByStructuredMetadata},
		{"by_label_key", md.ByLabelKey},
		{"by_keyword", md.ByKeyword},
	}
}

// firstRepeat returns the index of the first element of the sorted list
// sels that equals its predecessor, or -1 when every element is distinct.
func firstRepeat(sels []string) int {
	for i := 1; i < len(sels); i++ {
		if sels[i] == sels[i-1] {
			return i
		}
	}
	return -1
}
