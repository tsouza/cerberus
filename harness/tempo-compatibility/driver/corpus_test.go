package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCorpus_Basic(t *testing.T) {
	t.Parallel()
	in := `# leading comment
-- name --
attr_eq
-- query --
{ resource.service.name = "checkout" }
-- endpoint --
search
-- expected_min_traces --
1
-- expected_max_traces --
200
-- expected_services --
checkout
-- name --
status_error
-- query --
{ status = error }
-- endpoint --
search
-- expected_min_traces --
0
-- skip_reason --
metrics endpoint
`
	got, err := parseCorpus(strings.NewReader(in), "test")
	if err != nil {
		t.Fatalf("parseCorpus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 cases, got %d", len(got))
	}
	if got[0].Name != "attr_eq" || got[0].Query != `{ resource.service.name = "checkout" }` {
		t.Fatalf("case[0] = %+v", got[0])
	}
	if got[0].Endpoint != "search" {
		t.Fatalf("case[0] endpoint = %q", got[0].Endpoint)
	}
	if got[0].ExpectedMinTraces != 1 || got[0].ExpectedMaxTraces != 200 {
		t.Fatalf("case[0] bounds = %d..%d", got[0].ExpectedMinTraces, got[0].ExpectedMaxTraces)
	}
	if len(got[0].ExpectedServices) != 1 || got[0].ExpectedServices[0] != "checkout" {
		t.Fatalf("case[0] services = %v", got[0].ExpectedServices)
	}
	if got[1].SkipReason != "metrics endpoint" {
		t.Fatalf("case[1] skip = %q", got[1].SkipReason)
	}
}

func TestParseCorpus_DefaultEndpointIsSearch(t *testing.T) {
	t.Parallel()
	in := `-- name --
default_ep
-- query --
{ duration > 100ms }
`
	got, err := parseCorpus(strings.NewReader(in), "test")
	if err != nil {
		t.Fatalf("parseCorpus: %v", err)
	}
	if got[0].Endpoint != "search" {
		t.Fatalf("expected default endpoint = search, got %q", got[0].Endpoint)
	}
}

func TestParseCorpus_TracesEndpointRequiresTemplate(t *testing.T) {
	t.Parallel()
	in := `-- name --
trace_by_id
-- query --
{ }
-- endpoint --
traces
`
	if _, err := parseCorpus(strings.NewReader(in), "test"); err == nil {
		t.Fatal("expected error: traces endpoint without traceid_template")
	}
}

func TestParseCorpus_TracesEndpointWithTemplateOK(t *testing.T) {
	t.Parallel()
	in := `-- name --
trace_by_id
-- query --
{ }
-- endpoint --
traces
-- traceid_template --
checkout/0
`
	got, err := parseCorpus(strings.NewReader(in), "test")
	if err != nil {
		t.Fatalf("parseCorpus: %v", err)
	}
	if got[0].TraceIDTemplate != "checkout/0" {
		t.Fatalf("traceid_template = %q", got[0].TraceIDTemplate)
	}
}

func TestParseCorpus_RootNameRECompiles(t *testing.T) {
	t.Parallel()
	in := `-- name --
re
-- query --
{ resource.service.name = "checkout" }
-- expected_root_name_re --
^GET /api/[a-z]+/[0-9]+$
`
	got, err := parseCorpus(strings.NewReader(in), "test")
	if err != nil {
		t.Fatalf("parseCorpus: %v", err)
	}
	if got[0].ExpectedRootNameRE == nil {
		t.Fatal("expected_root_name_re did not compile")
	}
	if !got[0].ExpectedRootNameRE.MatchString("GET /api/checkout/3") {
		t.Fatalf("regex did not match expected fixture root name")
	}
}

func TestParseCorpus_BadRegexFails(t *testing.T) {
	t.Parallel()
	in := `-- name --
re
-- query --
{ }
-- expected_root_name_re --
[unclosed
`
	if _, err := parseCorpus(strings.NewReader(in), "test"); err == nil {
		t.Fatal("expected error on bad regex")
	}
}

func TestParseCorpus_UnknownSectionFails(t *testing.T) {
	t.Parallel()
	in := `-- name --
x
-- query --
{ }
-- bogus --
y
`
	if _, err := parseCorpus(strings.NewReader(in), "test"); err == nil {
		t.Fatal("expected error on unknown section")
	}
}

func TestParseCorpus_EmptyFails(t *testing.T) {
	t.Parallel()
	if _, err := parseCorpus(strings.NewReader("# only comments\n"), "test"); err == nil {
		t.Fatal("expected error on empty corpus")
	}
}

func TestParseCorpus_ContentBeforeFirstHeaderFails(t *testing.T) {
	t.Parallel()
	in := `not a header
-- name --
x
-- query --
{ }
`
	if _, err := parseCorpus(strings.NewReader(in), "test"); err == nil {
		t.Fatal("expected error on content before first header")
	}
}

func TestParseCorpus_UnknownEndpointFails(t *testing.T) {
	t.Parallel()
	in := `-- name --
x
-- query --
{ }
-- endpoint --
bogus
`
	if _, err := parseCorpus(strings.NewReader(in), "test"); err == nil {
		t.Fatal("expected error on unknown endpoint")
	}
}

func TestSmokeCorpus_LoadsAndMeetsFloor(t *testing.T) {
	t.Parallel()
	cases, err := LoadCorpus(filepath.Join("corpus", "smoke.txtar"))
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	if len(cases) < 20 {
		t.Fatalf("smoke corpus shrunk: got %d, want >= 20", len(cases))
	}
	// Every active case has a non-empty query.
	for _, c := range cases {
		if c.SkipReason != "" {
			continue
		}
		if c.Query == "" && c.Endpoint != "search_recent" {
			t.Fatalf("case %q: empty query but endpoint=%q", c.Name, c.Endpoint)
		}
	}
}
