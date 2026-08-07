package regression

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This file pins the answer to #1876: is `compatibility/loki/dataset_metadata.json`
// a generated artefact or a hand-authored one?
//
// The question had two mutually exclusive answers sitting side by side in the
// tree. `.gitattributes` records the file as hand-authored. The vendored bench
// package exports `SaveMetadata(dataDir, *DatasetMetadata)`, which writes a file
// named by `metadataFileName` — and that constant is the byte-identical string
// `dataset_metadata.json`. Point that writer at `compatibility/loki/` and it
// overwrites the curated file.
//
// The file is hand-authored. The proof is in the file itself: it carries a
// `_comment` key recording how a human derived each selector from the seeder's
// deterministic fixture, and `DatasetMetadata` does not model that key at all.
// A `SaveMetadata` round trip therefore does not merely rewrite the file — it
// silently deletes the provenance that makes the file reviewable, and does so
// while succeeding.
//
// The writer is NOT deleted, and that is deliberate. `metadata.go` is a verbatim
// AGPL-3.0 snapshot of grafana/loki's `pkg/logql/bench`, pinned by
// `compatibility/loki/upstream/loki-bench/VERSION` and re-copied wholesale by the
// bump procedure in `compatibility/loki/README.md`. `SaveMetadata` is upstream's
// code, not cerberus's dead code; it is unused only in the subset cerberus
// vendors. Deleting it would diverge the snapshot from its tag for a cosmetic
// reason, edit AGPL code the repository quarantines rather than maintains, and
// be silently undone at the next bump — a fix that deletes itself.
//
// So the ambiguity is closed on the cerberus side, where cerberus owns the code
// and where the guard survives re-vendoring: the curated key must still be there,
// and no cerberus-licensed code may reach for the writer.

const (
	// lokiBenchVendorRoot is the verbatim AGPL snapshot. Everything under it is
	// upstream's to define; the guard below is about what cerberus CALLS.
	lokiBenchVendorRoot = "compatibility/loki/upstream/loki-bench/"

	// lokiDatasetMetadataPath is the curated file `SaveMetadata` would clobber.
	lokiDatasetMetadataPath = "compatibility/loki/dataset_metadata.json"

	// lokiBenchMetadataSource declares `DatasetMetadata` and `SaveMetadata`.
	lokiBenchMetadataSource = lokiBenchVendorRoot + "metadata.go"

	// handAuthoredProvenanceKey is the curated key no struct field models, and
	// therefore the key a marshal round trip drops without erroring.
	handAuthoredProvenanceKey = "_comment"

	// datasetMetadataTypeDecl opens the struct whose json tags define exactly
	// what survives a `SaveMetadata` round trip.
	datasetMetadataTypeDecl = "type DatasetMetadata struct {"
)

// vendoredWriterName is the writer this guard keeps unwired. A reach for it is
// a qualified selector — `bench.SaveMetadata`, called or passed as a value —
// because cerberus code lives outside package `bench` and cannot name the
// writer without a qualifier.
const vendoredWriterName = "SaveMetadata"

// jsonStructTag captures the wire name from a struct field tag.
var jsonStructTag = regexp.MustCompile(`json:"([^",]+)`)

// TestHandAuthoredLokiMetadataOutlivesTheVendoredMarshaller pins the evidence
// that settles #1876. It asserts the curated provenance key is present AND that
// the vendored struct cannot round-trip it, which is the same fact stated from
// both ends: the file holds something no generator can reproduce.
//
// It fails in both directions on purpose. Regenerate the file with the vendored
// writer and the key vanishes — first assertion fires. Teach `DatasetMetadata` a
// `_comment` field and the file stops being hand-authored evidence — second
// assertion fires, and whoever did it has to reclassify the file in
// `.gitattributes` rather than let the two records drift apart again.
func TestHandAuthoredLokiMetadataOutlivesTheVendoredMarshaller(t *testing.T) {
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
			"means the file was regenerated — most likely through\n"+
			"bench.SaveMetadata, which names the same filename and drops every\n"+
			"key DatasetMetadata does not model. Restore the curated content from\n"+
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

	for _, tag := range datasetMetadataWireNames(t) {
		if tag == handAuthoredProvenanceKey {
			t.Fatalf("DatasetMetadata in %s now models %q.\n"+
				"That makes the file machine-writable, which contradicts the\n"+
				"hand-authored record for %s in .gitattributes. Reconcile the two:\n"+
				"either revert the struct change, or reclassify the file as\n"+
				"generated and give it a regeneration command.",
				lokiBenchMetadataSource, handAuthoredProvenanceKey, lokiDatasetMetadataPath)
		}
	}
}

// namesVendoredWriter reports whether the Go source in body reaches for the
// vendored writer through a qualified selector.
//
// The question is answered from the parsed syntax rather than from a text
// match, because a text match cannot tell a reach from a mention. The
// distinction is not academic: this very file names `bench.SaveMetadata` in
// the failure message below, so a regexp over the bytes reports the guard as
// its own first violator. Comments and string literals are not selector
// expressions, so deriving the answer from the AST puts prose out of scope by
// construction rather than by asking every future author to phrase around a
// pattern.
func namesVendoredWriter(t *testing.T, file string, body []byte) bool {
	t.Helper()

	parsed, err := parser.ParseFile(token.NewFileSet(), file, body, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	found := false
	ast.Inspect(parsed, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != vendoredWriterName {
			return !found
		}
		if _, ok := sel.X.(*ast.Ident); ok {
			found = true
		}
		return !found
	})
	return found
}

// TestNoCerberusCodeReachesForTheVendoredMetadataWriter keeps the destructive
// call unwired. The vendored writer is harmless while nothing calls it; the
// moment cerberus code does, a curated file becomes a generated one without
// anyone deciding that, and the loss is silent because the write succeeds.
func TestNoCerberusCodeReachesForTheVendoredMetadataWriter(t *testing.T) {
	t.Parallel()

	var callers []string
	for _, file := range trackedFiles(t) {
		if !strings.HasSuffix(file, ".go") || strings.HasPrefix(file, lokiBenchVendorRoot) {
			continue
		}

		body, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(file)))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if namesVendoredWriter(t, file, body) {
			callers = append(callers, file)
		}
	}

	if len(callers) > 0 {
		t.Fatalf("cerberus code now calls the vendored bench metadata writer:\n  %s\n\n"+
			"bench.SaveMetadata writes %q into the directory it is handed, and\n"+
			"%s is exactly that file, pinned by hand. The write marshals\n"+
			"DatasetMetadata, which does not model the curated %q key, so the call\n"+
			"succeeds and deletes the provenance in the same breath.\n\n"+
			"If the intent is to generate this dataset, that is a real design\n"+
			"change: give the file a generation command, record it as generated in\n"+
			".gitattributes, and reproduce the provenance the curated file carries.\n"+
			"If the intent was to reuse the bench types, load with bench.LoadMetadata\n"+
			"and never write back.",
			strings.Join(callers, "\n  "),
			"dataset_metadata.json", lokiDatasetMetadataPath, handAuthoredProvenanceKey)
	}
}

// datasetMetadataWireNames returns every json tag name declared by the vendored
// DatasetMetadata struct — precisely the set of keys a SaveMetadata round trip
// preserves. Anything in the curated file outside this set is lost on write.
func datasetMetadataWireNames(t *testing.T) []string {
	t.Helper()

	source, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(lokiBenchMetadataSource)))
	if err != nil {
		t.Fatalf("read %s: %v", lokiBenchMetadataSource, err)
	}

	_, after, found := strings.Cut(string(source), datasetMetadataTypeDecl)
	if !found {
		t.Fatalf("%s no longer declares %q.\n"+
			"The struct is the reference for what a SaveMetadata round trip keeps.\n"+
			"If the vendored bench package renamed or removed it, re-read whether\n"+
			"the writer can still target %s at all, and re-pin this test against\n"+
			"whatever replaced it.",
			lokiBenchMetadataSource, datasetMetadataTypeDecl, lokiDatasetMetadataPath)
	}

	body, _, closed := strings.Cut(after, "\n}")
	if !closed {
		t.Fatalf("%s: %q is not closed by a line-leading brace; cannot read its fields",
			lokiBenchMetadataSource, datasetMetadataTypeDecl)
	}

	matches := jsonStructTag.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatalf("%s: %q declares no json tags, so this test proves nothing.\n"+
			"Either the struct changed shape or the tag pattern stopped matching it.",
			lokiBenchMetadataSource, datasetMetadataTypeDecl)
	}

	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}

	return names
}
