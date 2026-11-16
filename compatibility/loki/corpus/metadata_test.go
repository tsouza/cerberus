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
	"time"
)

// committedMetadataDir holds the dataset metadata the harness runs with.
const committedMetadataDir = ".."

func TestLoadMetadata_Committed(t *testing.T) {
	t.Parallel()
	md, err := LoadMetadata(committedMetadataDir)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if len(md.AllSelectors) == 0 || len(md.ByServiceName) == 0 || len(md.RangesBySelector) == 0 {
		t.Fatalf("committed metadata decoded hollow: %d selectors, %d services, %d range entries",
			len(md.AllSelectors), len(md.ByServiceName), len(md.RangesBySelector))
	}
	ranged := 0
	for _, sel := range md.AllSelectors {
		if r := md.RangesBySelector[sel]; r != nil && r.MinRange > 0 {
			ranged++
		}
	}
	if ranged == 0 {
		t.Fatal("no committed selector has a positive range bound, so no ${RANGE} query can resolve")
	}
	if !md.TimeRange.End.After(md.TimeRange.Start) {
		t.Fatalf("time range %v..%v is empty", md.TimeRange.Start, md.TimeRange.End)
	}
}

// TestMetadata_ModelsEveryKeyTheResolverReads keeps the struct and the
// committed file in step: every key of dataset_metadata.json is either a
// field of Metadata or one of the keys the resolver deliberately does not
// read, so a renamed index cannot decode to an empty map unnoticed.
func TestMetadata_ModelsEveryKeyTheResolverReads(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(committedMetadataDir, MetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	modelled := map[string]bool{}
	ty := reflect.TypeFor[Metadata]()
	for field := range ty.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		modelled[name] = true
		if _, ok := doc[name]; !ok {
			t.Errorf("Metadata.%s reads key %q, which the committed file does not carry", field.Name, name)
		}
	}
	unread := []string{"_comment", "statistics"}
	var extra []string
	for key := range doc {
		if !modelled[key] && !slices.Contains(unread, key) {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Fatalf("dataset metadata keys %v are neither modelled nor listed as unread", extra)
	}
}

func TestLoadMetadata_Rejects(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"wrong version":                  `{"version": "2.0"}`,
		"no version":                     `{}`,
		"malformed":                      `{"version": `,
		"repeated selector":              `{"version": "1.0", "all_selectors": ["{a=\"1\"}", "{a=\"2\"}", "{a=\"1\"}"]}`,
		"repeated index entry":           `{"version": "1.0", "all_selectors": ["{a=\"1\"}", "{a=\"2\"}"], "by_label_key": {"a": ["{a=\"2\"}", "{a=\"1\"}", "{a=\"2\"}"]}}`,
		"index entry not a selector":     `{"version": "1.0", "all_selectors": ["{a=\"1\"}"], "by_format": {"json": ["{a=\"1\"}", "{a=\"3\"}"]}}`,
		"range bound not for a selector": `{"version": "1.0", "all_selectors": ["{a=\"1\"}"], "metadata_by_selector": {"{a=\"3\"}": {"min_range": 1}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, MetadataFileName), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadMetadata(dir); err == nil {
				t.Fatalf("LoadMetadata accepted %s", body)
			}
		})
	}
	if _, err := LoadMetadata(t.TempDir()); err == nil {
		t.Fatal("LoadMetadata over a directory with no metadata file returned no error")
	}
}

func TestLoadMetadata_DurationsAreNanoseconds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"version": "1.0", "all_selectors": ["{a=\"b\"}"], "metadata_by_selector": {"{a=\"b\"}": {"min_range": 300000000000, "min_instant_range": 600000000000}}}`
	if err := os.WriteFile(filepath.Join(dir, MetadataFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	md, err := LoadMetadata(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := md.RangesBySelector[`{a="b"}`]
	if r.MinRange != 5*time.Minute || r.MinInstantRange != 10*time.Minute {
		t.Fatalf("ranges = %+v, want 5m / 10m", r)
	}
}

// TestLoadMetadata_SortsEveryList loads a file whose lists are all out of
// order and requires each to come back ascending, so a seeded draw indexes
// the same stream however the file orders its lists.
func TestLoadMetadata_SortsEveryList(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"version": "1.0",
		"all_selectors": ["{a=\"3\"}", "{a=\"1\"}", "{a=\"2\"}"],
		"by_format": {"json": ["{a=\"2\"}", "{a=\"1\"}"]},
		"by_service_name": {"s": ["{a=\"3\"}", "{a=\"2\"}"]},
		"by_unwrappable_field": {"duration": ["{a=\"3\"}", "{a=\"1\"}"]},
		"by_detected_field": {"level": ["{a=\"2\"}", "{a=\"1\"}"]},
		"by_structured_metadata": {"detected_level": ["{a=\"3\"}", "{a=\"2\"}", "{a=\"1\"}"]},
		"by_label_key": {"a": ["{a=\"2\"}", "{a=\"3\"}", "{a=\"1\"}"]},
		"by_keyword": {"error": ["{a=\"3\"}", "{a=\"1\"}"]}}`
	if err := os.WriteFile(filepath.Join(dir, MetadataFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	md, err := LoadMetadata(dir)
	if err != nil {
		t.Fatal(err)
	}
	lists := map[string][]string{"all_selectors": md.AllSelectors}
	for name, index := range map[string]map[string][]string{
		"by_service_name":        md.ByServiceName,
		"by_unwrappable_field":   md.ByUnwrappableField,
		"by_detected_field":      md.ByDetectedField,
		"by_structured_metadata": md.ByStructuredMetadata,
		"by_label_key":           md.ByLabelKey,
		"by_keyword":             md.ByKeyword,
	} {
		for key, sels := range index {
			lists[name+"/"+key] = sels
		}
	}
	for f, sels := range md.ByFormat {
		lists["by_format/"+string(f)] = sels
	}
	if len(lists) != 8 {
		t.Fatalf("decoded %d lists, want 8", len(lists))
	}
	for name, sels := range lists {
		if len(sels) < 2 || !slices.IsSorted(sels) {
			t.Errorf("%s = %v, want it sorted ascending", name, sels)
		}
	}
}
