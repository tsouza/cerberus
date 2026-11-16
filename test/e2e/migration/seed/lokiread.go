package seed

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

const (
	// LogQueryLimit is the entry limit of a log range query that reads a
	// whole fixture stream back: reference Loki's own per-query entry cap
	// (`max_entries_limit_per_query`, 5000 by default), far above the
	// records one stream holds, so the limit never truncates the answer.
	LogQueryLimit = 5000
	// logQueryTimeout bounds one log range query.
	logQueryTimeout = 15 * time.Second
	// logQueryBodyLimit caps a log range-query response body: one stream's
	// whole fixture window, with room to spare, so a cap that truncated it
	// would surface as a decode error rather than a short stream.
	logQueryBodyLimit = 32 << 20
)

// LogSelector renders labels as the LogQL stream selector that matches
// exactly that stream, keys in sorted order.
func LogSelector(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// LogRangeStream is one stream of a Loki-API log range-query answer: its
// label set and its [<unix nanos>, <line>, …] values.
type LogRangeStream struct {
	Labels map[string]string `json:"stream"`
	Values [][]string        `json:"values"`
}

// logRangeResponse is the subset of a Loki-API range-query response the
// readers here decode.
type logRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []LogRangeStream `json:"result"`
	} `json:"data"`
}

// QueryLogRange runs the forward log range query selector over [start, end)
// against a Loki-API endpoint and returns the streams it answered with.
func QueryLogRange(ctx context.Context, baseURL, selector string, start, end time.Time) ([]LogRangeStream, error) {
	target := strings.TrimRight(baseURL, "/") + "/loki/api/v1/query_range?" + url.Values{
		"query":     {selector},
		"start":     {strconv.FormatInt(start.UnixNano(), 10)},
		"end":       {strconv.FormatInt(end.UnixNano(), 10)},
		"limit":     {strconv.Itoa(LogQueryLimit)},
		"direction": {"forward"},
	}.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, logQueryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, logQueryBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read %s body: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d: %s", target, resp.StatusCode, truncate(string(body)))
	}
	var got logRangeResponse
	if err := json.Unmarshal(body, &got); err != nil {
		return nil, fmt.Errorf("decode %s body %s: %w", target, truncate(string(body)), err)
	}
	if got.Status != "success" {
		return nil, fmt.Errorf("%s returned status %q, want %q", target, got.Status, "success")
	}
	return got.Data.Result, nil
}
