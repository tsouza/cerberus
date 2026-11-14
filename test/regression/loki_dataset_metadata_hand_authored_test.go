package regression

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/compatibility/loki/corpus"
)

// This file pins the answer to #1876: `compatibility/loki/dataset_metadata.json`
// is a hand-authored artefact, not a generated one, and `.gitattributes`
// records it that way.
//
// The proof is in the file itself: it carries a `_comment` key recording how a
// human derived each selector from the seeder's deterministic fixture, and
// `corpus.Metadata`, the struct the harness decodes the file into, does not
// model that key. A marshal round trip through the struct would therefore not
// merely rewrite the file — it would silently delete the provenance that makes
// the file reviewable, while succeeding. Nothing in the tree writes the file;
// the guard below fails if a regeneration ever drops the key, and if the struct
// grows a field for it (which would make the file machine-writable and
// contradict the hand-authored record).

const (
	// lokiDatasetMetadataPath is the curated file.
	lokiDatasetMetadataPath = "compatibility/loki/" + corpus.MetadataFileName

	// handAuthoredProvenanceKey is the curated key no struct field models, and
	// therefore the key a marshal round trip drops without erroring.
	handAuthoredProvenanceKey = "_comment"
)

// TestHandAuthoredLokiMetadataOutlivesTheMarshaller asserts the curated
// provenance key is present AND that the decoding struct cannot round-trip
// it, which is the same fact stated from both ends: the file holds something
// no generator can reproduce.
func TestHandAuthoredLokiMetadataOutlivesTheMarshaller(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(lokiDatasetMetadataPath)))
	if err != nil {
		t.Fatalf("read %s: %v", lokiDatasetMetadataPath, err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", lokiDatasetMetadataPath, err)
	}

	provenance, ok := doc[handAuthoredProvenanceKey]
	if !ok {
		t.Fatalf("%s has lost its %q key.\n"+
			"That key records how a human derived the pinned selectors from the\n"+
			"seeder's deterministic fixture, and no code writes it. Its absence\n"+
			"means the file was regenerated through a marshaller that drops every\n"+
			"key corpus.Metadata does not model. Restore the curated content from\n"+
			"git history; do not re-derive it.",
			lokiDatasetMetadataPath, handAuthoredProvenanceKey)
	}

	var comment string
	if err := json.Unmarshal(provenance, &comment); err != nil {
		t.Fatalf("%s: %q must be a string, got %s", lokiDatasetMetadataPath, handAuthoredProvenanceKey, provenance)
	}
	if strings.TrimSpace(comment) == "" {
		t.Fatalf("%s: %q is empty, so it records no provenance", lokiDatasetMetadataPath, handAuthoredProvenanceKey)
	}

	ty := reflect.TypeFor[corpus.Metadata]()
	if ty.NumField() == 0 {
		t.Fatal("corpus.Metadata declares no fields, so this test proves nothing")
	}
	for i := range ty.NumField() {
		name, _, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ",")
		if name == handAuthoredProvenanceKey {
			t.Fatalf("corpus.Metadata.%s now models %q.\n"+
				"That makes the file machine-writable, which contradicts the\n"+
				"hand-authored record for %s in .gitattributes. Reconcile the two:\n"+
				"either revert the struct change, or reclassify the file as\n"+
				"generated and give it a regeneration command.",
				ty.Field(i).Name, handAuthoredProvenanceKey, lokiDatasetMetadataPath)
		}
	}
}
