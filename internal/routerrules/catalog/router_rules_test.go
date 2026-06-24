package catalog_test

import (
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"

	"github.com/tsouza/cerberus/internal/routerrules"
)

// TestEmbeddedCatalogHasNoNumbers is the reviewer-facing proof of the
// no-numbers invariant: it walks EVERY embedded catalog file (the base plus each
// rules/<rule_id>.yaml) and asserts that no scalar that lives in a param or
// condition value position is a number. This is belt-and-suspenders over the
// structural guard (the condition AST has no number-literal node) and the
// load-time validator — it points at exactly what ships and fails the build the
// instant a deployment-specific number is smuggled into any catalog file.
func TestEmbeddedCatalogHasNoNumbers(t *testing.T) {
	files, err := routerrules.EmbeddedCatalogFiles()
	if err != nil {
		t.Fatalf("read embedded catalog files: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no embedded catalog files found")
	}

	var offenders []string
	sawParams := false
	sawRules := false
	for _, f := range files {
		var root yaml.Node
		if err := yaml.Unmarshal(f.Bytes, &root); err != nil {
			t.Fatalf("unmarshal embedded catalog file %q: %v", f.Name, err)
		}
		if len(root.Content) == 0 {
			continue
		}
		// apiVersion + catalogVersion are schema-shape contract scalars, not
		// deployment parameters; they are allowed to be numeric. Everything
		// under params: and rules: in any file must be number-free.
		doc := root.Content[0]
		if params := mappingValue(doc, "params"); params != nil {
			sawParams = true
			walkScalars(params, f.Name+":params", &offenders)
		}
		if rules := mappingValue(doc, "rules"); rules != nil {
			sawRules = true
			walkScalars(rules, f.Name+":rules", &offenders)
		}
	}
	if !sawParams {
		t.Fatalf("no params section found across embedded catalog files")
	}
	if !sawRules {
		t.Fatalf("no rules section found across embedded catalog files")
	}
	if len(offenders) > 0 {
		t.Fatalf("the shipped catalog contains %d numeric scalar(s) in param/condition positions — every threshold must be a named parameter, not a literal:\n%s",
			len(offenders), strings.Join(offenders, "\n"))
	}
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// structuralMetadataKeys are catalog-internal version markers, not deployment
// parameters or condition values. They carry a small integer (a revision
// counter), so the no-numbers walk skips them: the invariant forbids numbers in
// THRESHOLD positions, not in the rule's own provenance metadata.
var structuralMetadataKeys = map[string]struct{}{
	"since": {},
}

// walkScalars descends a YAML subtree and records any scalar that parses as a
// number (int or float). Mapping KEYS are skipped (they are field names like
// "percentile", never values); only VALUE scalars are checked. Structural
// metadata keys (see structuralMetadataKeys) are skipped wholesale.
func walkScalars(n *yaml.Node, path string, offenders *[]string) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			if _, skip := structuralMetadataKeys[k.Value]; skip {
				continue
			}
			walkScalars(v, path+"."+k.Value, offenders)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			walkScalars(c, path+"["+itoa(i)+"]", offenders)
		}
	case yaml.ScalarNode:
		if isNumericScalar(n) {
			*offenders = append(*offenders, "  "+path+" = "+n.Value)
		}
	}
}

// isNumericScalar reports whether a scalar node is a YAML number. A quoted
// string that happens to contain digits (e.g. "max(memory_usage)") is NOT a
// number — only an unquoted/plain scalar whose tag resolves to !!int or !!float
// counts. yaml.v3 sets the resolved tag on plain scalars.
func isNumericScalar(n *yaml.Node) bool {
	if n.Style == yaml.DoubleQuotedStyle || n.Style == yaml.SingleQuotedStyle {
		return false
	}
	switch n.Tag {
	case "!!int", "!!float":
		return true
	default:
		return false
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
