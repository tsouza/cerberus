package regression

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fixedPinsSectionHeading is the docs/operations.md heading of the section
// that lists every distributed-read pin cerberus stamps and states that none
// of them can be overridden. The section is located by this line, so prose
// around it can move freely.
const fixedPinsSectionHeading = "#### Distributed-read pins are fixed"

// settingsProfilePhrase is the lever the fixed-pins section rules out. It
// may appear only inside that section, which explains why a settings
// profile cannot override a pin; anywhere else in the files that describe
// the pins it would be offering the lever again.
const settingsProfilePhrase = "settings profile"

// fixedPinBulletRE matches one bullet of the fixed-pins section:
// "- `name=value`".
var fixedPinBulletRE = regexp.MustCompile("^- `([a-z_]+=[a-z_0-9]+)`$")

// TestDistributedReadPinsDocumentedAsFixed keeps the documented claim about
// cerberus's per-query distributed-read pins in agreement with the code
// that stamps them (issue #3815). Three places used to tell operators they
// could override a pin "via a server-side settings profile"; a setting sent
// with the query beats the profile's default, and a profile constraint makes
// every cerberus query fail, so that lever never existed.
//
// It pins two things:
//   - the bullet list under docs/operations.md's fixed-pins section names
//     exactly the name=value pairs Client.querySettings stamps
//     unconditionally (read from source with go/ast, so a pin added,
//     removed, or re-valued without a doc edit fails here);
//   - no other file that describes the pins mentions a settings profile.
func TestDistributedReadPinsDocumentedAsFixed(t *testing.T) {
	root := repoRootForParity(t)
	fromCode := unconditionalQuerySettingsFromSource(t, filepath.Join(root, "internal", "chclient"))
	if len(fromCode) == 0 {
		t.Fatal("Client.querySettings: no unconditional settings parsed from source")
	}

	opsPath := filepath.Join(root, "docs", "operations.md")
	section, outside := splitMarkdownSection(t, opsPath, fixedPinsSectionHeading)
	var fromDoc []string
	for line := range strings.SplitSeq(section, "\n") {
		if m := fixedPinBulletRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			fromDoc = append(fromDoc, m[1])
		}
	}
	slices.Sort(fromDoc)
	if !slices.Equal(fromDoc, fromCode) {
		t.Errorf("docs/operations.md %q lists %v; Client.querySettings stamps %v", fixedPinsSectionHeading, fromDoc, fromCode)
	}

	described := map[string]string{
		"docs/operations.md (outside the fixed-pins section)": outside,
	}
	for _, rel := range []string{
		"internal/chclient/distributed_query_settings.go",
		"internal/chclient/client.go",
		"docs/helm-clickhouse.md",
		"docs/helm-clickhouse.background.md",
	} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		described[rel] = string(b)
	}
	for name, text := range described {
		if strings.Contains(normalizeProse(text), settingsProfilePhrase) {
			t.Errorf("%s mentions a %s; the distributed-read pins cannot be overridden that way — describe them as fixed and link docs/operations.md %q",
				name, settingsProfilePhrase, fixedPinsSectionHeading)
		}
	}
}

// normalizeProse lower-cases text, drops Go line-comment markers, and
// collapses every whitespace run to one space, so a phrase wrapped across
// lines of a Markdown paragraph or a Go comment block still matches.
func normalizeProse(text string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.ToLower(text), "//", " ")), " ")
}

// splitMarkdownSection returns the body of the section whose heading line is
// exactly heading (up to the next heading of the same or a higher level),
// and the rest of the file with that section removed.
func splitMarkdownSection(t *testing.T, path, heading string) (section, outside string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	level := strings.Index(heading, " ")
	lines := strings.Split(string(b), "\n")
	start := slices.Index(lines, heading)
	if start < 0 {
		t.Fatalf("%s: heading %q not found", path, heading)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		hashes := len(lines[i]) - len(strings.TrimLeft(lines[i], "#"))
		if hashes > 0 && hashes <= level && strings.HasPrefix(lines[i][hashes:], " ") {
			end = i
			break
		}
	}
	section = strings.Join(lines[start+1:end], "\n")
	outside = strings.Join(slices.Concat(lines[:start], lines[end:]), "\n")
	return section, outside
}

// unconditionalQuerySettingsFromSource reads the `pins := clickhouse.Settings{…}`
// literal of Client.querySettings — the settings it stamps on every query,
// over any per-request setting of the same name — and renders each entry as a sorted
// "name=value" string, resolving constant names against the package's own
// string constants.
func unconditionalQuerySettingsFromSource(t *testing.T, pkgDir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("read %s: %v", pkgDir, err)
	}
	consts := map[string]string{}
	var querySettings *ast.FuncDecl
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(pkgDir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							v, err := strconv.Unquote(lit.Value)
							if err != nil {
								t.Fatalf("unquote %s: %v", name.Name, err)
							}
							consts[name.Name] = v
						}
					}
				}
			case *ast.FuncDecl:
				if d.Name.Name == "querySettings" && d.Recv != nil {
					querySettings = d
				}
			}
		}
	}
	if querySettings == nil {
		t.Fatalf("%s: method querySettings not found", pkgDir)
	}

	resolve := func(e ast.Expr) string {
		switch v := e.(type) {
		case *ast.Ident:
			c, ok := consts[v.Name]
			if !ok {
				t.Fatalf("querySettings: %s is not a string constant of package chclient", v.Name)
			}
			return c
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				s, err := strconv.Unquote(v.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", v.Value, err)
				}
				return s
			}
			return v.Value
		}
		t.Fatalf("querySettings: unsupported settings-literal expression %T", e)
		return ""
	}

	var out []string
	for _, stmt := range querySettings.Body.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			continue
		}
		if id, ok := assign.Lhs[0].(*ast.Ident); !ok || id.Name != "pins" {
			continue
		}
		lit, ok := assign.Rhs[0].(*ast.CompositeLit)
		if !ok {
			continue
		}
		for _, elt := range lit.Elts {
			kv := elt.(*ast.KeyValueExpr)
			out = append(out, resolve(kv.Key)+"="+resolve(kv.Value))
		}
		break
	}
	slices.Sort(out)
	return out
}
