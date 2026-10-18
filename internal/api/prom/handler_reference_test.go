package prom_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestQuery_OutsideEvaluationDomain_AnsweredByReference pins that a query
// whose evaluation leaves the lowering's exact span is answered, never
// refused: the handlers route it to the reference engine before anything
// is lowered, which is what makes the lowering's own refusal
// (promql.ErrOutsideEvaluationDomain) unreachable over HTTP. A read whose
// window lies wholly outside the storage domain issues no SQL at all.
func TestQuery_OutsideEvaluationDomain_AnsweredByReference(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		params     url.Values
		endpoint   string
		resultType string
		want       string
	}{
		{
			name:       "year-one pin",
			endpoint:   "/api/v1/query",
			params:     url.Values{"query": {`count_over_time(vector(1)[5m:1m] @ -62135596800)`}, "time": {"1767225601"}},
			resultType: "vector",
			want:       `[{"metric":{},"value":[1767225601,"5"]}]`,
		},
		{
			name:       "selector pinned below 1900",
			endpoint:   "/api/v1/query",
			params:     url.Values{"query": {`up @ -9300000000`}, "time": {"1767225601"}},
			resultType: "vector",
			want:       `[]`,
		},
		{
			name:     "range query over a pin past 2262",
			endpoint: "/api/v1/query_range",
			params: url.Values{
				"query": {`count_over_time(vector(1)[5m:1m] @ 9300000000)`},
				"start": {"1767225480"}, "end": {"1767225600"}, "step": {"60"},
			},
			resultType: "matrix",
			want:       `[{"metric":{},"values":[[1767225480,"5"],[1767225540,"5"],[1767225600,"5"]]}]`,
		},
		{
			// Go's zero time.Time as the request instant. Prometheus's
			// engine converts request instants through UnixNano(), which
			// wraps year 1 into 1754, and evaluates there.
			name:       "year-one request time",
			endpoint:   "/api/v1/query",
			params:     url.Values{"query": {`time()`}, "time": {"-62135596800"}},
			resultType: "scalar",
			want:       `[-6795364578.871,"-6795364578.871"]`,
		},
		{
			name:     "year-one range start",
			endpoint: "/api/v1/query_range",
			params: url.Values{
				"query": {`count_over_time(vector(1)[5m:1m])`},
				"start": {"-62135596800"}, "end": {"-62135596740"}, "step": {"60"},
			},
			resultType: "matrix",
			want:       `[{"metric":{},"values":[[-6795364578.871,"5"],[-6795364518.871,"5"]]}]`,
		},
		{
			// Prometheus converts -1e19 seconds to time.Unix(MinInt64, 0),
			// whose Unix milliseconds wrap to 0: it evaluates at the epoch.
			name:       "request time beyond int64 milliseconds",
			endpoint:   "/api/v1/query",
			params:     url.Values{"query": {`count_over_time(vector(1)[5m:1m])`}, "time": {"-1e19"}},
			resultType: "vector",
			want:       `[{"metric":{},"value":[0,"5"]}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{err: errors.New("no sample read is expected")}
			srv := newServer(q)
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.endpoint + "?" + tc.params.Encode())
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var parsed struct {
				Data struct {
					ResultType string          `json:"resultType"`
					Result     json.RawMessage `json:"result"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
			if parsed.Data.ResultType != tc.resultType || string(parsed.Data.Result) != tc.want {
				t.Errorf("got %s %s, want %s %s", parsed.Data.ResultType, parsed.Data.Result, tc.resultType, tc.want)
			}
			if len(q.allSQL) != 0 {
				t.Errorf("issued %d SQL statements for a read outside the storage domain: %v", len(q.allSQL), q.allSQL)
			}
		})
	}
}

// TestQuery_OutsideEvaluationDomain_ReadsThroughClickHouse pins the data
// half of the reference evaluation: a selector whose window reaches below
// 1900 reads its in-domain part through the ordinary pipeline, and a read
// failure surfaces as cerberus classifies it rather than as an empty
// answer.
func TestQuery_OutsideEvaluationDomain_ReadsThroughClickHouse(t *testing.T) {
	t.Parallel()
	// 1920-01-01: the window reaches below 1900, and what remains of it
	// once clipped to the storage domain is one sample-read piece.
	eval := time.Date(1920, 1, 1, 0, 0, 0, 0, time.UTC)
	params := url.Values{"query": {`count_over_time(up[50y])`}, "time": {"-1577923200"}}

	t.Run("samples", func(t *testing.T) {
		t.Parallel()
		q := &stubQuerier{samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "a"}, Timestamp: eval.Add(-2 * time.Minute), Value: 1},
			{MetricName: "up", Labels: map[string]string{"job": "a"}, Timestamp: eval.Add(-time.Minute), Value: 1},
			{MetricName: "up", Labels: map[string]string{"job": "b"}, Timestamp: eval, Value: 1},
		}}
		srv := newServer(q)
		t.Cleanup(srv.Close)
		resp, err := http.Get(srv.URL + "/api/v1/query?" + params.Encode())
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		body := readBody(t, resp)
		const want = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"a"},"value":[-1577923200,"2"]},{"metric":{"job":"b"},"value":[-1577923200,"1"]}]}}`
		if resp.StatusCode != http.StatusOK || body != want+"\n" {
			t.Errorf("status=%d body=%s, want %s", resp.StatusCode, body, want)
		}
		if len(q.allSQL) != 1 {
			t.Errorf("issued %d SQL statements, want 1", len(q.allSQL))
		}
	})

	t.Run("read failure", func(t *testing.T) {
		t.Parallel()
		q := &stubQuerier{err: errors.New("clickhouse is down")}
		srv := newServer(q)
		t.Cleanup(srv.Close)
		resp, err := http.Get(srv.URL + "/api/v1/query?" + params.Encode())
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		body := readBody(t, resp)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("status=200 body=%s for a failed read, want an error", body)
		}
	})
}
