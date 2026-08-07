package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	bench "github.com/tsouza/cerberus/compatibility/loki/upstream/loki-bench"
)

// The values pass exists because Logs Drilldown populates its filter
// menu from one route and filters against another: a field advertised by
// /detected_fields whose /values route answers differently is the same
// class of bug as an unqueryable field name. The pass is driven by the
// REFERENCE backend's own field list, so its enumeration step is part of
// the contract — a service whose fields cannot be enumerated must
// produce a visible failure row, never a silently skipped service.

// fieldValuesStub serves both routes the pass calls: the fields
// enumeration and the per-field values lookup.
type fieldValuesStub struct {
	fieldsBody     string
	valuesBody     map[string]string
	valuesFallback string
	status         int

	requestedFields []string
	requestedPaths  []string
	requestedQuery  string
	requestedLimit  string
}

func newFieldValuesStub(t *testing.T, s *fieldValuesStub) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.status != 0 {
			w.WriteHeader(s.status)
			_, _ = w.Write([]byte("backend said no"))
			return
		}
		if r.URL.Path == "/loki/api/v1/detected_fields" {
			_, _ = w.Write([]byte(s.fieldsBody))
			return
		}
		// /loki/api/v1/detected_field/<name>/values
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/loki/api/v1/detected_field/"), "/values")
		decoded, err := url.PathUnescape(name)
		if err != nil {
			decoded = name
		}
		s.requestedFields = append(s.requestedFields, decoded)
		// EscapedPath is the path as it travelled the wire, which is
		// what the escaping contract is actually about — r.URL.Path is
		// already decoded and so cannot distinguish an escaped name
		// from an unescaped one.
		s.requestedPaths = append(s.requestedPaths, r.URL.EscapedPath())
		s.requestedQuery = r.URL.Query().Get("query")
		s.requestedLimit = r.URL.Query().Get("line_limit")
		if body, ok := s.valuesBody[decoded]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(s.valuesFallback))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const (
	fieldsBodyTwo  = `{"fields":[{"label":"status","type":"int","cardinality":3},{"label":"path","type":"string","cardinality":9}],"limit":1000}`
	valuesBodyABC  = `{"values":["a","b","c"],"limit":1000}`
	valuesBodyCBA  = `{"values":["c","b","a"],"limit":1000}`
	valuesBodyNone = `{"values":[],"limit":1000}`
)

func fieldValuesMetadata() *bench.DatasetMetadata {
	return &bench.DatasetMetadata{
		ByServiceName: map[string][]string{
			"api": {`{service_name="api"}`},
		},
		TimeRange: bench.TimeRange{
			Start: time.Unix(1700000000, 0).UTC(),
			End:   time.Unix(1700086400, 0).UTC(),
		},
	}
}

// TestDiffDetectedFieldValues_OrderInsensitiveMatch — upstream ranges a
// Go map to build the list, so its order is random per request. A
// comparator that were order-sensitive would report a diff on every run
// of a perfectly correct backend.
func TestDiffDetectedFieldValues_OrderInsensitiveMatch(t *testing.T) {
	t.Parallel()
	expected := detectedFieldValuesWire{Values: []string{"a", "b", "c"}, Limit: 1000}
	actual := detectedFieldValuesWire{Values: []string{"c", "a", "b"}, Limit: 1000}
	if diff := diffDetectedFieldValues(expected, actual); diff != "" {
		t.Fatalf("re-ordered identical value sets diffed: %s", diff)
	}
	// The comparator must not mutate its inputs — the caller keeps the
	// decoded bodies for the report.
	if expected.Values[0] != "a" || actual.Values[0] != "c" {
		t.Fatalf("diffDetectedFieldValues sorted its arguments in place: expected=%v actual=%v", expected.Values, actual.Values)
	}
}

// TestDiffDetectedFieldValues_Divergences — a missing value, an extra
// value and a limit disagreement each surface, and all three are
// reported together rather than short-circuiting on the first.
func TestDiffDetectedFieldValues_Divergences(t *testing.T) {
	t.Parallel()
	expected := detectedFieldValuesWire{Values: []string{"a", "b"}, Limit: 1000}
	actual := detectedFieldValuesWire{Values: []string{"a", "z"}, Limit: 500}
	diff := diffDetectedFieldValues(expected, actual)
	for _, want := range []string{
		`value "b" missing from test endpoint`,
		`value "z" unexpected on test endpoint`,
		"limit: expected=1000 actual=500",
	} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff %q missing %q", diff, want)
		}
	}
}

// TestDiffDetectedFieldValues_DuplicateCountsMatter — the same value set
// with a repeated entry is NOT equal: a backend emitting a value twice
// where the reference emits it once is a real wire-shape divergence, and
// a set-membership-only comparison would miss it.
func TestDiffDetectedFieldValues_DuplicateCountsMatter(t *testing.T) {
	t.Parallel()
	expected := detectedFieldValuesWire{Values: []string{"a", "b"}, Limit: 1}
	actual := detectedFieldValuesWire{Values: []string{"a", "a", "b"}, Limit: 1}
	if diff := diffDetectedFieldValues(expected, actual); diff == "" {
		t.Fatal("a duplicated value compared equal to the single-occurrence set")
	}
}

// TestDetectedFieldValuesCase pins the report envelope: the identity a
// reviewer reads and the ratchet keys on.
func TestDetectedFieldValuesCase(t *testing.T) {
	t.Parallel()
	start := time.Unix(1700000000, 0).UTC()
	end := time.Unix(1700086400, 0).UTC()
	got := detectedFieldValuesCase("api", "status", `{service_name="api"}`, start, end)
	if got.Kind != "detected_field_values" {
		t.Fatalf("Kind = %q", got.Kind)
	}
	if got.Source != "detected-field-values" {
		t.Fatalf("Source = %q", got.Source)
	}
	if !strings.Contains(got.Description, "service=api") || !strings.Contains(got.Description, "field=status") {
		t.Fatalf("Description = %q, want both the service and the field named", got.Description)
	}
	if got.Query != `{service_name="api"}` {
		t.Fatalf("Query = %q", got.Query)
	}
	if got.Start != "2023-11-14T22:13:20Z" || got.End != "2023-11-15T22:13:20Z" {
		t.Fatalf("window = (%q, %q), want RFC3339Nano UTC", got.Start, got.End)
	}
}

// TestFetchDetectedFieldValues_DecodesBareShape — the route serialises
// the BARE DetectedFieldsResponse, and the request must carry the
// line_limit that makes both backends read every seeded row.
func TestFetchDetectedFieldValues_DecodesBareShape(t *testing.T) {
	t.Parallel()
	stub := &fieldValuesStub{valuesFallback: valuesBodyABC}
	addr := newFieldValuesStub(t, stub)

	got, err := fetchDetectedFieldValues(&http.Client{Timeout: 5 * time.Second}, addr+"/", "status",
		`{service_name="api"}`, time.Unix(0, 0), time.Unix(60, 0))
	if err != nil {
		t.Fatalf("fetchDetectedFieldValues: %v", err)
	}
	if len(got.Values) != 3 || got.Limit != 1000 {
		t.Fatalf("decoded = %+v", got)
	}
	if stub.requestedQuery != `{service_name="api"}` {
		t.Fatalf("query param = %q", stub.requestedQuery)
	}
	if stub.requestedLimit != "2000" {
		t.Fatalf("line_limit = %q, want 2000 (must exceed the seeded rows per service)", stub.requestedLimit)
	}
}

// TestFetchDetectedFieldValues_EscapesTheFieldName — a field name with a
// slash or a space must reach the backend intact, or the pass would
// query a different field than the one the reference advertised.
func TestFetchDetectedFieldValues_EscapesTheFieldName(t *testing.T) {
	t.Parallel()
	stub := &fieldValuesStub{valuesFallback: valuesBodyABC}
	addr := newFieldValuesStub(t, stub)

	const awkward = "http/status code"
	if _, err := fetchDetectedFieldValues(&http.Client{Timeout: 5 * time.Second}, addr, awkward,
		`{service_name="api"}`, time.Unix(0, 0), time.Unix(60, 0)); err != nil {
		t.Fatalf("fetchDetectedFieldValues: %v", err)
	}
	if len(stub.requestedFields) != 1 || stub.requestedFields[0] != awkward {
		t.Fatalf("backend saw field %v, want %q intact", stub.requestedFields, awkward)
	}
	// The decoded name round-trips even when the slash was NOT escaped,
	// because the extra path segment lands back inside the same
	// prefix/suffix trim. Only the on-the-wire path distinguishes the
	// two, and an unescaped slash is a different route to the backend.
	const wantPath = "/loki/api/v1/detected_field/http%2Fstatus%20code/values"
	if stub.requestedPaths[0] != wantPath {
		t.Fatalf("wire path = %q, want %q", stub.requestedPaths[0], wantPath)
	}
}

// TestFetchDetectedFieldValues_RejectsEnvelope — an enveloped body would
// otherwise decode to zero values and read as "the field has no values",
// which is exactly the bug class this pass exists to catch.
func TestFetchDetectedFieldValues_RejectsEnvelope(t *testing.T) {
	t.Parallel()
	stub := &fieldValuesStub{valuesFallback: `{"status":"success","data":{"values":["a"],"limit":1000}}`}
	addr := newFieldValuesStub(t, stub)

	_, err := fetchDetectedFieldValues(&http.Client{Timeout: 5 * time.Second}, addr, "status",
		`{service_name="api"}`, time.Unix(0, 0), time.Unix(60, 0))
	if err == nil {
		t.Fatal("an enveloped body was accepted")
	}
	if !strings.Contains(err.Error(), "envelope") {
		t.Fatalf("error %q should name the envelope", err.Error())
	}
}

// TestFetchDetectedFieldValues_ErrorPaths — a non-200 carries the status
// code, and a body that is not a JSON object is a decode error rather
// than an empty value set.
func TestFetchDetectedFieldValues_ErrorPaths(t *testing.T) {
	t.Parallel()

	failing := newFieldValuesStub(t, &fieldValuesStub{status: http.StatusServiceUnavailable})
	_, err := fetchDetectedFieldValues(&http.Client{Timeout: 5 * time.Second}, failing, "status",
		`{service_name="api"}`, time.Unix(0, 0), time.Unix(60, 0))
	if err == nil || !strings.Contains(err.Error(), "status=503") {
		t.Fatalf("error = %v, want it to carry status=503", err)
	}

	garbage := newFieldValuesStub(t, &fieldValuesStub{valuesFallback: `["a","b"]`})
	_, err = fetchDetectedFieldValues(&http.Client{Timeout: 5 * time.Second}, garbage, "status",
		`{service_name="api"}`, time.Unix(0, 0), time.Unix(60, 0))
	if err == nil || !strings.Contains(err.Error(), "decode top-level object") {
		t.Fatalf("error = %v, want a top-level decode failure", err)
	}
}

// TestCompareDetectedFieldValuesOne_Arms drives the per-field verdict:
// agreement, divergence, an empty baseline (a seed problem, not a parity
// datapoint), and a failure on each side.
func TestCompareDetectedFieldValuesOne_Arms(t *testing.T) {
	t.Parallel()
	start := time.Unix(1700000000, 0).UTC()
	end := time.Unix(1700086400, 0).UTC()

	cases := []struct {
		name        string
		ref         *fieldValuesStub
		test        *fieldValuesStub
		wantPass    bool
		wantDiff    string
		wantFailure string
	}{
		{
			name:     "agreement-ignores-order",
			ref:      &fieldValuesStub{valuesFallback: valuesBodyABC},
			test:     &fieldValuesStub{valuesFallback: valuesBodyCBA},
			wantPass: true,
		},
		{
			name:     "divergence",
			ref:      &fieldValuesStub{valuesFallback: valuesBodyABC},
			test:     &fieldValuesStub{valuesFallback: `{"values":["a","b"],"limit":1000}`},
			wantDiff: `value "c" missing from test endpoint`,
		},
		{
			name:        "empty-baseline-is-a-harness-problem",
			ref:         &fieldValuesStub{valuesFallback: valuesBodyNone},
			test:        &fieldValuesStub{valuesFallback: valuesBodyABC},
			wantFailure: "baseline returned empty",
		},
		{
			name:        "reference-side-failure",
			ref:         &fieldValuesStub{status: http.StatusInternalServerError},
			test:        &fieldValuesStub{valuesFallback: valuesBodyABC},
			wantFailure: "reference (-addr-1)",
		},
		{
			name:        "test-side-failure",
			ref:         &fieldValuesStub{valuesFallback: valuesBodyABC},
			test:        &fieldValuesStub{status: http.StatusInternalServerError},
			wantFailure: "status=500",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := flagsFor(newFieldValuesStub(t, tc.ref), newFieldValuesStub(t, tc.test))
			got := compareDetectedFieldValuesOne(&http.Client{Timeout: 5 * time.Second}, f, "api", "status", `{service_name="api"}`, start, end)
			switch {
			case tc.wantPass:
				if !got.success() {
					t.Fatalf("want pass, got %+v", got)
				}
			case tc.wantDiff != "":
				if !strings.Contains(got.Diff, tc.wantDiff) {
					t.Fatalf("Diff = %q, want %q (failure=%q)", got.Diff, tc.wantDiff, got.UnexpectedFailure)
				}
			default:
				if !strings.Contains(got.UnexpectedFailure, tc.wantFailure) {
					t.Fatalf("UnexpectedFailure = %q, want it to contain %q", got.UnexpectedFailure, tc.wantFailure)
				}
			}
		})
	}
}

// TestCompareDetectedFieldValuesAll_DrivenByTheReferenceInventory — the
// pass asks the reference which fields exist and diffs each of them, in
// sorted order. Driving it from a fixed name list instead would let a
// field the reference advertises go untested forever.
func TestCompareDetectedFieldValuesAll_DrivenByTheReferenceInventory(t *testing.T) {
	t.Parallel()
	ref := &fieldValuesStub{fieldsBody: fieldsBodyTwo, valuesFallback: valuesBodyABC}
	test := &fieldValuesStub{fieldsBody: fieldsBodyTwo, valuesFallback: valuesBodyABC}
	f := flagsFor(newFieldValuesStub(t, ref), newFieldValuesStub(t, test))

	results := compareDetectedFieldValuesAll(&http.Client{Timeout: 5 * time.Second}, f, fieldValuesMetadata())
	if len(results) != 2 {
		t.Fatalf("results = %d, want one per advertised field", len(results))
	}
	for _, r := range results {
		if !r.success() {
			t.Fatalf("agreeing backends produced a non-passing row: %+v", r)
		}
	}
	// sortedFieldLabels drives the order, so the roster is stable across
	// runs rather than dependent on upstream's map iteration.
	if len(ref.requestedFields) != 2 || ref.requestedFields[0] != "path" || ref.requestedFields[1] != "status" {
		t.Fatalf("reference queried fields %v, want [path status] in sorted order", ref.requestedFields)
	}
}

// TestCompareDetectedFieldValuesAll_CapsFieldsPerService — the sampled
// set is bounded, and the cap takes the SORTED prefix so the sample is
// stable rather than whichever fields upstream's map happened to yield.
func TestCompareDetectedFieldValuesAll_CapsFieldsPerService(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString(`{"fields":[`)
	// Names are emitted in descending order so a cap applied before the
	// sort would keep a different set than a cap applied after it.
	const wideFieldCount = detectedFieldValuesPerService + 4
	for i := wideFieldCount - 1; i >= 0; i-- {
		if i != wideFieldCount-1 {
			b.WriteString(",")
		}
		b.WriteString(`{"label":"f`)
		b.WriteByte(byte('0' + i))
		b.WriteString(`","type":"string","cardinality":1}`)
	}
	b.WriteString(`],"limit":1000}`)

	ref := &fieldValuesStub{fieldsBody: b.String(), valuesFallback: valuesBodyABC}
	test := &fieldValuesStub{fieldsBody: b.String(), valuesFallback: valuesBodyABC}
	f := flagsFor(newFieldValuesStub(t, ref), newFieldValuesStub(t, test))

	results := compareDetectedFieldValuesAll(&http.Client{Timeout: 5 * time.Second}, f, fieldValuesMetadata())
	if len(results) != detectedFieldValuesPerService {
		t.Fatalf("results = %d, want the per-service cap of %d", len(results), detectedFieldValuesPerService)
	}
	for i, name := range ref.requestedFields {
		want := "f" + string(rune('0'+i))
		if name != want {
			t.Fatalf("sampled field %d = %q, want %q (the sorted prefix)", i, name, want)
		}
	}
}

// TestCompareDetectedFieldValuesAll_EnumerationFailuresAreVisible — a
// service whose fields cannot be enumerated, or for which the reference
// advertises none, produces a failure row rather than vanishing from the
// report.
func TestCompareDetectedFieldValuesAll_EnumerationFailuresAreVisible(t *testing.T) {
	t.Parallel()

	broken := flagsFor(
		newFieldValuesStub(t, &fieldValuesStub{status: http.StatusInternalServerError}),
		newFieldValuesStub(t, &fieldValuesStub{fieldsBody: fieldsBodyTwo, valuesFallback: valuesBodyABC}),
	)
	results := compareDetectedFieldValuesAll(&http.Client{Timeout: 5 * time.Second}, broken, fieldValuesMetadata())
	if len(results) != 1 {
		t.Fatalf("results = %d, want one enumeration-failure row", len(results))
	}
	if !strings.Contains(results[0].UnexpectedFailure, "cannot enumerate fields") {
		t.Fatalf("UnexpectedFailure = %q", results[0].UnexpectedFailure)
	}
	if !strings.Contains(results[0].TestCase.Description, "field=*") {
		t.Fatalf("Description = %q, want the wildcard field marker", results[0].TestCase.Description)
	}

	empty := flagsFor(
		newFieldValuesStub(t, &fieldValuesStub{fieldsBody: `{"fields":[],"limit":1000}`}),
		newFieldValuesStub(t, &fieldValuesStub{fieldsBody: fieldsBodyTwo, valuesFallback: valuesBodyABC}),
	)
	results = compareDetectedFieldValuesAll(&http.Client{Timeout: 5 * time.Second}, empty, fieldValuesMetadata())
	if len(results) != 1 {
		t.Fatalf("results = %d, want one no-fields row", len(results))
	}
	if results[0].UnexpectedFailure != "reference advertised no fields for this service" {
		t.Fatalf("UnexpectedFailure = %q", results[0].UnexpectedFailure)
	}
}

// TestCompareDetectedFieldValuesAll_SkipsSelectorlessServices — a
// metadata entry with no selector cannot be queried, so it contributes
// no row at all (as opposed to a failure row, which would blame the
// backend for a fixture gap).
func TestCompareDetectedFieldValuesAll_SkipsSelectorlessServices(t *testing.T) {
	t.Parallel()
	md := fieldValuesMetadata()
	md.ByServiceName["ghost"] = nil

	ref := &fieldValuesStub{fieldsBody: fieldsBodyTwo, valuesFallback: valuesBodyABC}
	f := flagsFor(newFieldValuesStub(t, ref), newFieldValuesStub(t, &fieldValuesStub{fieldsBody: fieldsBodyTwo, valuesFallback: valuesBodyABC}))

	results := compareDetectedFieldValuesAll(&http.Client{Timeout: 5 * time.Second}, f, md)
	if len(results) != 2 {
		t.Fatalf("results = %d, want the 2 rows of the one queryable service", len(results))
	}
	for _, r := range results {
		if strings.Contains(r.TestCase.Description, "ghost") {
			t.Fatalf("the selector-less service produced a row: %+v", r.TestCase)
		}
	}
}
