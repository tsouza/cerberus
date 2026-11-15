package logql_test

import (
	"reflect"
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestPatternLineFilter_LowersThePatternUpToInvalidUTF8 pins that a `|>` /
// `!>` pattern holding an invalid UTF-8 sequence (reachable through a `\x`
// escape) lowers to the SQL of the pattern that ends there: the same query
// text and arguments as the pattern written without the sequence and what
// follows it.
func TestPatternLineFilter_LowersThePatternUpToInvalidUTF8(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	cases := []struct {
		query, truncated string
	}{
		// The text after the sequence would have made the pattern end on
		// a capture; the truncated pattern ends on its literal.
		{`{job="api"} |> "a\xff<_>"`, `{job="api"} |> "a"`},
		{`{job="api"} !> "a\xff<_>"`, `{job="api"} !> "a"`},
		{`{job="api"} |> "<_> b\xff<_>c"`, `{job="api"} |> "<_> b"`},
		// The text after the sequence would have made the pattern end on
		// a literal; the truncated pattern ends on its capture.
		{`{job="api"} |> "a <_>\xff z"`, `{job="api"} |> "a <_>"`},
		{`{job="api"} |> "<_>\xffz"`, `{job="api"} |> "<_>"`},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			t.Parallel()
			gotSQL, wantSQL := emitSQL(t, c.query, s), emitSQL(t, c.truncated, s)
			if gotSQL != wantSQL {
				t.Fatalf("SQL of %s:\n%s\nwant the SQL of %s:\n%s", c.query, gotSQL, c.truncated, wantSQL)
			}
			if got, want := emitArgs(t, c.query, s), emitArgs(t, c.truncated, s); !reflect.DeepEqual(got, want) {
				t.Fatalf("arguments of %s = %v, want those of %s = %v", c.query, got, c.truncated, want)
			}
		})
	}
}
