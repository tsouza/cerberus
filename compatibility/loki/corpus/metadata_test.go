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
	for i := range ty.NumField() {
		name, _, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ",")
		modelled[name] = true
		if _, ok := doc[name]; !ok {
			t.Errorf("Metadata.%s reads key %q, which the committed file does not carry", ty.Field(i).Name, name)
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
		"wrong version": `{"version": "2.0"}`,
		"no version":    `{}`,
		"malformed":     `{"version": `,
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
	body := `{"version": "1.0", "metadata_by_selector": {"{a=\"b\"}": {"min_range": 300000000000, "min_instant_range": 600000000000}}}`
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
