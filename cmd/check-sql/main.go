// Command check-sql enforces the "no clause-keyword cosplay" rule from
// CLAUDE.md: SQL clause keywords (SELECT / FROM / WHERE / GROUP BY /
// ORDER BY / LIMIT / PREWHERE / HAVING / UNION / WITH RECURSIVE / JOIN
// variants) must not appear inside fmt.Sprintf / fmt.Fprintf format
// strings nor inside Builder.WriteString / Builder.WriteSQL calls.
//
// The only legitimate site is chsql.QueryBuilder.writeInto in
// internal/chsql/builder.go — the typed renderer that backs every
// SELECT statement. Every other emission must compose through typed
// QueryBuilder slots (Select / From / Where / GroupBy / OrderBy /
// Limit / Prewhere / Join / WithRecursive).
//
// Exemptions live in cmd/check-sql/allowlist.txt. The format is one
// entry per line:
//
//	path                — exempt the whole file
//	path:line           — exempt a single line
//	path:start-end      — exempt an inclusive line range
//	#…                  — comment
//	(blank)             — ignored
//
// The tool exits with status 1 if any violation is found. Output
// uses the standard `<file>:<line>:<col>: <message>` format so
// editors and GitHub Actions annotate the violations automatically.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// forbiddenSprintf is the clause-keyword set checked against
// fmt.Sprintf / fmt.Fprintf format strings. Match is case-insensitive
// on a substring basis. A trailing space anchors the keyword as a
// clause (e.g. "SELECT name" vs the column literal "SELECT").
var forbiddenSprintf = []string{
	"SELECT ",
	"FROM ",
	"WHERE ",
	"INSERT ",
	"UPDATE ",
	"DELETE ",
}

// forbiddenWrite is the clause-keyword set checked against
// Builder.WriteString / Builder.WriteSQL string literals. Match is
// case-sensitive substring (callers spell the keyword in upper case
// per CH convention) and uses a leading space to distinguish the
// clause keyword from incidental occurrences inside operator glue.
var forbiddenWrite = []string{
	"SELECT",
	" FROM",
	" WHERE",
	" GROUP BY",
	" ORDER BY",
	" LIMIT",
	" PREWHERE",
	" HAVING",
	" UNION",
	" WITH RECURSIVE",
	" INNER JOIN",
	" LEFT JOIN",
	" RIGHT JOIN",
	" CROSS JOIN",
	" FULL JOIN",
}

// scanDirsSprintf is the prefix list walked for fmt.Sprintf / Fprintf
// violations.
var scanDirsSprintf = []string{"internal", "cmd", "harness"}

// scanDirsWrite is the (narrower) prefix list walked for WriteString /
// WriteSQL clause-keyword violations.
var scanDirsWrite = []string{"internal/chsql", "internal/api"}

type allowlist struct {
	files  map[string]bool
	ranges map[string][][2]int
}

func main() {
	var allowlistPath string
	flag.StringVar(&allowlistPath, "allowlist", "cmd/check-sql/allowlist.txt",
		"path to allowlist file")
	flag.Parse()

	al, err := loadAllowlist(allowlistPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-sql: load allowlist %s: %v\n", allowlistPath, err)
		os.Exit(2)
	}

	var violations []violation
	violations = append(violations, scanSprintf(scanDirsSprintf, al)...)
	violations = append(violations, scanWriteCalls(scanDirsWrite, al)...)

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		if violations[i].line != violations[j].line {
			return violations[i].line < violations[j].line
		}
		return violations[i].col < violations[j].col
	})

	for _, v := range violations {
		fmt.Fprintf(os.Stdout, "%s:%d:%d: %s\n", v.file, v.line, v.col, v.msg)
	}
	if len(violations) > 0 {
		os.Exit(1)
	}
}

type violation struct {
	file string
	line int
	col  int
	msg  string
}

func loadAllowlist(path string) (*allowlist, error) {
	al := &allowlist{
		files:  map[string]bool{},
		ranges: map[string][][2]int{},
	}
	f, err := os.Open(path) //nolint:gosec // G304: allowlist path is a CLI argument, not user input
	if err != nil {
		if os.IsNotExist(err) {
			return al, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// strip trailing inline comment
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			al.files[filepath.ToSlash(line)] = true
			continue
		}
		file := filepath.ToSlash(line[:colon])
		spec := line[colon+1:]
		if dash := strings.Index(spec, "-"); dash >= 0 {
			a, err1 := strconv.Atoi(spec[:dash])
			b, err2 := strconv.Atoi(spec[dash+1:])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("bad range %q", line)
			}
			al.ranges[file] = append(al.ranges[file], [2]int{a, b})
			continue
		}
		n, err := strconv.Atoi(spec)
		if err != nil {
			return nil, fmt.Errorf("bad line %q", line)
		}
		al.ranges[file] = append(al.ranges[file], [2]int{n, n})
	}
	return al, sc.Err()
}

func (a *allowlist) exempt(file string, line int) bool {
	file = filepath.ToSlash(file)
	if a.files[file] {
		return true
	}
	for _, r := range a.ranges[file] {
		if line >= r[0] && line <= r[1] {
			return true
		}
	}
	return false
}

func walkGoFiles(roots []string, fn func(path string) error) error {
	for _, root := range roots {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			return fn(path)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func scanSprintf(roots []string, al *allowlist) []violation {
	var out []violation
	_ = walkGoFiles(roots, func(path string) error {
		out = append(out, checkFile(path, al, checkSprintfCall)...)
		return nil
	})
	return out
}

func scanWriteCalls(roots []string, al *allowlist) []violation {
	var out []violation
	_ = walkGoFiles(roots, func(path string) error {
		out = append(out, checkFile(path, al, checkWriteCall)...)
		return nil
	})
	return out
}

type checkFn func(fset *token.FileSet, call *ast.CallExpr) (string, bool)

func checkFile(path string, al *allowlist, check checkFn) []violation {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var out []violation
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		msg, hit := check(fset, call)
		if !hit {
			return true
		}
		pos := fset.Position(call.Lparen)
		if al.exempt(path, pos.Line) {
			return true
		}
		out = append(out, violation{file: path, line: pos.Line, col: pos.Column, msg: msg})
		return true
	})
	return out
}

func checkSprintfCall(_ *token.FileSet, call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "fmt" {
		return "", false
	}
	if sel.Sel.Name != "Sprintf" && sel.Sel.Name != "Fprintf" {
		return "", false
	}
	if len(call.Args) == 0 {
		return "", false
	}
	idx := 0
	if sel.Sel.Name == "Fprintf" {
		if len(call.Args) < 2 {
			return "", false
		}
		idx = 1
	}
	lit, ok := call.Args[idx].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	val, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	upper := strings.ToUpper(val)
	for _, kw := range forbiddenSprintf {
		if strings.Contains(upper, kw) {
			return fmt.Sprintf(
				"fmt.%s builds SQL containing %q; use chsql.QueryBuilder slots instead (see docs/chsql-audit.md)",
				sel.Sel.Name, strings.TrimSpace(kw),
			), true
		}
	}
	return "", false
}

func checkWriteCall(_ *token.FileSet, call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if sel.Sel.Name != "WriteString" && sel.Sel.Name != "WriteSQL" {
		return "", false
	}
	if len(call.Args) == 0 {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	val, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	for _, kw := range forbiddenWrite {
		if strings.Contains(val, kw) {
			return fmt.Sprintf(
				"%s emits clause keyword %q; route through chsql.QueryBuilder slots (.Select/.From/.Where/.GroupBy/.OrderBy/.Limit/.Prewhere/.Join/.WithRecursive)",
				sel.Sel.Name, strings.TrimSpace(kw),
			), true
		}
	}
	return "", false
}
