package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestScan runs the AST walker against a synthetic fixture under
// testdata/violations/ and verifies the expected violations land on
// the expected lines. The fixture is shipped as a .txt file so the Go
// toolchain ignores it during regular builds; the test copies it into
// a temp tree as a .go file before scanning.
func TestScan(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "violations", "violations.go.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	tmp := t.TempDir()
	// Mirror the scan-root prefixes so both walkers see the file.
	// scanSprintf walks {internal,cmd,harness}; scanWriteCalls walks
	// {internal/chsql,internal/api}. Drop the fixture under
	// internal/chsql so both apply.
	dir := filepath.Join(tmp, "internal", "chsql")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dst := filepath.Join(dir, "violations.go")
	if err := os.WriteFile(dst, src, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	al := &allowlist{
		files:  map[string]bool{},
		ranges: map[string][][2]int{},
	}
	var got []violation
	got = append(got, scanSprintf(scanDirsSprintf, al)...)
	got = append(got, scanWriteCalls(scanDirsWrite, al)...)

	// Build a set of (line) pairs that were flagged. The fixture is
	// stable; if the layout shifts, update the expected line numbers
	// below.
	lines := make([]int, 0, len(got))
	for _, v := range got {
		lines = append(lines, v.line)
	}
	sort.Ints(lines)

	want := []int{
		21, // sprintfHit: SELECT
		25, // fprintfHit: INSERT
		30, // WriteString("SELECT ")
		31, // WriteString(" FROM t")
		32, // WriteString(" WHERE x = 1")
		33, // WriteString(" GROUP BY g")
		34, // WriteString(" ORDER BY t")
		35, // WriteString(" LIMIT 10")
		36, // WriteString(" PREWHERE p")
		37, // WriteString(" INNER JOIN r ON x = y")
		43, // WriteSQL(" UNION ALL")
		44, // WriteSQL(" WITH RECURSIVE c AS (SELECT 1)")
	}
	if len(lines) != len(want) {
		t.Fatalf("violation count: got %d (%v), want %d (%v)", len(lines), lines, len(want), want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("violation[%d]: got line %d, want %d (all=%v)", i, lines[i], want[i], lines)
		}
	}

	// Spot-check the legit-token rows do NOT appear.
	for _, v := range got {
		if v.line >= 48 && v.line <= 55 {
			t.Errorf("legit-token row %d flagged: %s", v.line, v.msg)
		}
		if v.line == 60 {
			t.Errorf("legit non-SQL Sprintf flagged at line %d: %s", v.line, v.msg)
		}
	}
}

func TestAllowlistExempt(t *testing.T) {
	al := &allowlist{
		files:  map[string]bool{"a/b/c.go": true},
		ranges: map[string][][2]int{"d/e.go": {{10, 20}, {30, 30}}},
	}
	cases := []struct {
		file string
		line int
		want bool
	}{
		{"a/b/c.go", 1, true},
		{"a/b/c.go", 999, true},
		{"d/e.go", 5, false},
		{"d/e.go", 15, true},
		{"d/e.go", 25, false},
		{"d/e.go", 30, true},
		{"d/e.go", 31, false},
		{"other.go", 1, false},
	}
	for _, tc := range cases {
		if got := al.exempt(tc.file, tc.line); got != tc.want {
			t.Errorf("exempt(%s, %d) = %v, want %v", tc.file, tc.line, got, tc.want)
		}
	}
}

func TestLoadAllowlist(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "allowlist.txt")
	content := `# comment
internal/foo.go
internal/bar.go:42
internal/baz.go:10-20  # trailing comment
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	al, err := loadAllowlist(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !al.files["internal/foo.go"] {
		t.Error("whole-file entry missing")
	}
	if !al.exempt("internal/bar.go", 42) || al.exempt("internal/bar.go", 41) {
		t.Error("single-line entry mis-applied")
	}
	if !al.exempt("internal/baz.go", 15) || al.exempt("internal/baz.go", 21) {
		t.Error("range entry mis-applied")
	}
}
