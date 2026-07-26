package lib

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// probeReadTimeout bounds one instant-query round trip.
const probeReadTimeout = 15 * time.Second

// probeBodyLimit caps how much of a probe's HTTP response is read.
const probeBodyLimit = 1 << 20

// ProbeSample is one series a probe query returned: its label set and its
// value at the queried instant.
type ProbeSample struct {
	Labels map[string]string
	Value  float64
}

// ProbeVector is a probe's full result, sorted into a deterministic order so
// two probes are comparable without the caller re-sorting.
type ProbeVector []ProbeSample

// probeInstantResponse is the subset of a Prometheus-wire instant-query
// response a probe reads. A reference Prometheus and cerberus both answer
// this exact shape — that agreement is the whole premise a CUTOVER scenario
// leans on: the same decoder reads either side of a flip.
type probeInstantResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// ProbeResult is one instant query's outcome: the exact request URL it
// dialled, so a caller can assert two probes against different backends
// differed only in host, plus the vector it decoded.
type ProbeResult struct {
	RequestURL string
	Vector     ProbeVector
}

// QueryInstant runs one Prometheus-wire instant query — GET /api/v1/query —
// against baseURL at at, and returns the exact request URL alongside the
// decoded vector. It is the mechanism a CUTOVER scenario drives directly,
// rather than composing a `cerberus migrate` CLI invocation: MIG-22 and
// MIG-23 are about what happens when a client's URL is retargeted, not about
// the differential verify report `cerberus migrate verify` produces — the
// same "dedicated comparator, not verify" posture MIG-20 already takes for a
// shape verify does not cover (see docs/migration-testing.md section 6,
// VERIFY scenarios).
func QueryInstant(ctx context.Context, baseURL, query string, at time.Time) (ProbeResult, error) {
	target := strings.TrimRight(baseURL, "/") + "/api/v1/query?" + url.Values{
		"query": {query},
		"time":  {strconv.FormatInt(at.Unix(), 10)},
	}.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, probeReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("migration harness: build probe request for %s: %w", target, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("migration harness: probe %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
	if err != nil {
		return ProbeResult{}, fmt.Errorf("migration harness: read probe response from %s: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return ProbeResult{}, fmt.Errorf("migration harness: probe %s returned %d: %s", target, resp.StatusCode, string(body))
	}
	var decoded probeInstantResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ProbeResult{}, fmt.Errorf("migration harness: decode probe response from %s: %w", target, err)
	}
	if decoded.Status != "success" {
		return ProbeResult{}, fmt.Errorf("migration harness: probe %s returned status %q, not success", target, decoded.Status)
	}
	vector := make(ProbeVector, 0, len(decoded.Data.Result))
	for _, r := range decoded.Data.Result {
		if len(r.Value) < 2 {
			return ProbeResult{}, fmt.Errorf("migration harness: probe %s returned a series with no sample value", target)
		}
		var raw string
		if err := json.Unmarshal(r.Value[1], &raw); err != nil {
			return ProbeResult{}, fmt.Errorf("migration harness: decode probe sample value from %s: %w", target, err)
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return ProbeResult{}, fmt.Errorf("migration harness: parse probe sample value %q from %s: %w", raw, target, err)
		}
		vector = append(vector, ProbeSample{Labels: r.Metric, Value: value})
	}
	sort.Slice(vector, func(i, j int) bool { return vector[i].labelKey() < vector[j].labelKey() })
	return ProbeResult{RequestURL: target, Vector: vector}, nil
}

// labelKey renders a sample's label set as a stable sort/comparison key.
func (s ProbeSample) labelKey() string {
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(s.Labels[k])
		b.WriteByte(',')
	}
	return b.String()
}

// Equal reports whether two probe vectors carry the same series at the same
// exact values — comparison mode 1 in docs/migration-testing.md section 5,
// the same "the same float" contract `cerberus migrate verify --tolerance`
// uses for its own float axis.
func (v ProbeVector) Equal(other ProbeVector) bool {
	if len(v) != len(other) {
		return false
	}
	for i := range v {
		if v[i].labelKey() != other[i].labelKey() || v[i].Value != other[i].Value {
			return false
		}
	}
	return true
}

// NonEmpty reports whether the vector carries at least one series, so a
// caller can tell "answered, but held nothing here" apart from "answered
// with real data" without leaning on transport success alone.
func (v ProbeVector) NonEmpty() bool { return len(v) > 0 }

// RequestPath returns a request URL's component after the scheme and
// authority — the part two probes against different hosts should share
// byte-for-byte when they differ only in which backend they dialled.
func RequestPath(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("migration harness: parse probe request url %q: %w", rawURL, err)
	}
	return u.RequestURI(), nil
}
