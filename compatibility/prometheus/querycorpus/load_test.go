package querycorpus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimumRepositoryCases = 287

func TestLoadRepositoryCorpus(t *testing.T) {
	t.Parallel()

	raw, cases, err := Load(filepath.Join("..", "query-corpus"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# Cerberus-side curated PromQL test queries") {
		t.Fatal("assembled corpus lost its policy header")
	}
	if len(cases) < minimumRepositoryCases {
		t.Fatalf("assembled corpus shrank to %d cases; floor is %d", len(cases), minimumRepositoryCases)
	}
}

func TestLoadOrdersFragmentsDeterministically(t *testing.T) {
	t.Parallel()

	dir := newCorpus(t, []string{"000-scalars.yml", "001-functions.yml"}, map[string]string{
		"001-functions.yml": "  - query: 'vector(1)'\n",
		"000-scalars.yml":   "  - query: '1'\n",
	})
	_, cases, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{cases[0].Query, cases[1].Query}; got[0] != "1" || got[1] != "vector(1)" {
		t.Fatalf("case order = %v, want manifest order", got)
	}
}

func TestLoadFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		manifest  []string
		fragments map[string]string
		mutate    func(t *testing.T, dir string)
		want      string
	}{
		{
			name:     "omitted fragment",
			manifest: []string{"000-scalars.yml", "001-functions.yml"},
			fragments: map[string]string{
				"000-scalars.yml": "  - query: '1'\n",
			},
			want: "roster mismatch",
		},
		{
			name:     "unlisted fragment",
			manifest: []string{"000-scalars.yml"},
			fragments: map[string]string{
				"000-scalars.yml":   "  - query: '1'\n",
				"001-functions.yml": "  - query: 'vector(1)'\n",
			},
			want: "roster mismatch",
		},
		{
			name:     "noncanonical order",
			manifest: []string{"001-functions.yml", "000-scalars.yml"},
			fragments: map[string]string{
				"000-scalars.yml":   "  - query: '1'\n",
				"001-functions.yml": "  - query: 'vector(1)'\n",
			},
			want: "index 001, want 000",
		},
		{
			name:     "duplicate case",
			manifest: []string{"000-scalars.yml", "001-functions.yml"},
			fragments: map[string]string{
				"000-scalars.yml":   "  - query: '1'\n",
				"001-functions.yml": "  - query: '1'\n",
			},
			want: "duplicates test case",
		},
		{
			name:     "empty fragment",
			manifest: []string{"000-scalars.yml"},
			fragments: map[string]string{
				"000-scalars.yml": "\n",
			},
			want: "is empty",
		},
		{
			name:     "malformed yaml",
			manifest: []string{"000-scalars.yml"},
			fragments: map[string]string{
				"000-scalars.yml": "  - query: [\n",
			},
			want: "decode assembled corpus",
		},
		{
			name:     "unexpected directory",
			manifest: []string{"000-scalars.yml"},
			fragments: map[string]string{
				"000-scalars.yml": "  - query: '1'\n",
			},
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(dir, fragmentsDirname, "nested"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "unexpected directory",
		},
		{
			name:      "hollow corpus",
			manifest:  []string{"000-scalars.yml"},
			fragments: map[string]string{"000-scalars.yml": "  # no cases\n"},
			want:      "contains no test cases",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := newCorpus(t, tt.manifest, tt.fragments)
			if tt.mutate != nil {
				tt.mutate(t, dir)
			}
			_, _, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func newCorpus(t *testing.T, manifest []string, fragments map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	fragmentsDir := filepath.Join(dir, fragmentsDirname)
	if err := os.Mkdir(fragmentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, headerFilename), []byte("# test\ntest_cases:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFilename), []byte(strings.Join(manifest, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, contents := range fragments {
		if err := os.WriteFile(filepath.Join(fragmentsDir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
