package loki_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
)

// getDetectedFieldValues issues a /loki/api/v1/detected_field/{name}/values
// request and decodes the body as the consumer does — the same BARE
// logproto.DetectedFieldsResponse the fields route returns, with only
// `values` populated. No {status, data} envelope.
func getDetectedFieldValues(t *testing.T, base, name, extra string) loki.DetectedFieldsResponse {
	t.Helper()
	u := base + "/loki/api/v1/detected_field/" + url.PathEscape(name) +
		"/values?query=" + url.QueryEscape(`{job="api"}`) + extra
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var out loki.DetectedFieldsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// valuesRows is the peek window both tests below share: two structured
// metadata keys and a logfmt-parsed body, as ClickHouse would return
// them (LogfmtFields is the CH-side `| logfmt` extraction, not a Go
// re-parse of Line).
func valuesRows() []chclient.DetectedFieldRow {
	return []chclient.DetectedFieldRow{
		{
			Line:         `status=200 latency=12ms`,
			Attributes:   map[string]string{"detected_level": "info", "query_id": "a"},
			LogfmtFields: map[string]string{"status": "200", "latency": "12ms"},
		},
		{
			Line:         `status=500 latency=1s`,
			Attributes:   map[string]string{"detected_level": "error", "query_id": "b"},
			LogfmtFields: map[string]string{"status": "500", "latency": "1s"},
		},
		{
			Line:         `status=200 latency=9ms`,
			Attributes:   map[string]string{"detected_level": "info", "query_id": "c"},
			LogfmtFields: map[string]string{"status": "200", "latency": "9ms"},
		},
	}
}

// TestDetectedFieldValues_Sources — the route answers for both field
// sources: structured metadata (LogAttributes) and the body-parsed
// labels. Values are distinct and sorted; duplicates across rows
// collapse.
func TestDetectedFieldValues_Sources(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{detectedRows: valuesRows()})
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		field string
		want  []string
	}{
		{"detected_level", []string{"error", "info"}}, // structured metadata
		{"status", []string{"200", "500"}},            // logfmt-parsed
		{"latency", []string{"12ms", "1s", "9ms"}},
	} {
		out := getDetectedFieldValues(t, srv.URL, tc.field, "")
		if !reflect.DeepEqual(out.Values, tc.want) {
			t.Errorf("%s values=%v want %v", tc.field, out.Values, tc.want)
		}
		if len(out.Fields) != 0 {
			t.Errorf("%s: values route must not emit `fields`: %+v", tc.field, out.Fields)
		}
	}
}

// TestDetectedFieldValues_AnswersForEveryAdvertisedField is the contract
// tying #1888 and #1485 together: every field /detected_fields
// advertises must resolve to a non-empty value list on the values route,
// and a name it does not advertise must resolve to nothing. Both routes
// read one inventory, so this cannot be satisfied by two agreeing
// re-derivations — only by there being a single one.
func TestDetectedFieldValues_AnswersForEveryAdvertisedField(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{detectedRows: valuesRows()})
	t.Cleanup(srv.Close)

	fields := getDetectedFields(t, srv.URL+
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D`)
	if len(fields.Fields) == 0 {
		t.Fatal("no advertised fields to check")
	}
	for _, f := range fields.Fields {
		out := getDetectedFieldValues(t, srv.URL, f.Label, "")
		if len(out.Values) == 0 {
			t.Errorf("advertised field %q has no values — the two routes disagree", f.Label)
		}
	}

	// A name nobody advertised resolves to an empty list, not a 404:
	// upstream answers 200 with an omitted `values`.
	out := getDetectedFieldValues(t, srv.URL, "not_a_field", "")
	if len(out.Values) != 0 {
		t.Errorf("unknown field returned values=%v", out.Values)
	}
	if out.Limit != 0 {
		t.Errorf("limit echoed on an empty value list: %d", out.Limit)
	}
}

// TestDetectedFieldValues_LimitCapsDistinctValues — `limit` caps the
// number of DISTINCT values returned and is echoed back, mirroring the
// fields route.
func TestDetectedFieldValues_LimitCapsDistinctValues(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{detectedRows: valuesRows()})
	t.Cleanup(srv.Close)

	out := getDetectedFieldValues(t, srv.URL, "latency", "&limit=2")
	if len(out.Values) != 2 {
		t.Fatalf("values=%v want 2 (limit cap)", out.Values)
	}
	if out.Limit != 2 {
		t.Errorf("limit echo=%d want 2", out.Limit)
	}
}

// TestDetectedFieldValues_SkipsEmpty — a CH Map read of an absent key
// yields "", which is indistinguishable from "the field is not on this
// row". An empty string is never offered as a value: a filter built from
// it would match nothing.
func TestDetectedFieldValues_SkipsEmpty(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{detectedRows: []chclient.DetectedFieldRow{
		{Attributes: map[string]string{"level": ""}},
		{Attributes: map[string]string{"level": "warn"}},
	}})
	t.Cleanup(srv.Close)

	out := getDetectedFieldValues(t, srv.URL, "level", "")
	if !reflect.DeepEqual(out.Values, []string{"warn"}) {
		t.Errorf("values=%v want [warn]", out.Values)
	}
}

// TestDetectedFieldValues_BadInput — the values route takes the same
// parameters as the fields route and rejects the same way.
func TestDetectedFieldValues_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"missing query", `/loki/api/v1/detected_field/level/values?start=1&end=2`},
		{"bad query", `/loki/api/v1/detected_field/level/values?query=%7Bnot+a+selector`},
		{"bad limit", `/loki/api/v1/detected_field/level/values?query=%7Bjob%3D%22api%22%7D&limit=-1`},
		{"bad line_limit", `/loki/api/v1/detected_field/level/values?query=%7Bjob%3D%22api%22%7D&line_limit=0`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", resp.StatusCode)
			}
		})
	}
}
