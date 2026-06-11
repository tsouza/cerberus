package main

// Differential pass for GET /loki/api/v1/detected_fields.
//
// The bench corpus only exercises /query + /query_range, but Grafana's
// Logs Drilldown drives every service page off /detected_fields — a
// 200 with the wrong shape (or wrong field semantics) renders
// "Fields: 0" while every status-code oracle stays green. This pass
// closes that gap: for every seeded service it requests the endpoint
// from both backends, decodes the BARE top-level wire shape exactly as
// the consumer does, and diffs the field sets — label, type,
// cardinality, parsers, and jsonPath all participate. No allow-list:
// any divergence from reference Loki is a real cerberus bug.
//
// Comparability contract (held by the seeder, see cmd/seed/main.go):
// the CH `LogAttributes` map carries the same key set pushLoki sends
// as structured metadata, and both backends compile the same
// axiomhq/hyperloglog version, so cardinality estimates are
// bit-identical when both sides observed the same value sets. The
// per-service line_limit below exceeds the per-service row count so
// neither backend truncates the peek window.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	bench "github.com/tsouza/cerberus/compatibility/loki/upstream/loki-bench"
)

// detectedFieldsLineLimit must exceed entriesPerService (1440 — see
// cmd/seed/main.go) so both backends parse EVERY seeded row for the
// selector: a truncated peek window would make the hyperloglog
// cardinality estimates depend on tie-breaking order instead of data.
const detectedFieldsLineLimit = 2000

// detectedFieldWire mirrors upstream logproto.DetectedField's JSON
// surface — the shape Grafana's Logs Drilldown decodes.
type detectedFieldWire struct {
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Cardinality uint64   `json:"cardinality"`
	Parsers     []string `json:"parsers"`
	JSONPath    []string `json:"jsonPath"`
}

// detectedFieldsWire is the BARE top-level response body — upstream
// serializes logproto.DetectedFieldsResponse verbatim (no {status,
// data} envelope; see pkg/util/marshal WriteDetectedFieldsResponseJSON).
type detectedFieldsWire struct {
	Fields []detectedFieldWire `json:"fields"`
	Limit  uint32              `json:"limit"`
}

// compareDetectedFieldsAll runs the per-service detected-fields
// differential and returns one Result per seeded service. Results feed
// the same report + score pipeline as the corpus cases.
func compareDetectedFieldsAll(c *http.Client, f flags, metadata *bench.DatasetMetadata) []Result {
	services := make([]string, 0, len(metadata.ByServiceName))
	for svc := range metadata.ByServiceName {
		services = append(services, svc)
	}
	sort.Strings(services)

	var results []Result
	for _, svc := range services {
		selectors := metadata.ByServiceName[svc]
		if len(selectors) == 0 {
			continue
		}
		// One stream per service in the seeded fixture; the first
		// selector is the full label-set selector for that stream.
		results = append(results, compareDetectedFieldsOne(
			c, f, svc, selectors[0], metadata.TimeRange.Start, metadata.TimeRange.End,
		))
	}
	return results
}

func compareDetectedFieldsOne(c *http.Client, f flags, service, selector string, start, end time.Time) Result {
	result := Result{TestCase: TestCase{
		Query:       selector,
		Source:      "detected-fields",
		Description: "detected_fields parity: service=" + service,
		Kind:        "detected_fields",
		Direction:   "backward",
		Start:       start.UTC().Format(time.RFC3339Nano),
		End:         end.UTC().Format(time.RFC3339Nano),
	}}

	type fetched struct {
		body detectedFieldsWire
		err  error
	}
	out := make([]fetched, 2)
	done := make(chan int, 2)
	for idx, addr := range []string{f.addr1, f.addr2} {
		idx, addr := idx, addr
		go func() {
			body, err := fetchDetectedFields(c, addr, selector, start, end)
			out[idx] = fetched{body: body, err: err}
			done <- idx
		}()
	}
	<-done
	<-done

	refErr, testErr := out[0].err, out[1].err
	switch {
	case refErr != nil:
		result.UnexpectedFailure = fmt.Sprintf("reference (-addr-1) failed: %v", refErr)
		return result
	case testErr != nil:
		result.UnexpectedFailure = testErr.Error()
		result.Unsupported = isUnsupportedErr(testErr)
		return result
	}

	expected, actual := out[0].body, out[1].body
	if len(expected.Fields) == 0 {
		// Same convention as the corpus path: an empty baseline is a
		// harness/seed problem, not a parity datapoint.
		result.UnexpectedFailure = "baseline returned empty"
		return result
	}
	if len(actual.Fields) == 0 {
		result.UnexpectedFailure = "test endpoint returned empty"
		return result
	}
	if diff := diffDetectedFields(expected, actual); diff != "" {
		result.Diff = diff
	}
	return result
}

func fetchDetectedFields(c *http.Client, addr, selector string, start, end time.Time) (detectedFieldsWire, error) {
	base := strings.TrimRight(addr, "/")
	params := url.Values{}
	params.Set("query", selector)
	params.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	params.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	params.Set("line_limit", strconv.Itoa(detectedFieldsLineLimit))

	req, err := http.NewRequest(http.MethodGet, base+"/loki/api/v1/detected_fields?"+params.Encode(), nil)
	if err != nil {
		return detectedFieldsWire{}, fmt.Errorf("new request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return detectedFieldsWire{}, fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if readErr != nil {
		return detectedFieldsWire{}, fmt.Errorf("read body: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 400 {
			snippet = snippet[:400] + "…"
		}
		return detectedFieldsWire{}, fmt.Errorf("status=%d body=%s", resp.StatusCode, snippet)
	}

	// Consumer-grade decode: the response must be the BARE
	// DetectedFieldsResponse. An enveloped body decodes to zero fields
	// here — exactly what Grafana would see — and surfaces as
	// "test endpoint returned empty" / a field-set diff upstream of
	// this function.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return detectedFieldsWire{}, fmt.Errorf("decode top-level object: %w", err)
	}
	if _, ok := top["data"]; ok {
		return detectedFieldsWire{}, fmt.Errorf("response carries a {status,data} envelope; upstream serializes DetectedFieldsResponse bare: %s", body)
	}
	var decoded detectedFieldsWire
	if err := json.Unmarshal(body, &decoded); err != nil {
		return detectedFieldsWire{}, fmt.Errorf("decode: %w", err)
	}
	return decoded, nil
}

// diffDetectedFields compares the two field sets (order-insensitive —
// upstream iterates a Go map so its output order is random) and
// returns a human-readable diff, or "" when they match.
func diffDetectedFields(expected, actual detectedFieldsWire) string {
	exp := fieldMap(expected.Fields)
	act := fieldMap(actual.Fields)

	var diffs []string
	for _, label := range sortedFieldLabels(exp) {
		e := exp[label]
		a, ok := act[label]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("field %q missing from test endpoint", label))
			continue
		}
		if e.Type != a.Type {
			diffs = append(diffs, fmt.Sprintf("field %q type: expected=%q actual=%q", label, e.Type, a.Type))
		}
		if e.Cardinality != a.Cardinality {
			diffs = append(diffs, fmt.Sprintf("field %q cardinality: expected=%d actual=%d", label, e.Cardinality, a.Cardinality))
		}
		if !sameStringSet(e.Parsers, a.Parsers) {
			diffs = append(diffs, fmt.Sprintf("field %q parsers: expected=%v actual=%v", label, e.Parsers, a.Parsers))
		}
		if !slices.Equal(e.JSONPath, a.JSONPath) {
			diffs = append(diffs, fmt.Sprintf("field %q jsonPath: expected=%v actual=%v", label, e.JSONPath, a.JSONPath))
		}
	}
	for _, label := range sortedFieldLabels(act) {
		if _, ok := exp[label]; !ok {
			diffs = append(diffs, fmt.Sprintf("field %q unexpected on test endpoint", label))
		}
	}
	if expected.Limit != actual.Limit {
		diffs = append(diffs, fmt.Sprintf("limit: expected=%d actual=%d", expected.Limit, actual.Limit))
	}
	return strings.Join(diffs, "; ")
}

func fieldMap(fields []detectedFieldWire) map[string]detectedFieldWire {
	m := make(map[string]detectedFieldWire, len(fields))
	for _, f := range fields {
		m[f.Label] = f
	}
	return m
}

func sortedFieldLabels(m map[string]detectedFieldWire) []string {
	labels := make([]string, 0, len(m))
	for l := range m {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	return labels
}

// sameStringSet compares two parser lists as sets: the upstream
// handler appends parser names in row-processing order, which is not
// part of the contract.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := slices.Clone(a)
	bs := slices.Clone(b)
	sort.Strings(as)
	sort.Strings(bs)
	return slices.Equal(as, bs)
}
