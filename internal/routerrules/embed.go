package routerrules

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
)

// catalogFS is the shipped split catalog, compiled into the binary so the CLI
// works with no external file. The base (catalog.yaml) carries apiVersion /
// catalogVersion / params; each rules/<rule_id>.yaml carries exactly one rule.
// An operator can override the whole set at runtime via --catalog <path> to test
// a newer ruleset. This FS is the single audit target for the no-numbers
// invariant — the guard test walks the base plus every rule file.
//
//go:embed catalog/catalog.yaml catalog/rules/*.yaml
var catalogFS embed.FS

const (
	// catalogBasePath is the embedded base file: schema-shape contract + params.
	catalogBasePath = "catalog/catalog.yaml"
	// catalogRulesDir holds one file per rule, filename == rule id.
	catalogRulesDir = "catalog/rules"
)

// CatalogFile pairs an embedded catalog file's name with its raw bytes. The
// no-numbers guard test walks every file the binary ships so the reviewer-facing
// proof runs against exactly what ships, base and rules alike.
type CatalogFile struct {
	Name  string
	Bytes []byte
}

// embeddedBase returns the raw bytes of the base catalog file.
func embeddedBase() ([]byte, error) {
	return catalogFS.ReadFile(catalogBasePath)
}

// embeddedRuleFiles returns every rules/*.yaml file, sorted by name, so the
// merge order (and therefore the in-memory rule order) is deterministic and
// independent of FS iteration order.
func embeddedRuleFiles() ([]CatalogFile, error) {
	entries, err := fs.ReadDir(catalogFS, catalogRulesDir)
	if err != nil {
		return nil, fmt.Errorf("routerrules: read embedded rules dir: %w", err)
	}
	out := make([]CatalogFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := path.Join(catalogRulesDir, e.Name())
		b, err := catalogFS.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("routerrules: read embedded rule %q: %w", p, err)
		}
		out = append(out, CatalogFile{Name: e.Name(), Bytes: b})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// EmbeddedCatalogFiles returns the base file followed by every rule file, in the
// deterministic order the loader merges them (base first, then rules sorted by
// filename). The no-numbers guard test consumes this so it audits every file the
// binary ships.
func EmbeddedCatalogFiles() ([]CatalogFile, error) {
	base, err := embeddedBase()
	if err != nil {
		return nil, fmt.Errorf("routerrules: read embedded base catalog: %w", err)
	}
	rules, err := embeddedRuleFiles()
	if err != nil {
		return nil, err
	}
	out := make([]CatalogFile, 0, len(rules)+1)
	out = append(out, CatalogFile{Name: path.Base(catalogBasePath), Bytes: base})
	out = append(out, rules...)
	return out, nil
}
