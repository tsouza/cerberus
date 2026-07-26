package lib

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// promHTTPTimeout bounds one Prometheus-API HTTP round trip.
const promHTTPTimeout = 15 * time.Second

// promBodyErrLimit caps how much of a failing response body a query error
// quotes back, so a large HTML error page does not flood a scenario failure.
const promBodyErrLimit = 4096

// PromSample is one instant/range-query data point, decoded from the wire's
// `[unixSeconds, "stringValue"]` pair.
type PromSample struct {
	Time  time.Time
	Value float64
}

// PromSeries is one series a query returned: its label set plus every sample
// on it.
type PromSeries struct {
	Labels  map[string]string
	Samples []PromSample
}

// QueryRange runs a Prometheus-API range query and decodes the matrix
// result. It requires resultType "matrix" — a query whose shape does not
// even reach a matrix is a scenario-construction bug, not a divergence to
// diff, so this fails loudly rather than returning an empty result set.
func QueryRange(ctx context.Context, baseURL, query string, start, end time.Time, step time.Duration) ([]PromSeries, error) {
	target := strings.TrimRight(baseURL, "/") + "/api/v1/query_range?" + url.Values{
		"query": {query},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatInt(int64(step.Seconds()), 10)},
	}.Encode()

	var decoded struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string    `json:"metric"`
				Values [][2]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := fetchJSON(ctx, target, &decoded); err != nil {
		return nil, err
	}
	if decoded.Status != "success" {
		return nil, fmt.Errorf("GET %s returned status %q: %s", target, decoded.Status, decoded.Error)
	}
	if decoded.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("GET %s returned resultType %q, want %q", target, decoded.Data.ResultType, "matrix")
	}

	out := make([]PromSeries, 0, len(decoded.Data.Result))
	for _, r := range decoded.Data.Result {
		samples := make([]PromSample, 0, len(r.Values))
		for _, pair := range r.Values {
			s, err := decodeSample(pair)
			if err != nil {
				return nil, fmt.Errorf("GET %s: %w", target, err)
			}
			samples = append(samples, s)
		}
		out = append(out, PromSeries{Labels: r.Metric, Samples: samples})
	}
	return out, nil
}

// decodeSample decodes one `[unixSeconds, "value"]` wire pair.
func decodeSample(pair [2]json.RawMessage) (PromSample, error) {
	var unixSeconds float64
	if err := json.Unmarshal(pair[0], &unixSeconds); err != nil {
		return PromSample{}, fmt.Errorf("decode sample timestamp: %w", err)
	}
	var valueStr string
	if err := json.Unmarshal(pair[1], &valueStr); err != nil {
		return PromSample{}, fmt.Errorf("decode sample value: %w", err)
	}
	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return PromSample{}, fmt.Errorf("parse sample value %q: %w", valueStr, err)
	}
	nanos := int64((unixSeconds - float64(int64(unixSeconds))) * float64(time.Second))
	return PromSample{Time: time.Unix(int64(unixSeconds), nanos).UTC(), Value: value}, nil
}

// QueryInstantSeries runs a Prometheus-API instant query at t and decodes the
// vector result into the same PromSeries shape QueryRange returns, so a caller
// can hold a range answer and an instant answer in one type. Like QueryRange,
// it requires resultType "vector". Distinct from probe.go's QueryInstant,
// which returns a sortable ProbeVector plus the dialled request URL — the
// shape the cutover scenarios compare backend-to-backend.
func QueryInstantSeries(ctx context.Context, baseURL, query string, t time.Time) ([]PromSeries, error) {
	target := strings.TrimRight(baseURL, "/") + "/api/v1/query?" + url.Values{
		"query": {query},
		"time":  {strconv.FormatInt(t.Unix(), 10)},
	}.Encode()

	var decoded struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string  `json:"metric"`
				Value  [2]json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := fetchJSON(ctx, target, &decoded); err != nil {
		return nil, err
	}
	if decoded.Status != "success" {
		return nil, fmt.Errorf("GET %s returned status %q: %s", target, decoded.Status, decoded.Error)
	}
	if decoded.Data.ResultType != "vector" {
		return nil, fmt.Errorf("GET %s returned resultType %q, want %q", target, decoded.Data.ResultType, "vector")
	}

	out := make([]PromSeries, 0, len(decoded.Data.Result))
	for _, r := range decoded.Data.Result {
		s, err := decodeSample(r.Value)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", target, err)
		}
		out = append(out, PromSeries{Labels: r.Metric, Samples: []PromSample{s}})
	}
	return out, nil
}

// fetchJSON GETs url, requiring a 200 with a JSON body, and decodes it into
// out.
func fetchJSON(ctx context.Context, target string, out any) error {
	reqCtx, cancel := context.WithTimeout(ctx, promHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// The success path decodes the FULL body — a query_range matrix can
	// easily exceed a few KB — and only the error path truncates, so a large
	// failing response does not flood a scenario failure message.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s body: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d: %s", target, resp.StatusCode, truncate(body, promBodyErrLimit))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s body %q: %w", target, truncate(body, promBodyErrLimit), err)
	}
	return nil
}

// truncate caps how much of body an error message quotes.
func truncate(body []byte, limit int) string {
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "…(truncated)"
}
