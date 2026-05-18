package logql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerUnpackPattern_NoSQLImpact pins the contract that `| unpack`
// and `| pattern` are post-fetch stages: they extract labels in Go
// after the rows return, so the lowered SQL contains exactly the same
// predicates as the equivalent query without the parser stage.
//
// This mirrors the existing decolorize / line_format / label_format
// stages — see internal/logql/lower.go for the dispatch.
func TestLowerUnpackPattern_NoSQLImpact(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	cases := []struct {
		name string
		// `with` includes the parser stage; `without` strips it. Both
		// should lower to the same SQL.
		with    string
		without string
	}{
		{
			name:    "unpack on bare selector",
			with:    `{job="api"} | unpack`,
			without: `{job="api"}`,
		},
		{
			name:    "unpack after line filter",
			with:    `{job="api"} |= "packed" | unpack`,
			without: `{job="api"} |= "packed"`,
		},
		{
			name:    "unpack before label filter",
			with:    `{job="api"} | unpack | level="error"`,
			without: `{job="api"} | level="error"`,
		},
		{
			name:    "pattern on bare selector",
			with:    `{job="api"} | pattern "<ip> <_> <method> <path>"`,
			without: `{job="api"}`,
		},
		{
			name:    "pattern after line filter",
			with:    `{job="api"} |= "GET" | pattern "<ip> <_> <method> <path>"`,
			without: `{job="api"} |= "GET"`,
		},
		{
			name:    "pattern before label filter",
			with:    `{job="api"} | pattern "<_> <level> <msg>" | level="error"`,
			without: `{job="api"} | level="error"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotWith := emitSQL(t, tc.with, s)
			gotWithout := emitSQL(t, tc.without, s)
			if gotWith != gotWithout {
				t.Errorf("parser stage altered SQL\nwith stage:    %s\nwithout stage: %s",
					gotWith, gotWithout)
			}
		})
	}
}

// TestLowerUnpackPattern_StillRejectsOtherParsers pins that we didn't
// accidentally light up `| json` / `| regexp` while enabling unpack +
// pattern (and `| logfmt`) — those stay deferred to RC3.
func TestLowerUnpackPattern_StillRejectsOtherParsers(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	cases := []string{
		`{job="api"} | json`,
		`{job="api"} | regexp "(?P<status>\\d+)"`,
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			if _, err := logql.Lower(context.Background(), expr, s); err == nil {
				t.Errorf("expected Lower(%q) to error; got nil", q)
			}
		})
	}
}

// TestParserMalformedPattern — Loki's upstream parser is responsible
// for rejecting malformed patterns (it panics inside ParseExpr with a
// ParseError). Cerberus relies on that; the test pins the contract so
// we notice if upstream relaxes it.
func TestParserMalformedPattern(t *testing.T) {
	t.Parallel()

	// `bar` has no captures — invalid pattern per Loki's own rules.
	_, err := syntax.ParseExpr(`{job="api"} | pattern "bar"`)
	if err == nil {
		t.Fatalf("expected ParseExpr to reject `| pattern \"bar\"`; got nil error")
	}
	if !strings.Contains(err.Error(), "pattern") {
		t.Errorf("error message should mention `pattern`; got: %v", err)
	}
}

func emitSQL(t *testing.T, q string, s schema.Logs) string {
	t.Helper()
	expr, err := syntax.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	plan, err := logql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower(%q): %v", q, err)
	}
	sqlStr, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", q, err)
	}
	return sqlStr
}
