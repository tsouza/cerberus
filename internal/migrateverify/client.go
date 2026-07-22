package migrateverify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tsouza/cerberus/internal/migrate"
)

// queryRangePath is the Prometheus HTTP API range-query endpoint, appended to
// each backend's base URL.
const queryRangePath = "/api/v1/query_range"

// defaultHTTPTimeout bounds a single backend request so one hung query can't
// stall the whole gate.
const defaultHTTPTimeout = 30 * time.Second

// Params are the shared query_range parameters replayed identically against both
// backends, plus the comparison tolerance.
type Params struct {
	Start     time.Time
	End       time.Time
	Step      time.Duration
	Tolerance float64
}

// HTTPBackend issues range queries against a Prometheus-compatible HTTP API.
type HTTPBackend struct {
	BaseURL string
	HTTP    *http.Client
}

// NewHTTPBackend builds a backend for baseURL with a bounded default client.
func NewHTTPBackend(baseURL string) *HTTPBackend {
	return &HTTPBackend{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// promRangeResponse is the standard Prometheus range-query JSON envelope.
type promRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     []promRawSeries `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// promRawSeries is one series: a label set plus [ [ts, "value"], ... ].
type promRawSeries struct {
	Metric map[string]string    `json:"metric"`
	Values [][2]json.RawMessage `json:"values"`
}

// QueryRange issues GET {base}/api/v1/query_range with the shared params and
// parses the response. An HTTP non-200 is returned as RangeResult{Status: code}
// (no series) rather than an error, so the caller can classify it as unsupported
// vs error; only transport and decode failures are returned as err.
func (b *HTTPBackend) QueryRange(ctx context.Context, expr string, p Params) (RangeResult, error) {
	q := url.Values{}
	q.Set("query", expr)
	q.Set("start", formatTimestamp(p.Start))
	q.Set("end", formatTimestamp(p.End))
	q.Set("step", strconv.FormatInt(int64(p.Step.Seconds()), 10))
	reqURL := b.BaseURL + queryRangePath + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return RangeResult{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return RangeResult{}, fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return RangeResult{}, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return RangeResult{Status: resp.StatusCode}, nil
	}
	return parseRangeBody(resp.StatusCode, body)
}

// parseRangeBody decodes a 200 range body into a RangeResult. A body that is not
// valid JSON is a decode error; a valid body with a non-matrix resultType is
// carried through (Status + ResultType) for the caller to classify.
func parseRangeBody(status int, body []byte) (RangeResult, error) {
	var raw promRangeResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return RangeResult{}, fmt.Errorf("decode range response: %w", err)
	}
	res := RangeResult{Status: status, ResultType: raw.Data.ResultType}
	if raw.Data.ResultType != resultTypeMatrix {
		return res, nil
	}
	for _, rs := range raw.Data.Result {
		s := Series{Labels: rs.Metric, Samples: make([]Sample, 0, len(rs.Values))}
		for _, pair := range rs.Values {
			ts, val, err := parseSamplePair(pair)
			if err != nil {
				return RangeResult{}, err
			}
			s.Samples = append(s.Samples, Sample{T: ts, V: val})
		}
		res.Series = append(res.Series, s)
	}
	return res, nil
}

// parseSamplePair decodes one [ts, "value"] point. The timestamp is a JSON
// number; the value is a JSON string (Prometheus encodes values as strings so
// NaN / Inf round-trip).
func parseSamplePair(pair [2]json.RawMessage) (float64, float64, error) {
	var ts float64
	if err := json.Unmarshal(pair[0], &ts); err != nil {
		return 0, 0, fmt.Errorf("decode sample timestamp: %w", err)
	}
	var valStr string
	if err := json.Unmarshal(pair[1], &valStr); err != nil {
		return 0, 0, fmt.Errorf("decode sample value: %w", err)
	}
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse sample value %q: %w", valStr, err)
	}
	return ts, val, nil
}

// formatTimestamp renders an instant as Unix seconds for the query_range API.
func formatTimestamp(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

// ParseTime resolves a start/end value that may be RFC3339, a Unix-seconds
// integer, the literal "now", or a relative offset ("-1h", "+30m", "now-15m").
// Relative values are resolved against now, which the caller pins for
// determinism.
func ParseTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time value")
	}
	if s == "now" {
		return now, nil
	}
	if rest, ok := strings.CutPrefix(s, "now"); ok {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return time.Time{}, fmt.Errorf("relative time %q: %w", s, err)
		}
		return now.Add(d), nil
	}
	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		d, err := time.ParseDuration(s)
		if err != nil {
			return time.Time{}, fmt.Errorf("relative time %q: %w", s, err)
		}
		return now.Add(d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("time %q: not RFC3339, a Unix timestamp, or a relative offset", s)
}

// BuildParams resolves the raw string inputs into validated Params. now pins the
// resolution of any relative start/end so the result is reproducible.
func BuildParams(startStr, endStr, stepStr string, tolerance float64, now time.Time) (Params, error) {
	start, err := ParseTime(startStr, now)
	if err != nil {
		return Params{}, fmt.Errorf("start: %w", err)
	}
	end, err := ParseTime(endStr, now)
	if err != nil {
		return Params{}, fmt.Errorf("end: %w", err)
	}
	if !end.After(start) {
		return Params{}, fmt.Errorf("end (%s) must be after start (%s)", end.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	step, err := time.ParseDuration(stepStr)
	if err != nil {
		return Params{}, fmt.Errorf("step %q: %w", stepStr, err)
	}
	if step <= 0 {
		return Params{}, fmt.Errorf("step must be positive, got %s", step)
	}
	return Params{Start: start, End: end, Step: step, Tolerance: tolerance}, nil
}

// LoadCorpus reads a corpus.json produced by `migrate harvest`, splits it into
// the PromQL queries to replay and the non-PromQL entries carried through for
// honest accounting, and rejects a corpus whose version this build does not
// understand. Every query is accounted for — none is silently dropped.
func LoadCorpus(path string) (Corpus, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied corpus path; offline CLI input.
	if err != nil {
		return Corpus{}, fmt.Errorf("read corpus %q: %w", path, err)
	}
	var c migrate.Corpus
	if err := json.Unmarshal(data, &c); err != nil {
		return Corpus{}, fmt.Errorf("decode corpus %q: %w", path, err)
	}
	if c.Version != migrate.CorpusVersion {
		return Corpus{}, fmt.Errorf("corpus %q has version %d, this build understands version %d", path, c.Version, migrate.CorpusVersion)
	}
	out := Corpus{}
	for _, q := range c.Queries {
		if q.Lang == migrate.LangPromQL {
			out.PromQL = append(out.PromQL, Query{Expr: q.Expr, Source: q.Source})
			continue
		}
		out.OutOfScope = append(out.OutOfScope, OutOfScopeEntry{Source: q.Source, Lang: q.Lang})
	}
	return out, nil
}

// WriteJSON renders the report as machine-readable JSON with a trailing newline.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	return nil
}
