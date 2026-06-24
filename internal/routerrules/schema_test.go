package routerrules

import (
	"strings"
	"testing"
)

func TestDecodeEmbeddedCatalogRoundTrips(t *testing.T) {
	files, err := EmbeddedCatalogFiles()
	if err != nil {
		t.Fatalf("read embedded catalog files: %v", err)
	}
	cat, err := mergeCatalogFiles(files)
	if err != nil {
		t.Fatalf("merge embedded catalog: %v", err)
	}
	if cat.APIVersion != SchemaAPIVersion {
		t.Fatalf("apiVersion = %q, want %q", cat.APIVersion, SchemaAPIVersion)
	}
	if cat.CatalogVersion <= 0 {
		t.Fatalf("catalogVersion = %d, want > 0", cat.CatalogVersion)
	}
	if len(cat.Params) == 0 {
		t.Fatalf("expected params in embedded catalog")
	}
	if len(cat.Rules) == 0 {
		t.Fatalf("expected rules in embedded catalog")
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	const y = `
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params: []
rules:
  - id: r
    severity: low
    since: 1
    status: active
    group_by: [shape_id]
    condition: { col: route, op: eq, enum: A }
    finding: "x"
    bogus_field: 7
`
	_, err := DecodeCatalog([]byte(y))
	if err == nil {
		t.Fatalf("expected decode to reject unknown field bogus_field")
	}
	if !strings.Contains(err.Error(), "bogus_field") {
		t.Fatalf("error should name the unknown field, got: %v", err)
	}
}

func TestLoadEmbeddedCatalogValidates(t *testing.T) {
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("LoadEmbeddedCatalog: %v", err)
	}
	if cat == nil {
		t.Fatalf("nil catalog")
	}
}
