package shadow

import (
	"strings"
	"testing"
)

func TestParseCorpus_Basic(t *testing.T) {
	t.Parallel()
	in := `# smoke
-- query --
up
-- query --
rate(http_requests_total[5m])
-- expected_strategy --
prefer-native
`
	got, err := parseCorpus(strings.NewReader(in), "test")
	if err != nil {
		t.Fatalf("parseCorpus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 queries, got %d", len(got))
	}
	if got[0].Expr != "up" {
		t.Fatalf("q0 expr = %q, want %q", got[0].Expr, "up")
	}
	if got[1].ExpectedStrategy != "prefer-native" {
		t.Fatalf("q1 strategy = %q, want %q", got[1].ExpectedStrategy, "prefer-native")
	}
}

func TestParseCorpus_EmptyFails(t *testing.T) {
	t.Parallel()
	if _, err := parseCorpus(strings.NewReader("# only comments\n"), "test"); err == nil {
		t.Fatal("expected error on empty corpus")
	}
}

func TestParseCorpus_ContentBeforeHeaderFails(t *testing.T) {
	t.Parallel()
	if _, err := parseCorpus(strings.NewReader("up\n-- query --\nup\n"), "test"); err == nil {
		t.Fatal("expected error on content before first header")
	}
}

func TestParseCorpus_UnknownSectionFails(t *testing.T) {
	t.Parallel()
	in := "-- query --\nup\n-- bogus --\nx\n"
	if _, err := parseCorpus(strings.NewReader(in), "test"); err == nil {
		t.Fatal("expected error on unknown section")
	}
}
