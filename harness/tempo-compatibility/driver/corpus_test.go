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
	// Every active case has a non-empty query unless the endpoint is one
	// of the "no TraceQL" kinds (search_recent + the four tag endpoints).
	for _, c := range cases {
		if c.SkipReason != "" {
			continue
		}
		if c.Query == "" && c.Endpoint != "search_recent" && !isTagEndpoint(c.Endpoint) {
			t.Fatalf("case %q: empty query but endpoint=%q", c.Name, c.Endpoint)
		}
	}
}

func TestSmokeCorpus_TagEndpointCoverage(t *testing.T) {
	t.Parallel()
	cases, err := LoadCorpus(filepath.Join("corpus", "smoke.txtar"))
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	// PR 6 commits the four tag endpoints. Every endpoint kind must
	// have at least one active case in the smoke corpus so the differ
	// exercises the full response-shape matrix on every nightly run.
	required := map[string]bool{
		"tags_v1":       false,
		"tags_v2":       false,
		"tag_values_v1": false,
		"tag_values_v2": false,
	}
	for _, c := range cases {
		if c.SkipReason != "" {
			continue
		}
		if _, ok := required[c.Endpoint]; ok {
			required[c.Endpoint] = true
		}
	}
	for ep, seen := range required {
		if !seen {
			t.Errorf("smoke corpus lacks an active case for endpoint=%q", ep)
		}
	}
}

func TestParseCorpus_TagValuesRequiresTagName(t *testing.T) {
	t.Parallel()
	for _, ep := range []string{"tag_values_v1", "tag_values_v2"} {
		in := `-- name --
x
-- endpoint --
` + ep + `
`
		if _, err := parseCorpus(strings.NewReader(in), "test"); err == nil {
			t.Fatalf("endpoint=%s without tag_name should fail", ep)
		}
	}
}

func TestParseCorpus_TagValuesWithTagNameOK(t *testing.T) {
	t.Parallel()
	in := `-- name --
tv
-- endpoint --
tag_values_v1
-- tag_name --
service.name
-- expected_values --
checkout
payments
`
	got, err := parseCorpus(strings.NewReader(in), "test")
	if err != nil {
		t.Fatalf("parseCorpus: %v", err)
	}
	if got[0].TagName != "service.name" {
		t.Fatalf("tag_name = %q", got[0].TagName)
	}
	if len(got[0].ExpectedValues) != 2 ||
		got[0].ExpectedValues[0] != "checkout" || got[0].ExpectedValues[1] != "payments" {
		t.Fatalf("expected_values = %v", got[0].ExpectedValues)
	}
}

func TestParseCorpus_TagsV2WithScopeOK(t *testing.T) {
	t.Parallel()
	in := `-- name --
tg
-- endpoint --
tags_v2
-- scope --
resource
-- expected_scopes --
resource
-- expected_min_values --
1
-- expected_max_values --
500
`
	got, err := parseCorpus(strings.NewReader(in), "test")
	if err != nil {
		t.Fatalf("parseCorpus: %v", err)
	}
	if got[0].Scope != "resource" {
		t.Fatalf("scope = %q", got[0].Scope)
	}
	if len(got[0].ExpectedScopes) != 1 || got[0].ExpectedScopes[0] != "resource" {
		t.Fatalf("expected_scopes = %v", got[0].ExpectedScopes)
	}
	if got[0].ExpectedMinValues != 1 || got[0].ExpectedMaxValues != 500 {
		t.Fatalf("min/max values = %d..%d", got[0].ExpectedMinValues, got[0].ExpectedMaxValues)
	}
}

func TestParseCorpus_ScopeOnlyValidForTagsV2(t *testing.T) {
	t.Parallel()
	in := `-- name --
bad
-- query --
{ x = 1 }
-- endpoint --
search
-- scope --
resource
`
	if _, err := parseCorpus(strings.NewReader(in), "test"); err == nil {
		t.Fatal("scope on a non-tags_v2 case should fail")
	}
}

func TestParseCorpus_MetricsRangeSections(t *testing.T) {
	t.Parallel()
	in := `-- name --
metrics_rate
-- query --
{ } | rate()
-- endpoint --
metrics_range
-- step --
60s
-- expected_min_series --
1
-- expected_max_series --
10
-- expected_samples_per_series --
5
-- semantic_checks --
samples_non_negative
groupby_labels_present:resource.service.name
`
	got, err := parseCorpus(strings.NewReader(in), "test")
	if err != nil {
		t.Fatalf("parseCorpus: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 case, got %d", len(got))
	}
	c := got[0]
	if c.Endpoint != "metrics_range" {
		t.Fatalf("Endpoint = %q", c.Endpoint)
	}
	if c.Step != "60s" {
		t.Fatalf("Step = %q", c.Step)
	}
	if c.ExpectedMinSeries != 1 || c.ExpectedMaxSeries != 10 {
		t.Fatalf("series bounds = %d..%d", c.ExpectedMinSeries, c.ExpectedMaxSeries)
	}
	if c.ExpectedSamplesPerSeries != 5 {
		t.Fatalf("ExpectedSamplesPerSeries = %d", c.ExpectedSamplesPerSeries)
	}
	if len(c.SemanticChecks) != 2 {
		t.Fatalf("SemanticChecks = %v", c.SemanticChecks)
	}
	if c.SemanticChecks[0] != "samples_non_negative" {
		t.Fatalf("SemanticChecks[0] = %q", c.SemanticChecks[0])
	}
	if c.SemanticChecks[1] != "groupby_labels_present:resource.service.name" {
		t.Fatalf("SemanticChecks[1] = %q", c.SemanticChecks[1])
	}
}

func TestParseCorpus_MetricsRangeRequiresStep(t *testing.T) {
	t.Parallel()
	in := `-- name --
no_step
-- query --
{ } | rate()
-- endpoint --
metrics_range
`
	if _, err := parseCorpus(strings.NewReader(in), "test"); err == nil {
		t.Fatal("expected error: metrics_range without step")
	}
}

func TestParseCorpus_MetricsRangeSkipReasonBypassesStep(t *testing.T) {
	t.Parallel()
	// skip_reason'd metrics_range cases don't need step (the case won't
	// run; the step omission is just a corpus-author convenience).
	in := `-- name --
skipped
-- query --
{ } | rate()
-- endpoint --
metrics_range
-- skip_reason --
not implemented
`
	got, err := parseCorpus(strings.NewReader(in), "test")
	if err != nil {
		t.Fatalf("parseCorpus: %v", err)
	}
	if got[0].SkipReason != "not implemented" {
		t.Fatalf("SkipReason = %q", got[0].SkipReason)
	}
}

func TestParseCorpus_MetricsInstantOK(t *testing.T) {
	t.Parallel()
	in := `-- name --
instant
-- query --
{ } | rate()
-- endpoint --
metrics_instant
`
	got, err := parseCorpus(strings.NewReader(in), "test")
	if err != nil {
		t.Fatalf("parseCorpus: %v", err)
	}
	if got[0].Endpoint != "metrics_instant" {
		t.Fatalf("Endpoint = %q", got[0].Endpoint)
	}
}
