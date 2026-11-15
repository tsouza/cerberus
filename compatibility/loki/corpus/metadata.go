package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// the per-stream range bounds. Every index list is read as a sorted set
// (see [intersectSorted]).
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

// LoadMetadata reads [MetadataFileName] from dir.
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
	return &md, nil
}
