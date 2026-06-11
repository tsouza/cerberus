package consumercorpus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Entry is one captured consumer request shape plus the contract its
// consumer enforces on the response.
type Entry struct {
	// Name is the unique entry identifier (kebab-case, used as the
	// subtest name). Must match the file's base name without .json.
	Name string `json:"name"`
	// Datasource routes the entry to the right handler: "prom",
	// "loki", or "tempo".
	Datasource string `json:"datasource"`
	// Consumer names the exact Grafana code path that issues this
	// request and decodes its response (plugin + version surface).
	Consumer string `json:"consumer"`
	// Provenance records where the shape was captured from — live
	// stack, spec file, upstream app source — one string per line.
	Provenance []string `json:"provenance"`
	// GrafanaRequest is the Grafana-side request that produced this
	// downstream request (e.g. the /api/ds/query body, or the
	// drilldown app URL). Informational but mandatory: it ties the
	// entry back to the consumer behaviour it pins.
	GrafanaRequest map[string]any `json:"grafana_request"`
	// Request is the HTTP request cerberus receives.
	Request RequestSpec `json:"request"`
	// Stub names the canned-row fixture the default (stub) lane backs
	// the handler with. See stub.go for the fixture registry.
	Stub string `json:"stub"`
	// Expect declares the consumer contract.
	Expect Expect `json:"expect"`

	// Version is the corpus directory the entry was loaded from
	// (e.g. "grafana-12.2.9"). Populated by Load, not by the file.
	Version string `json:"-"`
}

// RequestSpec is the wire request cerberus receives. Query values and
// the path may carry ${TOKEN} placeholders (e.g. ${START_UNIX},
// ${TRACE_ID}) that each replay lane expands against its own seed
// anchors — see replay.go.
type RequestSpec struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Expect declares the per-entry consumer contract.
type Expect struct {
	// Status is the expected HTTP status (0 means 200).
	Status int `json:"status,omitempty"`
	// Decode names the consumer decoder from the registry in
	// replay.go. The decode itself is an assertion: it mirrors the
	// exact unmarshal the consumer performs and fails on drift.
	Decode string `json:"decode"`
	// Wire predicates run in BOTH lanes — they must hold for the
	// stub fixture's canned rows as well as for the chdb seed.
	Wire []string `json:"wire,omitempty"`
	// Data predicates run in the chdb lane only — they assert
	// data-bearing properties the stub lane's canned rows don't
	// faithfully reproduce (row counts through real SQL, value
	// sanity through real execution).
	Data []string `json:"data,omitempty"`
}

// versionDirRE pins the version-keyed directory convention.
var versionDirRE = regexp.MustCompile(`^grafana-\d+\.\d+\.\d+$`)

// validDatasources is the closed set of handler routes.
var validDatasources = map[string]bool{"prom": true, "loki": true, "tempo": true}

// Load reads every corpus entry under dir (each version-keyed
// subdirectory), strictly decoding and validating each file. The
// returned slice is sorted by (version, name) for deterministic
// iteration.
func Load(dir string) ([]Entry, error) {
	versions, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("consumercorpus: read corpus root %s: %w", dir, err)
	}
	var entries []Entry
	seen := map[string]bool{}
	for _, v := range versions {
		if !v.IsDir() {
			continue
		}
		if !versionDirRE.MatchString(v.Name()) {
			return nil, fmt.Errorf("consumercorpus: corpus subdirectory %q does not match the version-keyed convention %s", v.Name(), versionDirRE)
		}
		files, err := os.ReadDir(filepath.Join(dir, v.Name()))
		if err != nil {
			return nil, fmt.Errorf("consumercorpus: read version dir %s: %w", v.Name(), err)
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				return nil, fmt.Errorf("consumercorpus: %s/%s is not a corpus entry (.json files only)", v.Name(), f.Name())
			}
			path := filepath.Join(dir, v.Name(), f.Name())
			e, err := loadEntry(path)
			if err != nil {
				return nil, err
			}
			e.Version = v.Name()
			key := e.Version + "/" + e.Name
			if seen[key] {
				return nil, fmt.Errorf("consumercorpus: duplicate entry name %s", key)
			}
			seen[key] = true
			if want := strings.TrimSuffix(f.Name(), ".json"); e.Name != want {
				return nil, fmt.Errorf("consumercorpus: %s: entry name %q must match file base name %q", path, e.Name, want)
			}
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Version != entries[j].Version {
			return entries[i].Version < entries[j].Version
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

// loadEntry strictly decodes and validates one corpus file.
func loadEntry(path string) (Entry, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, fmt.Errorf("consumercorpus: read %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	var e Entry
	if err := dec.Decode(&e); err != nil {
		return Entry{}, fmt.Errorf("consumercorpus: decode %s: %w", path, err)
	}
	if err := e.validate(); err != nil {
		return Entry{}, fmt.Errorf("consumercorpus: %s: %w", path, err)
	}
	return e, nil
}

// validate enforces the per-entry schema invariants the ratchet
// meta-test relies on.
func (e Entry) validate() error {
	switch {
	case e.Name == "":
		return fmt.Errorf("name is required")
	case !validDatasources[e.Datasource]:
		return fmt.Errorf("datasource %q must be one of prom/loki/tempo", e.Datasource)
	case e.Consumer == "":
		return fmt.Errorf("consumer is required")
	case len(e.Provenance) == 0:
		return fmt.Errorf("provenance is required — every entry must say where its shape was captured from")
	case len(e.GrafanaRequest) == 0:
		return fmt.Errorf("grafana_request is required — every entry must carry the Grafana-side request that produced it")
	case e.Request.Method == "":
		return fmt.Errorf("request.method is required")
	case e.Request.Path == "":
		return fmt.Errorf("request.path is required")
	case e.Stub == "":
		return fmt.Errorf("stub fixture name is required (use \"empty\" when the wire contract needs no canned rows)")
	case e.Expect.Decode == "":
		return fmt.Errorf("expect.decode is required — an entry that doesn't decode as its consumer pins nothing")
	}
	for _, p := range append(append([]string{}, e.Expect.Wire...), e.Expect.Data...) {
		if _, _, err := splitPredicate(p); err != nil {
			return err
		}
	}
	return nil
}

// splitPredicate parses "name" or "name:arg" predicate strings.
func splitPredicate(p string) (name, arg string, err error) {
	if p == "" {
		return "", "", fmt.Errorf("empty predicate")
	}
	name, arg, _ = strings.Cut(p, ":")
	if name == "" {
		return "", "", fmt.Errorf("predicate %q has no name", p)
	}
	return name, arg, nil
}
