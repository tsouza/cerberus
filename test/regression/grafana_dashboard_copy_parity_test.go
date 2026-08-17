package regression

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This pins the drift #2042 surfaced when the error-rate panel landed in one
// copy but not the others. The cerberus self-observability dashboard exists
// in three places, and every one of them is hand-maintained:
//
//   - test/e2e/grafana/dashboards/cerberus.json — the k3d editable copy.
//   - test/e2e/k3s/grafana-dashboards.yaml — the k3d DEPLOYED copy, the same
//     JSON embedded as a ConfigMap literal. This is what Grafana serves in the
//     k3d stack; the file above is never mounted.
//   - test/e2e/grafana/compose/dashboards/cerberus.json — the compose copy,
//     mounted straight into Grafana by docker-compose.yml.
//
// Nothing generated any of them and nothing copies one to another, so the three
// drift silently and asymmetrically. The failure mode is specifically quiet:
// iterate-all-dashboards.spec.ts probes panel expressions read FROM DISK while
// the browser renders what the stack actually SERVES, so a panel added to the
// editable copy but not to the ConfigMap passes its own probe against a
// dashboard that does not contain it. An operator on k3d then sees a board that
// is missing the panel CI just declared green.
//
// These tests are the gate that comment asks for.
const (
	dashboardK3dPath     = "../../test/e2e/grafana/dashboards/cerberus.json"
	dashboardComposePath = "../../test/e2e/grafana/compose/dashboards/cerberus.json"
	dashboardConfigMap   = "../../test/e2e/k3s/grafana-dashboards.yaml"

	// configMapJSONKey is the ConfigMap data key whose block scalar holds the
	// dashboard, and configMapJSONIndent is the block's indentation. A YAML
	// block scalar under a `data:` mapping is indented one level past the key,
	// which at this file's two-space step is four columns.
	configMapJSONKey    = "cerberus.json: |"
	configMapJSONIndent = "    "

	// maxReportedDiffs bounds the failure message. A drifted pair usually
	// differs in one panel; printing every path in a wholesale rewrite would
	// bury the first, actionable one.
	maxReportedDiffs = 10
)

// TestK3dDashboardConfigMapMatchesItsSourceFile pins the k3d editable copy
// against the ConfigMap literal that is actually deployed. The relationship is
// byte-for-byte: the ConfigMap block is the source file with every line
// indented by configMapJSONIndent, which is what makes the drift mechanically
// checkable rather than a review convention.
func TestK3dDashboardConfigMapMatchesItsSourceFile(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile(dashboardK3dPath)
	if err != nil {
		t.Fatalf("read dashboard source %s: %v", dashboardK3dPath, err)
	}
	raw, err := os.ReadFile(dashboardConfigMap)
	if err != nil {
		t.Fatalf("read ConfigMap %s: %v", dashboardConfigMap, err)
	}

	embedded, err := configMapEmbeddedDashboard(string(raw))
	if err != nil {
		t.Fatalf("extract %s from %s: %v", configMapJSONKey, dashboardConfigMap, err)
	}

	if embedded == string(source) {
		return
	}

	// Report at the JSON level when both sides parse — a panel-level path is
	// far more actionable than a line number into an indented blob — and fall
	// back to the byte verdict when one side is not valid JSON.
	var srcDoc, embDoc any
	srcErr := json.Unmarshal(source, &srcDoc)
	embErr := json.Unmarshal([]byte(embedded), &embDoc)
	if srcErr == nil && embErr == nil {
		if diffs := jsonDiffPaths("", srcDoc, embDoc); len(diffs) > 0 {
			t.Errorf("%s is out of sync with %s at %d path(s):\n  - %s\n"+
				"The ConfigMap is the copy k3d actually serves. Re-embed the source file: "+
				"indent every line of %s by %d spaces and replace the `%s` block.",
				dashboardConfigMap, dashboardK3dPath, len(diffs),
				strings.Join(diffs, "\n  - "),
				dashboardK3dPath, len(configMapJSONIndent), configMapJSONKey)
			return
		}
	}

	t.Errorf("%s and %s carry the same JSON but differ byte-for-byte (formatting drift).\n"+
		"The ConfigMap block must be the source file indented by %d spaces, so the two stay diffable. "+
		"Re-embed the source file into the `%s` block.",
		dashboardConfigMap, dashboardK3dPath, len(configMapJSONIndent), configMapJSONKey)
}

// TestComposeAndK3dDashboardsCarryTheSamePanels pins the two mounted copies
// against each other. They are deliberately NOT byte-identical: each carries a
// `description` naming the data its own stack ships, and that is the only
// divergence the split justifies. Everything an operator reads — the panel set,
// their ids, titles, expressions, units and grid positions — has to be the
// same board on both substrates, or "it works on compose" stops meaning
// anything about k3d.
func TestComposeAndK3dDashboardsCarryTheSamePanels(t *testing.T) {
	t.Parallel()

	k3d := readDashboardBody(t, dashboardK3dPath)
	compose := readDashboardBody(t, dashboardComposePath)

	// The per-stack description is the sanctioned difference; assert it is
	// actually present on both rather than deleting a key that silently is not
	// there, which would let a dropped description pass as parity.
	for path, doc := range map[string]map[string]any{
		dashboardK3dPath:     k3d,
		dashboardComposePath: compose,
	} {
		desc, ok := doc["description"].(string)
		if !ok || strings.TrimSpace(desc) == "" {
			t.Fatalf("%s has no top-level description; the per-stack description is the one "+
				"sanctioned difference between the two copies and must exist on both", path)
		}
		delete(doc, "description")
	}

	if reflect.DeepEqual(k3d, compose) {
		return
	}
	diffs := jsonDiffPaths("", k3d, compose)
	t.Errorf("%s and %s diverge at %d path(s) outside the sanctioned `description`:\n  - %s\n"+
		"Both stacks provision the same board; a panel added to one must be added to the other "+
		"(and re-embedded into %s).",
		dashboardK3dPath, dashboardComposePath, len(diffs),
		strings.Join(diffs, "\n  - "), dashboardConfigMap)
}

func readDashboardBody(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dashboard %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse dashboard %s: %v", path, err)
	}
	return doc
}

// configMapEmbeddedDashboard returns the dashboard JSON carried by the
// configMapJSONKey block scalar, with the block indentation stripped. It errors
// rather than returning a partial document when the block is absent or a line
// is under-indented, so a restructured ConfigMap fails loudly instead of
// silently comparing an empty string.
func configMapEmbeddedDashboard(yaml string) (string, error) {
	lines := strings.Split(yaml, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == configMapJSONKey {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no `%s` block found", configMapJSONKey)
	}

	body := lines[start:]
	// A block scalar ends at the first line indented less than the block, or at
	// end of file. Trailing blank lines belong to the file, not the scalar.
	end := len(body)
	for i, line := range body {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, configMapJSONIndent) {
			end = i
			break
		}
	}
	body = body[:end]
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
	}
	if len(body) == 0 {
		return "", fmt.Errorf("`%s` block is empty", configMapJSONKey)
	}

	out := make([]string, len(body))
	for i, line := range body {
		out[i] = strings.TrimPrefix(line, configMapJSONIndent)
	}
	return strings.Join(out, "\n") + "\n", nil
}

// jsonDiffPaths walks two decoded JSON documents in lockstep and returns the
// paths at which they differ, capped at maxReportedDiffs. Paths are dotted for
// object keys and bracketed for array indices, and a panel's own `id` is
// appended to its index so the caller reads "panels[6] (id=8)" rather than an
// ordinal they would have to count out by hand.
func jsonDiffPaths(path string, a, b any) []string {
	var out []string
	appendDiff := func(p, msg string) {
		if len(out) < maxReportedDiffs {
			out = append(out, fmt.Sprintf("%s: %s", pathOrRoot(p), msg))
		}
	}

	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			appendDiff(path, fmt.Sprintf("object on one side, %s on the other", jsonKind(b)))
			return out
		}
		keys := make([]string, 0, len(av)+len(bv))
		seen := make(map[string]bool, len(av)+len(bv))
		for k := range av {
			keys, seen[k] = append(keys, k), true
		}
		for k := range bv {
			if !seen[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			ae, aok := av[k]
			be, bok := bv[k]
			switch {
			case aok && !bok:
				appendDiff(joinPath(path, k), "present on the first, missing on the second")
			case !aok && bok:
				appendDiff(joinPath(path, k), "missing on the first, present on the second")
			default:
				out = appendCapped(out, jsonDiffPaths(joinPath(path, k), ae, be))
			}
			if len(out) >= maxReportedDiffs {
				return out
			}
		}
		return out

	case []any:
		bv, ok := b.([]any)
		if !ok {
			appendDiff(path, fmt.Sprintf("array on one side, %s on the other", jsonKind(b)))
			return out
		}
		if len(av) != len(bv) {
			appendDiff(path, fmt.Sprintf("%d element(s) vs %d", len(av), len(bv)))
			return out
		}
		for i := range av {
			out = appendCapped(out, jsonDiffPaths(indexPath(path, i, av[i]), av[i], bv[i]))
			if len(out) >= maxReportedDiffs {
				return out
			}
		}
		return out

	default:
		if !reflect.DeepEqual(a, b) {
			appendDiff(path, fmt.Sprintf("%s vs %s", jsonScalar(a), jsonScalar(b)))
		}
		return out
	}
}

func appendCapped(dst, src []string) []string {
	for _, s := range src {
		if len(dst) >= maxReportedDiffs {
			return dst
		}
		dst = append(dst, s)
	}
	return dst
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// indexPath labels an array element by its position, plus the element's own
// `id` when it carries one — the dashboards' panel arrays are id-keyed, and the
// id is what a reader searches the JSON for.
func indexPath(path string, i int, elem any) string {
	label := fmt.Sprintf("%s[%d]", path, i)
	if obj, ok := elem.(map[string]any); ok {
		if id, ok := obj["id"]; ok {
			label += fmt.Sprintf(" (id=%s)", jsonScalar(id))
		}
	}
	return label
}

func pathOrRoot(path string) string {
	if path == "" {
		return "<root>"
	}
	return path
}

func jsonKind(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "null"
	default:
		return "scalar"
	}
}

func jsonScalar(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
