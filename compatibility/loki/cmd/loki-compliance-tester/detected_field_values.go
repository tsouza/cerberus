package main

// Differential pass for GET /loki/api/v1/detected_field/{name}/values.
//
// The sibling of the /detected_fields pass. Logs Drilldown calls this
// route the moment a user opens one of the fields the fields route
// advertised, so a field that is advertised but whose values route
// answers differently — or not at all — is the same class of bug as an
// unqueryable field name: the panel populates from one inventory and
// filters against another.
//
// This pass is deliberately driven by the REFERENCE backend's own field
// list, not by a fixed name set: whatever upstream Loki says the seeded
// service has, cerberus must be able to answer for. No allow-list — any
// divergence is a real cerberus bug.

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

// detectedFieldValuesPerService caps how many of a service's fields the
// pass diffs. The seeded services carry a handful of fields each; the
// cap bounds the request count if a future fixture grows a wide schema,
// and the fields are taken in sorted order so the sampled set is stable
// across runs rather than dependent on upstream's map iteration.
const detectedFieldValuesPerService = 8

// detectedFieldValuesWire is the BARE top-level response body. Upstream
// reuses logproto.DetectedFieldsResponse for this route and populates
// only `values` (pkg/querier/queryrange/detected_fields.go).
type detectedFieldValuesWire struct {
	Values []string `json:"values"`
	Limit  uint32   `json:"limit"`
}

// compareDetectedFieldValuesAll runs the per-service, per-field values
// differential and returns one Result per (service, field) pair.
func compareDetectedFieldValuesAll(c *http.Client, f flags, metadata *bench.DatasetMetadata) []Result {
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
		selector := selectors[0]
		start, end := metadata.TimeRange.Start, metadata.TimeRange.End

		// Ask the reference which fields this service has. A failure
		// here is a harness/seed problem, and it is reported as one
		// case rather than silently skipping the service.
		fields, err := fetchDetectedFields(c, f.addr1, selector, start, end)
		if err != nil {
			results = append(results, Result{
				TestCase: detectedFieldValuesCase(svc, "*", selector, start, end),
				UnexpectedFailure: fmt.Sprintf(
					"reference (-addr-1) /detected_fields failed, cannot enumerate fields: %v", err,
				),
			})
			continue
		}
		names := sortedFieldLabels(fieldMap(fields.Fields))
		if len(names) == 0 {
			results = append(results, Result{
				TestCase:          detectedFieldValuesCase(svc, "*", selector, start, end),
				UnexpectedFailure: "reference advertised no fields for this service",
			})
			continue
		}
		if len(names) > detectedFieldValuesPerService {
			names = names[:detectedFieldValuesPerService]
		}
		for _, name := range names {
			results = append(results, compareDetectedFieldValuesOne(c, f, svc, name, selector, start, end))
		}
	}
	return results
}

func detectedFieldValuesCase(service, field, selector string, start, end time.Time) TestCase {
	return TestCase{
		Query:       selector,
		Source:      "detected-field-values",
		Description: "detected_field values parity: service=" + service + " field=" + field,
		Kind:        "detected_field_values",
		Direction:   "backward",
		Start:       start.UTC().Format(time.RFC3339Nano),
		End:         end.UTC().Format(time.RFC3339Nano),
	}
}

func compareDetectedFieldValuesOne(c *http.Client, f flags, service, field, selector string, start, end time.Time) Result {
	result := Result{TestCase: detectedFieldValuesCase(service, field, selector, start, end)}

	type fetched struct {
		body detectedFieldValuesWire
		err  error
	}
	out := make([]fetched, 2)
	done := make(chan int, 2)
	for idx, addr := range []string{f.addr1, f.addr2} {
		idx, addr := idx, addr
		go func() {
			body, err := fetchDetectedFieldValues(c, addr, field, selector, start, end)
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
	if len(expected.Values) == 0 {
		// The reference advertised this field, so it must have values.
		// An empty baseline is a harness/seed problem, not a parity
		// datapoint — same convention as the fields pass.
		result.UnexpectedFailure = "baseline returned empty"
		return result
	}
	if diff := diffDetectedFieldValues(expected, actual); diff != "" {
		result.Diff = diff
	}
	return result
}

func fetchDetectedFieldValues(c *http.Client, addr, field, selector string, start, end time.Time) (detectedFieldValuesWire, error) {
	base := strings.TrimRight(addr, "/")
	params := url.Values{}
	params.Set("query", selector)
	params.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	params.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	params.Set("line_limit", strconv.Itoa(detectedFieldsLineLimit))

	target := base + "/loki/api/v1/detected_field/" + url.PathEscape(field) + "/values?" + params.Encode()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return detectedFieldValuesWire{}, fmt.Errorf("new request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return detectedFieldValuesWire{}, fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if readErr != nil {
		return detectedFieldValuesWire{}, fmt.Errorf("read body: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return detectedFieldValuesWire{}, fmt.Errorf("status=%d body=%s", resp.StatusCode, errorBodySnippet(string(body)))
	}

	// Consumer-grade decode, same contract as the fields route: the
	// body is the BARE DetectedFieldsResponse, no {status, data}
	// envelope.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return detectedFieldValuesWire{}, fmt.Errorf("decode top-level object: %w", err)
	}
	if _, ok := top["data"]; ok {
		return detectedFieldValuesWire{}, fmt.Errorf("response carries a {status,data} envelope; upstream serializes DetectedFieldsResponse bare: %s", body)
	}
	var decoded detectedFieldValuesWire
	if err := json.Unmarshal(body, &decoded); err != nil {
		return detectedFieldValuesWire{}, fmt.Errorf("decode: %w", err)
	}
	return decoded, nil
}

// diffDetectedFieldValues compares the two value sets order-insensitively
// (upstream ranges a Go map, so its order is random) and returns a
// human-readable diff, or "" when they match.
func diffDetectedFieldValues(expected, actual detectedFieldValuesWire) string {
	exp := slices.Clone(expected.Values)
	act := slices.Clone(actual.Values)
	sort.Strings(exp)
	sort.Strings(act)

	var diffs []string
	if !slices.Equal(exp, act) {
		for _, v := range exp {
			if !slices.Contains(act, v) {
				diffs = append(diffs, fmt.Sprintf("value %q missing from test endpoint", v))
			}
		}
		for _, v := range act {
			if !slices.Contains(exp, v) {
				diffs = append(diffs, fmt.Sprintf("value %q unexpected on test endpoint", v))
			}
		}
		if len(diffs) == 0 {
			// The two membership loops answer "which values are on one
			// side only", which is silent when the lists differ solely
			// in how many times a value appears. Reporting the sorted
			// lists keeps the sorted-slice inequality this branch was
			// entered on from resolving to "no diff": a backend
			// repeating a value where the reference emits it once is a
			// real wire-shape divergence, and Grafana renders the
			// duplicate.
			diffs = append(diffs, fmt.Sprintf("value multiplicity: expected=%v actual=%v", exp, act))
		}
	}
	if expected.Limit != actual.Limit {
		diffs = append(diffs, fmt.Sprintf("limit: expected=%d actual=%d", expected.Limit, actual.Limit))
	}
	return strings.Join(diffs, "; ")
}
