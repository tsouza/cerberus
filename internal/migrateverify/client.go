package migrateverify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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

// maxResponseBytes caps how much of a backend response body verify will buffer.
// A range response is bounded in practice (matrix JSON over one window), so a
// body beyond this is a misbehaving or wrong endpoint, not a valid parity
// response; failing loudly beats letting an unbounded stream OOM the gate.
const maxResponseBytes = 256 << 20 // 256 MiB

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
	// bearerToken, when set, is sent as an Authorization: Bearer header — the
	// credential-clean alternative to embedding user:pass@ in BaseURL (which
	// would leak into repro lines and report artifacts).
	bearerToken string
}

// BackendOption configures an HTTPBackend at construction.
type BackendOption func(*HTTPBackend)

// WithBearerToken sends an "Authorization: Bearer <token>" header on every
// request. It is the clean auth path: credentials travel in a header, never in
// the URL, so they cannot leak into the repro command or the report JSON. An
// empty token is a no-op, so a caller can pass a possibly-unset flag value
// unconditionally.
func WithBearerToken(token string) BackendOption {
	return func(b *HTTPBackend) { b.bearerToken = token }
}

// NewHTTPBackend builds a backend for baseURL with a bounded default client.
func NewHTTPBackend(baseURL string, opts ...BackendOption) *HTTPBackend {
	b := &HTTPBackend{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: defaultHTTPTimeout},
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// redactedUserinfo is the placeholder that replaces any user:pass@ userinfo in a
// redacted URL: it removes the secret while keeping the redaction visible (an
// operator can see credentials WERE present, just not what they were).
const redactedUserinfo = "REDACTED"

// RedactURL strips any userinfo (user:pass@) from a URL so basic-auth
// credentials embedded in --ref / --cerberus can never leak into a printed repro
// line, the --report JSON, or any other artifact. Credentials belong in the
// Authorization header (see WithBearerToken); when an operator nonetheless puts
// them in the URL, this keeps them out of every artifact. A string that does not
// parse as a URL, or carries no userinfo, is returned unchanged.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User(redactedUserinfo)
	return u.String()
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
	q.Set("step", formatStep(p.Step))
	reqURL := b.BaseURL + queryRangePath + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return RangeResult{}, fmt.Errorf("build request: %w", err)
	}
	if b.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.bearerToken)
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return RangeResult{}, fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readCappedBody(resp.Body, maxResponseBytes)
	if err != nil {
		return RangeResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return RangeResult{Status: resp.StatusCode}, nil
	}
	return parseRangeBody(resp.StatusCode, body)
}

// readCappedBody reads r fully but no further than limit bytes, erroring rather
// than buffering an unbounded stream: it reads one byte past the limit so an
// over-limit body is detected instead of silently truncated into a mis-parse.
// This bounds cerberus-process memory against a misbehaving or wrong backend.
func readCappedBody(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response body exceeds the %d-byte cap", limit)
	}
	return body, nil
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

// formatStep renders the range step as Prometheus-style float seconds, keeping
// sub-second precision (e.g. 1500ms → "1.5", 500ms → "0.5") so the replayed
// window matches the operator's requested step exactly instead of truncating to
// whole seconds.
func formatStep(step time.Duration) string {
	return strconv.FormatFloat(step.Seconds(), 'f', -1, 64)
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
	if err := validateTolerance(tolerance); err != nil {
		return Params{}, err
	}
	return Params{Start: start, End: end, Step: step, Tolerance: tolerance}, nil
}

// validateTolerance rejects a tolerance that would corrupt the gate rather than
// merely loosen it: a NaN/Inf comparison never holds (or always holds), a
// negative tolerance makes every value diverge, and an absurdly large one
// (>= maxVerifyTolerance) silently blesses real divergences. A fat-fingered
// --tolerance must fail loudly here, never ride through into a clean-looking
// verify.json the cutover gate then trusts.
func validateTolerance(tol float64) error {
	switch {
	case math.IsNaN(tol) || math.IsInf(tol, 0):
		return fmt.Errorf("tolerance must be a finite number, got %v", tol)
	case tol < 0:
		return fmt.Errorf("tolerance must be non-negative, got %v", tol)
	case tol >= maxVerifyTolerance:
		return fmt.Errorf("tolerance %v is too loose (must be < %v): a tolerance this large would bless real divergences", tol, maxVerifyTolerance)
	}
	return nil
}

// LoadCorpus reads a corpus.json produced by `migrate harvest`, splits it into
// the PromQL queries to replay and the non-PromQL entries carried through for
// honest accounting, carries through the harvest-time skips the corpus recorded,
// and rejects a corpus whose version this build does not understand. Every entry
// is accounted for — none is silently dropped: PromQL queries are replayed,
// non-PromQL queries are counted out of scope, and the harvester's own skips are
// surfaced so the operator sees queries that never became replayable at all.
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
	for _, s := range c.Skipped {
		out.HarvestSkipped = append(out.HarvestSkipped, HarvestSkippedEntry{Source: s.Source, Reason: s.Reason})
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
