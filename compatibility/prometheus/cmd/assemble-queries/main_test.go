package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAssemblesRepositoryCorpus(t *testing.T) {
	t.Parallel()

	output := filepath.Join(t.TempDir(), "queries.yml")
	var stderr bytes.Buffer
	code := run([]string{
		"-source", filepath.Join("..", "..", "query-corpus"),
		"-output", output,
	}, &stderr)
	if code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\ntest_cases:\n") {
		t.Fatal("assembled output has no test_cases document")
	}
}

func TestRunFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{name: "missing output", code: 2, want: "-output is required"},
		{
			name: "missing source",
			args: []string{"-source", filepath.Join(t.TempDir(), "missing"), "-output", filepath.Join(t.TempDir(), "out")},
			code: 1,
			want: "assemble PromQL compatibility corpus",
		},
		{name: "invalid flag", args: []string{"-no-such-flag"}, code: 2, want: "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			if got := run(tt.args, &stderr); got != tt.code {
				t.Fatalf("run() code = %d, want %d; stderr = %q", got, tt.code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("run() stderr = %q, want substring %q", stderr.String(), tt.want)
			}
		})
	}
}
