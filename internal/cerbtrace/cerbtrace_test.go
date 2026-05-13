package cerbtrace

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestTruncate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		in     string
		maxLen int
		want   string
	}{
		{"empty", "", 10, ""},
		{"shorter than max", "abc", 10, "abc"},
		{"exact max", "abcdefghij", 10, "abcdefghij"},
		{"truncated with ellipsis", strings.Repeat("a", 20), 10, strings.Repeat("a", 7) + "…"},
		{"max smaller than ellipsis falls back to byte clip", "abcdef", 2, "ab"},
		{"zero max returns input", "abc", 0, "abc"},
		{"negative max returns input", "abc", -1, "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Truncate(tc.in, tc.maxLen)
			if got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.maxLen, got, tc.want)
			}
			if tc.maxLen > 0 && len(got) > tc.maxLen {
				t.Errorf("Truncate output %q has %d bytes, exceeds maxLen=%d", got, len(got), tc.maxLen)
			}
		})
	}
}

func TestCountNodes(t *testing.T) {
	t.Parallel()

	// Scan: 1 node
	if got := CountNodes(&chplan.Scan{Table: "t"}); got != 1 {
		t.Errorf("Scan: got %d, want 1", got)
	}
	// Filter(Scan): 2 nodes
	plan := chplan.Node(&chplan.Filter{
		Input:     &chplan.Scan{Table: "t"},
		Predicate: &chplan.LitBool{V: true},
	})
	if got := CountNodes(plan); got != 2 {
		t.Errorf("Filter(Scan): got %d, want 2", got)
	}
	// nil safety
	if got := CountNodes(nil); got != 0 {
		t.Errorf("nil: got %d, want 0", got)
	}
}

func TestParseAttrs(t *testing.T) {
	t.Parallel()

	attrs := ParseAttrs("promql", "up{job=\"api\"}")
	if len(attrs) != 2 {
		t.Fatalf("ParseAttrs len = %d, want 2", len(attrs))
	}
	if attrs[0].Key != AttrQL || attrs[0].Value.AsString() != "promql" {
		t.Errorf("ParseAttrs[0] = %v, want cerberus.ql=promql", attrs[0])
	}
	if attrs[1].Key != AttrQuery {
		t.Errorf("ParseAttrs[1] key = %v, want cerberus.query", attrs[1].Key)
	}

	// Long queries truncate
	long := strings.Repeat("a", MaxQueryLen+10)
	attrs = ParseAttrs("logql", long)
	if got := attrs[1].Value.AsString(); len(got) > MaxQueryLen {
		t.Errorf("ParseAttrs cerberus.query length = %d, want <= %d", len(got), MaxQueryLen)
	}
}
