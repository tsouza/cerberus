package logql_test

import (
	"context"
	"strings"
	"testing"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerUnpackPattern_NoSQLImpact pins the contract that `| pattern`
// is a post-fetch stage: it extracts labels in Go after the rows
// return, so the lowered SQL contains exactly the same predicates as
// the equivalent query without the parser stage. A downstream label
// filter on an ORDINARY label name still gets a SQL predicate too (the
// structured-metadata/stream-label fallback — structuredOrStreamLookup
// in lower.go), which is what the "before label filter" case below
// pins; only a filter on the `__error__` / `__error_details__` family
// loses SQL visibility (see
// TestLowerUnpackPattern_ErrorLabelFilterDeferredToGo below and
// lowerPipelineWithLabels's dynamicLabels gate) since those keys are
// never legitimately present via that fallback.
//
// `| unpack` is NOT in this family: its extraction is modelled in SQL,
// so a filter that follows it resolves against the extracted map — see
// TestLowerUnpack_FiltersResolveAgainstExtractedLabels.
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

// TestLowerUnpackPattern_ErrorLabelFilterDeferredToGo pins the fix for
// the bug the loki-unpack-corpus-coverage compat corpus caught (#1611):
// `| __error__=""` / `| __error__="JSONParserErr"` following `| pattern`
// used to still get the structured-metadata SQL fallback pushed down —
// a silent no-op, since `__error__` never legitimately exists in
// LogAttributes/ResourceAttributes: the "" comparison matched every row
// and the non-empty comparison matched none, incorrectly excluding rows
// before the pattern step's Go-side extraction ever ran (see
// internal/api/loki/post_process.go's newLabelFilterStep, which applies
// these filters instead). A filter on an ORDINARY label name is
// unaffected — see TestLowerUnpackPattern_NoSQLImpact.
func TestLowerUnpackPattern_ErrorLabelFilterDeferredToGo(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	cases := []string{
		`{job="api"} | pattern "<_> <level> <msg>" | __error__=""`,
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			got := emitSQL(t, q, s)
			bare := emitSQL(t, `{job="api"}`, s)
			if got != bare {
				t.Errorf("expected the __error__ filter to have no SQL impact once handled in Go instead\nwith filter: %s\nbare:        %s", got, bare)
			}
		})
	}
}

// TestLowerUnpack_FiltersResolveAgainstExtractedLabels pins the other
// half of the `| unpack` contract: because the stage's extraction is
// modelled in SQL, a label filter that follows it — including one on
// the `__error__` family, which only exists as unpack's own marker —
// lowers to a predicate over the extracted map rather than being
// deferred to Go or silently pushed at the wrong columns.
//
// The assertion is that the SQL DIFFERS from the same query without the
// filter. A filter that resolved against columns unpack never writes
// would collapse back to the unfiltered SQL (`__error__=""` matching
// every row), which is exactly the #1611 shape.
func TestLowerUnpack_FiltersResolveAgainstExtractedLabels(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	unfiltered := emitSQL(t, `{job="api"} | unpack`, s)
	cases := []string{
		`{job="api"} | unpack | level="error"`,
		`{job="api"} | unpack | __error__=""`,
		`{job="api"} | unpack | __error__="JSONParserErr"`,
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			got := emitSQL(t, q, s)
			if got == unfiltered {
				t.Errorf("filter after `| unpack` produced no SQL predicate\nwith filter: %s\nunfiltered:  %s", got, unfiltered)
			}
		})
	}
}

// TestLowerUnpackPattern_AcceptsAllParsers pins that `| json`, `| logfmt`,
// `| regexp` are lowered alongside `| unpack` / `| pattern`. Used to
// pin rejection of the json / regexp shapes; both are now supported.
func TestLowerUnpackPattern_AcceptsAllParsers(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	cases := []string{
		`{job="api"} | json`,
		`{job="api"} | logfmt`,
		`{job="api"} | regexp "(?P<status>\\d+)"`,
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			if _, err := logql.Lower(context.Background(), expr, s); err != nil {
				t.Errorf("expected Lower(%q) to succeed; got %v", q, err)
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
