package migrateverify

import (
	"context"
	"encoding/json"
	"errors"
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

// HTTPBackend issues range queries against one head's HTTP API. The head-specific
// wire contract — endpoint path, query-string encoding, response decoding — is
// supplied by a Dialect; everything security-relevant (auth headers, credential
// redaction, the response-size cap, the non-200 rule) is written once here and
// therefore holds identically for every head.
type HTTPBackend struct {
	BaseURL string
	HTTP    *http.Client
	// bearerToken, when set, is sent as an Authorization: Bearer header — the
	// credential-clean alternative to embedding user:pass@ in BaseURL (which
	// would leak into repro lines and report artifacts).
	bearerToken string
	// orgID, when set, is sent as X-Scope-OrgID — the tenant header a
	// multi-tenant reference Loki / Tempo requires.
	orgID string
	// dialect is the head wire contract this backend speaks.
	dialect Dialect
	// kinds are the non-matrix wire contracts this backend speaks, keyed by
	// result kind. A backend asked for a kind it has no dialect for fails
	// loudly rather than issuing a request to a guessed endpoint.
	kinds map[string]KindDialect
}

// BackendOption configures an HTTPBackend at construction.
type BackendOption func(*HTTPBackend)

// WithDialect makes the backend speak a head's wire contract (endpoint path,
// query-string encoding, response decoding). Without it a backend speaks the
// Prometheus dialect.
func WithDialect(d Dialect) BackendOption {
	return func(b *HTTPBackend) { b.dialect = d }
}

// WithKindDialects teaches the backend the non-matrix wire contracts it speaks.
// The SAME backend serves them: log-stream, trace and discovery probes hang off
// the same base URL, token and tenant the operator already supplied for that
// head, so a lane never needs a second connection or a second flag.
func WithKindDialects(ds ...KindDialect) BackendOption {
	return func(b *HTTPBackend) {
		if b.kinds == nil {
			b.kinds = map[string]KindDialect{}
		}
		for _, d := range ds {
			b.kinds[d.Kind()] = d
		}
	}
}

// WithOrgID sends an "X-Scope-OrgID: <id>" header on every request, so a
// reference Loki / Tempo running with auth_enabled answers instead of 401ing the
// whole lane into VerdictError. Like WithBearerToken it travels in a header, never
// in the URL, and is never recorded in any artifact. An empty id is a no-op.
func WithOrgID(id string) BackendOption {
	return func(b *HTTPBackend) { b.orgID = id }
}

// WithBearerToken sends an "Authorization: Bearer <token>" header on every
// request. It is the clean auth path: credentials travel in a header, never in
// the URL, so they cannot leak into the repro command or the report JSON. An
// empty token is a no-op, so a caller can pass a possibly-unset flag value
// unconditionally.
func WithBearerToken(token string) BackendOption {
	return func(b *HTTPBackend) { b.bearerToken = token }
}

// NewHTTPBackend builds a backend for baseURL with a bounded default client,
// speaking the Prometheus dialect unless WithDialect says otherwise.
func NewHTTPBackend(baseURL string, opts ...BackendOption) *HTTPBackend {
	b := &HTTPBackend{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: defaultHTTPTimeout},
		dialect: promDialect{},
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

// redactTransportErr strips any basic-auth userinfo from a backend transport
// error before it can reach an artifact. Go's http client wraps a request
// failure in a *url.Error whose URL keeps the USERNAME (only the password is
// masked by stripPassword), so a token-as-username embedded in --ref / --cerberus
// would leak in full into the error string — which lands in the gate-consumed
// verify.json AND the --report file operators attach to bug reports. Rebuilding
// the *url.Error with a RedactURL'd URL removes it; a non-url.Error carries no
// URL and is returned unchanged. Redacting here, at the client boundary, means
// every error string a verdict Detail can quote is already clean.
func redactTransportErr(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return &url.Error{Op: uerr.Op, URL: RedactURL(uerr.URL), Err: uerr.Err}
	}
	return err
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

// QueryRange issues the dialect's range request with the shared params and
// decodes the response. An HTTP non-200 is returned as RangeResult{Status: code}
// (no series) rather than an error, so the caller can classify it as unsupported
// vs error; only transport and decode failures are returned as err.
func (b *HTTPBackend) QueryRange(ctx context.Context, expr string, p Params) (RangeResult, error) {
	status, body, err := b.do(ctx, b.dialect.Path(), b.dialect.Encode(expr, p), nil)
	if err != nil {
		return RangeResult{}, err
	}
	if status != http.StatusOK {
		return RangeResult{Status: status}, nil
	}
	res, err := b.dialect.Decode(body)
	if err != nil {
		return RangeResult{}, err
	}
	res.Status = status
	return res, nil
}

// Fetch issues one non-matrix probe. It routes through the SAME request helper
// QueryRange uses, so auth headers, the response cap, credential redaction and
// the non-200 rule hold identically on every surface. A non-200 is returned as
// FetchResult{Status: code} with no payload, so the caller classifies
// unsupported vs error by the same rule the matrix path applies.
func (b *HTTPBackend) Fetch(ctx context.Context, q Query, p Params) (FetchResult, error) {
	d, ok := b.kinds[q.Kind]
	if !ok {
		return FetchResult{}, fmt.Errorf("this backend speaks no wire contract for result kind %q", q.Kind)
	}
	status, body, err := b.do(ctx, d.Path(q), d.Encode(q, p), d.Header())
	if err != nil {
		return FetchResult{}, err
	}
	if status != http.StatusOK {
		return FetchResult{Status: status}, nil
	}
	payload, err := d.Decode(body)
	if err != nil {
		return FetchResult{}, err
	}
	return FetchResult{Status: status, Payload: payload}, nil
}

// do issues one GET against the backend and returns the status and the capped
// body. It is the single place every request passes through, so the auth header,
// the tenant header, credential redaction and the response-size cap are written
// once and hold for every endpoint any dialect names.
func (b *HTTPBackend) do(ctx context.Context, path string, params url.Values, extra http.Header) (int, []byte, error) {
	reqURL := b.BaseURL + path + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", redactTransportErr(err))
	}
	if b.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.bearerToken)
	}
	if b.orgID != "" {
		req.Header.Set(scopeOrgIDHeader, b.orgID)
	}
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("do request: %w", redactTransportErr(err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readCappedBody(resp.Body, maxResponseBytes)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
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

// decodePromMatrix decodes a 200 range body carrying the standard Prometheus
// matrix envelope into a RangeResult. A body that is not valid JSON is a decode
// error; a valid body with a non-matrix resultType is carried through
// (ResultType) for the caller to classify. Status is stamped by the caller.
//
// The Loki lane shares this decoder rather than duplicating it: Loki's `matrix`
// result is literally []prometheus/common/model.SampleStream, the same Go type
// Prometheus marshals, so the envelope is byte-compatible. Loki's extra
// stats / warnings / encodingFlags fields are simply not read.
func decodePromMatrix(body []byte) (RangeResult, error) {
	var raw promRangeResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return RangeResult{}, fmt.Errorf("decode range response: %w", err)
	}
	res := RangeResult{ResultType: raw.Data.ResultType}
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

// LoadCorpus reads a corpus.json produced by `migrate harvest`, routes every
// entry to its head's lane or to the out-of-scope accounting, carries through
// the harvest-time skips the corpus recorded, and rejects a corpus whose version
// this build does not understand.
//
// Every entry is accounted for — none is silently dropped. A query whose shape a
// comparator can judge is replayed and tagged with its head and its result kind;
// a shape with no definition of equality is counted out of scope with the
// specific reason it was not judged; and the harvester's own skips are surfaced
// so the operator also sees queries that never became replayable at all. Corpus
// order is preserved so the report reads in the order the operator's sources
// were harvested.
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
		r := RouteQuery(q.Lang, q.Expr)
		if r.Replayable {
			// The routed KIND is carried through, not discarded: it is what selects
			// the comparator that judges this entry's shape, and it is the same
			// vocabulary the out-of-scope accounting below names shapes with.
			out.Queries = append(out.Queries, Query{Expr: q.Expr, Source: q.Source, Head: r.Head, Lang: q.Lang, Kind: r.Kind})
			continue
		}
		out.OutOfScope = append(out.OutOfScope, OutOfScopeEntry{
			Source: q.Source, Expr: q.Expr, Head: r.Head, Lang: q.Lang, Kind: r.Kind, Reason: r.Reason,
		})
	}
	for _, s := range c.Skipped {
		out.HarvestSkipped = append(out.HarvestSkipped, HarvestSkippedEntry{Source: s.Source, Reason: s.Reason})
	}
	return out, nil
}

// WriteJSON renders the report as machine-readable JSON with a trailing newline.
// It stamps the current schema version so every artifact on disk is self-describing
// and the cutover gate can refuse a report shape it does not understand.
func (r Report) WriteJSON(w io.Writer) error {
	r.SchemaVersion = ReportVersion
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	return nil
}
