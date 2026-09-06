package regression

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// justDependency is one entry of a recipe's `dependencies` array in
// `just --dump --dump-format json` output — the OTHER recipes it composes
// (e.g. `test: test-unit test-chaos-sleep vet-tagged`).
type justDependency struct {
	Recipe    string   `json:"recipe"`
	Arguments []string `json:"arguments"`
}

// justParameter is one positional parameter a recipe declares
// (`e2e-bwc-up scenario="object-storage"`, `route-rules *ARGS`).
type justParameter struct {
	Name    string  `json:"name"`
	Kind    string  `json:"kind"` // "singular" | "plus" | "star"
	Default *string `json:"default"`
}

// justRecipeDump is one entry of `just --dump`'s `recipes` map. `Body` is a
// list of lines, each line a list of fragments: a plain JSON string is
// literal text, and everything else is an interpolation expression — see
// renderBodySyntax().
type justRecipeDump struct {
	Name         string              `json:"name"`
	Doc          *string             `json:"doc"`
	Body         [][]json.RawMessage `json:"body"`
	Dependencies []justDependency    `json:"dependencies"`
	Parameters   []justParameter     `json:"parameters"`
	Private      bool                `json:"private"`
}

// dumpAssignment is one entry of `just --dump`'s top-level `assignments`
// map. `Value` is either a plain JSON string (`NAME := "literal"`) or a
// tagged expression array (`["variable", ...]`, `["concatenate", ...]`,
// `["call", "env_var_or_default", ...]`) — see renderJustValueOK().
type dumpAssignment struct {
	Name    string          `json:"name"`
	Export  bool            `json:"export"`
	Private bool            `json:"private"`
	Value   json.RawMessage `json:"value"`
}

// justDumpDoc is the top-level shape of `just --dump --dump-format json`
// this package reads from. The real output carries more top-level keys
// (aliases, groups, modules, settings, source, warnings); only the two
// every current regression check needs are declared here — encoding/json
// ignores the rest.
type justDumpDoc struct {
	Recipes     map[string]justRecipeDump `json:"recipes"`
	Assignments map[string]dumpAssignment `json:"assignments"`
}

// justDump runs `just --dump --dump-format json` from the repo root and
// parses it.
//
// This is the fix (#3093) for every regression check that used to assume
// the Justfile was one physical file and read it as raw text to find a
// recipe's body, doc, dependencies, or parameters: `import` merges every
// just/*.just file into ONE flat `recipes` map here, keyed by name
// regardless of which physical file declares it — verified live against
// this repo's own just/*.just split, not merely trusted from the design
// doc. What this does NOT carry is a source file:line for anything, or the
// full multi-line comment block above a recipe (only the already-collapsed
// last-line `doc` survives) — the two regression checks that genuinely need
// either one read justfileSources() instead (see that file's doc comment).
func justDump(t *testing.T) justDumpDoc {
	t.Helper()
	cmd := exec.Command("just", "--dump", "--dump-format", "json")
	cmd.Dir = "../.." // test/regression -> repo root, where the Justfile lives
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("just --dump --dump-format json: %v\n%s", err, out)
	}
	var d justDumpDoc
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatalf("parse `just --dump --dump-format json` output: %v", err)
	}
	if len(d.Recipes) == 0 {
		t.Fatal("just --dump --dump-format json reported zero recipes — the dump command itself is broken, not the recipe set")
	}
	return d
}

// recipe looks up one recipe by name, failing the test loudly (not
// returning a zero value) when it is missing — the caller's whole point is
// usually to assert something ABOUT that recipe, and a silently-empty
// justRecipeDump would make every subsequent assertion pass vacuously.
func (d justDumpDoc) recipe(t *testing.T, name string) justRecipeDump {
	t.Helper()
	r, ok := d.Recipes[name]
	if !ok {
		t.Fatalf("just --dump reports no recipe %q", name)
	}
	return r
}

// assignment looks up one top-level variable assignment by name, same
// fail-loud contract as recipe().
func (d justDumpDoc) assignment(t *testing.T, name string) dumpAssignment {
	t.Helper()
	a, ok := d.Assignments[name]
	if !ok {
		t.Fatalf("just --dump reports no assignment %q", name)
	}
	return a
}

// bodyText reconstructs a recipe's body exactly as it reads in source: each
// line's fragments are literal text interleaved with interpolation
// expressions, which are rejoined here as `{{NAME}}` / `{{fn(...)}}` so every
// existing regex or substring check written against the ORIGINAL Justfile
// text keeps working unchanged against the reconstruction. Unlike
// stringValue()/resolvedStringAssignments(), a `call` expression (`just
// _executable()`, `env_var_or_default(...)`) IS rendered here — reconstructed
// back to its call SYNTAX, not resolved to a runtime value, since that is
// exactly what a text-shaped check on a recipe body wants to see.
func (r justRecipeDump) bodyText(t *testing.T) string {
	t.Helper()
	lines := make([]string, len(r.Body))
	for i, frags := range r.Body {
		var b strings.Builder
		for _, frag := range frags {
			s, ok := renderBodySyntax(frag)
			if !ok {
				t.Fatalf("recipe %q: unrecognised body fragment %s", r.Name, frag)
			}
			b.WriteString(s)
		}
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n")
}

// dependencyNames returns just the recipe names a composite recipe chains
// to, in declaration order (`test: test-unit test-chaos-sleep vet-tagged`
// -> ["test-unit", "test-chaos-sleep", "vet-tagged"]).
func (r justRecipeDump) dependencyNames() []string {
	names := make([]string, len(r.Dependencies))
	for i, d := range r.Dependencies {
		names[i] = d.Recipe
	}
	return names
}

// transitiveDependencyNames returns every recipe name reached from `recipe`,
// including `recipe` itself, walking `dependencies` recursively — "does
// recipe A reach recipe B" as a real graph query rather than a
// `strings.Contains(body, "B:")` substring proxy (which relied on a raw
// Justfile scan including each visited recipe's own header line; bodyText()
// deliberately does not, since it reconstructs BODY content only).
func (d justDumpDoc) transitiveDependencyNames(t *testing.T, recipe string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, dep := range d.recipe(t, name).dependencyNames() {
			walk(dep)
		}
	}
	walk(recipe)
	return seen
}

// doc returns the recipe's single-line description (the `just --list`
// summary), or "" if it has none.
func (r justRecipeDump) doc() string {
	if r.Doc == nil {
		return ""
	}
	return *r.Doc
}

// stringValue resolves an assignment to a plain string, recursively
// evaluating a `concatenate` of literals (`MIGRATION_TIER2_SERVICES`'s
// shape) the same way `just` itself would. Fails loudly on a value that
// depends on something this process cannot know without running `just`
// itself (an env-derived `call`) — no current caller's assignment has that
// shape, and a silently-wrong placeholder would be worse than a clear
// failure pointing at the one that does.
func (a dumpAssignment) stringValue(t *testing.T) string {
	t.Helper()
	s, ok := renderJustValueOK(a.Value)
	if !ok {
		t.Fatalf("assignment %q: cannot render %s to a plain string", a.Name, a.Value)
	}
	return s
}

// recipeNames returns every recipe name `just --dump` reports, private
// (leading `_`) ones included — the full set a raw `\bjust\s+(\S+)` scan
// used to have to derive from the Justfile's own text.
func (d justDumpDoc) recipeNames() map[string]bool {
	names := make(map[string]bool, len(d.Recipes))
	for name := range d.Recipes {
		names[name] = true
	}
	return names
}

// resolvedStringAssignments returns every top-level assignment that resolves
// to a plain string (a literal, or a `concatenate` of literals) — skipping,
// not failing on, the handful that are genuinely dynamic (`env_var_or_default`
// calls: E2E_MODE, CERBERUS_BUILD_TAGS, K3D_EXTRA_ARGS). A caller that reads
// ONE specific assignment by name and needs it to resolve should use
// assignment(t, name).stringValue(t) instead, which fails loudly when that
// one does not.
func (d justDumpDoc) resolvedStringAssignments() map[string]string {
	out := make(map[string]string, len(d.Assignments))
	for name, a := range d.Assignments {
		if s, ok := renderJustValueOK(a.Value); ok {
			out[name] = s
		}
	}
	return out
}

// renderJustValueOK is the non-fatal core: it renders what it recognises and
// reports false on what it does not, rather than failing outright — a
// value that depends on something this process cannot know without running
// `just` itself (an env-derived `call`) is a real, occasional shape here,
// not a bug in this renderer, and a caller iterating EVERY assignment (see
// resolvedStringAssignments) needs to skip just that one rather than crash
// on it.
//
// A JSON string is literal text as-is. A JSON array is either:
//   - a bare tagged expression `[tag, ...args]` (an assignment's `value`,
//     or a `concatenate` argument) — tag is a JSON string; or
//   - a body fragment's interpolation wrapper: a list of one or more such
//     tagged expressions, each rendered and concatenated (a recipe body
//     fragment's second-and-later elements, per the dump's own nesting).
//
// Distinguished by peeking at the first element: a JSON string there means
// THIS array is the tagged expression; a JSON array there means this array
// is the wrapper.
func renderJustValueOK(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}

	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return "", false
	}
	if len(arr) == 0 {
		return "", true
	}

	var tag string
	if err := json.Unmarshal(arr[0], &tag); err == nil {
		return renderTaggedExprOK(tag, arr[1:])
	}

	var b strings.Builder
	for _, item := range arr {
		s, ok := renderJustValueOK(item)
		if !ok {
			return "", false
		}
		b.WriteString(s)
	}
	return b.String(), true
}

// renderTaggedExprOK renders one `[tag, ...args]` just expression to its
// RESOLVED VALUE. Only "variable" (rendered back to `{{NAME}}`, since a
// caller resolving one assignment's value may still be holding an unresolved
// reference to another) and "concatenate" are recognised; "call" (an
// env-derived default, or a builtin like `just_executable()`) and anything
// else report false rather than guess at a runtime value this process does
// not have. Use renderBodySyntax instead when reconstructing a recipe BODY's
// source text, where a `call` should round-trip as syntax, not fail.
func renderTaggedExprOK(tag string, args []json.RawMessage) (string, bool) {
	switch tag {
	case "variable":
		var name string
		if err := json.Unmarshal(args[0], &name); err != nil {
			return "", false
		}
		return "{{" + name + "}}", true
	case "concatenate":
		var b strings.Builder
		for _, a := range args {
			s, ok := renderJustValueOK(a)
			if !ok {
				return "", false
			}
			b.WriteString(s)
		}
		return b.String(), true
	default:
		return "", false
	}
}

// renderBodySyntax renders one recipe-body fragment back to the source
// syntax it reads as — the body-reconstruction counterpart of
// renderJustValueOK, which resolves an assignment to its VALUE instead. The
// two agree on "variable" and "concatenate"; they diverge on "call" (a
// builtin/function invocation like `just_executable()` or
// `env_var_or_default(NAME, default)`), which a body reconstruction renders
// back to `{{fn(arg, ...)}}` call syntax rather than refusing, since nothing
// here is trying to know the runtime value — only to reproduce the text a
// substring or regex check was written against.
func renderBodySyntax(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}

	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return "", false
	}
	if len(arr) == 0 {
		return "", true
	}

	var tag string
	if err := json.Unmarshal(arr[0], &tag); err == nil {
		return renderBodyTaggedExpr(tag, arr[1:])
	}

	var b strings.Builder
	for _, item := range arr {
		s, ok := renderBodySyntax(item)
		if !ok {
			return "", false
		}
		b.WriteString(s)
	}
	return b.String(), true
}

func renderBodyTaggedExpr(tag string, args []json.RawMessage) (string, bool) {
	switch tag {
	case "variable":
		var name string
		if err := json.Unmarshal(args[0], &name); err != nil {
			return "", false
		}
		return "{{" + name + "}}", true
	case "concatenate":
		var b strings.Builder
		for _, a := range args {
			s, ok := renderBodySyntax(a)
			if !ok {
				return "", false
			}
			b.WriteString(s)
		}
		return b.String(), true
	case "call":
		if len(args) == 0 {
			return "", false
		}
		var fname string
		if err := json.Unmarshal(args[0], &fname); err != nil {
			return "", false
		}
		parts := make([]string, 0, len(args)-1)
		for _, a := range args[1:] {
			// A bare string argument is a quoted literal in source
			// (`env_var_or_default("NAME", "default")`) — re-quote it rather
			// than rendering it as bare text. Anything else (a nested
			// expression) renders generically.
			var lit string
			if err := json.Unmarshal(a, &lit); err == nil {
				parts = append(parts, strconv.Quote(lit))
				continue
			}
			s, ok := renderBodySyntax(a)
			if !ok {
				return "", false
			}
			parts = append(parts, s)
		}
		return "{{" + fname + "(" + strings.Join(parts, ", ") + ")}}", true
	default:
		return "", false
	}
}
