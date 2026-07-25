package migrateinventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// lokiIndexStatsPath is Loki's real, bounded per-selector cardinality/volume
// endpoint. Loki exposes no single ranked top-N call analogous to
// Prometheus's /api/v1/status/tsdb; index/stats answers "how much does THIS
// stream selector match," so an operator-supplied selector set is what gets
// ranked, never a whole-tenant scan.
const lokiIndexStatsPath = "/loki/api/v1/index/stats"

// LokiInventory is the optional per-selector cardinality/volume section for a
// Loki source. Unlike Prometheus's ranked whole-TSDB status/tsdb call, Loki
// exposes no global top-N cardinality endpoint — its index/stats API answers
// "how much does THIS stream selector match" — so the operator supplies the
// selectors to probe and this section ranks only across that supplied set.
// Loki's label/series enumeration endpoints (/loki/api/v1/labels,
// /label/<name>/values, /series) are deliberately not probed here: they
// return raw sets with no built-in count, so ranking them would require
// unbounded client-side enumeration with no natural top-N boundary.
type LokiInventory struct {
	Source    string              `json:"source"`
	Window    string              `json:"window"`
	Selectors []LokiSelectorStats `json:"selectors"` // sorted by Streams desc
	Notes     []string            `json:"notes"`
}

// LokiSelectorStats is one operator-supplied selector's index/stats result.
type LokiSelectorStats struct {
	Selector string `json:"selector"`
	Streams  uint64 `json:"streams"`
	Chunks   uint64 `json:"chunks"`
	Entries  uint64 `json:"entries"`
	Bytes    uint64 `json:"bytes"`
}

// lokiIndexStatsResponse is /loki/api/v1/index/stats's JSON envelope: a flat
// object with no "status" wrapper, matching Loki's documented shape (unlike
// the Prometheus HTTP API's {status, data} envelope).
type lokiIndexStatsResponse struct {
	Streams uint64 `json:"streams"`
	Chunks  uint64 `json:"chunks"`
	Entries uint64 `json:"entries"`
	Bytes   uint64 `json:"bytes"`
}

// LokiClient probes a Loki-compatible HTTP API for per-selector stream
// cardinality/volume.
type LokiClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewLokiClient builds a probe client for baseURL, with the same bounded
// default HTTP client the Prometheus Client uses.
func NewLokiClient(baseURL string) *LokiClient {
	return &LokiClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// Probe ranks selectors by streams matched, issuing one independent
// /loki/api/v1/index/stats call per selector. window, if non-empty, is a
// duration parsed once and applied as the real [now-window, now) query range
// for every selector's call — unlike the Prometheus Client's Window (recorded
// verbatim, cosmetic only against a point-in-time snapshot), Loki's
// index/stats endpoint takes a real start/end range, so window here actually
// bounds the query. An empty window omits start/end, letting the server apply
// its own default range.
//
// Zero selectors is a validation error raised before any HTTP call — an
// operator who points --loki-source at a Loki instance must also name at
// least one --loki-selector, since there is no way to drive this API without
// one; returning an empty section here would misrepresent "nothing
// configured" as "checked, nothing found." If every selector's call fails,
// that is a hard error for the same reason: real work was attempted and
// nothing came back, which must not read as a clean, empty top-N. A PARTIAL
// failure (some selectors succeed, others don't) instead appends the failures
// to Notes and still ranks and truncates the selectors that did succeed.
func (c *LokiClient) Probe(ctx context.Context, selectors []string, window string, top int) (LokiInventory, error) {
	if len(selectors) == 0 {
		return LokiInventory{}, errors.New(
			"loki inventory requires at least one --loki-selector (or CERBERUS_INVENTORY_LOKI_SELECTORS entry)",
		)
	}
	if top <= 0 {
		return LokiInventory{}, fmt.Errorf("top must be positive, got %d", top)
	}

	startParam, endParam, err := lokiWindowParams(window)
	if err != nil {
		return LokiInventory{}, err
	}

	inv := LokiInventory{Source: c.BaseURL, Window: window}

	var failed []string
	for _, sel := range selectors {
		stats, err := c.fetchIndexStats(ctx, sel, startParam, endParam)
		if err != nil {
			failed = append(failed, sel)
			inv.Notes = append(inv.Notes, fmt.Sprintf("selector %s: %s unavailable: %v", sel, lokiIndexStatsPath, err))
			continue
		}
		inv.Selectors = append(inv.Selectors, LokiSelectorStats{
			Selector: sel,
			Streams:  stats.Streams,
			Chunks:   stats.Chunks,
			Entries:  stats.Entries,
			Bytes:    stats.Bytes,
		})
	}

	if len(failed) == len(selectors) {
		return LokiInventory{}, fmt.Errorf("all %d loki selector(s) failed %s: %s",
			len(selectors), lokiIndexStatsPath, strings.Join(failed, ", "))
	}

	rankSelectorsByStreams(inv.Selectors)
	if len(inv.Selectors) > top {
		inv.Selectors = inv.Selectors[:top]
	}

	return inv, nil
}

// lokiWindowParams parses window (if non-empty) into the [start, end) epoch
// nanosecond strings index/stats expects, anchored to a single "now" so every
// selector in one Probe call is compared over the identical range. An empty
// window yields empty params, letting the server apply its own default.
func lokiWindowParams(window string) (startParam, endParam string, err error) {
	if window == "" {
		return "", "", nil
	}
	dur, err := time.ParseDuration(window)
	if err != nil {
		return "", "", fmt.Errorf("window %q is not a valid duration: %w", window, err)
	}
	end := time.Now().UTC()
	start := end.Add(-dur)
	return strconv.FormatInt(start.UnixNano(), 10), strconv.FormatInt(end.UnixNano(), 10), nil
}

// rankSelectorsByStreams sorts stats descending by Streams (ties broken by
// Selector ascending for determinism), matching rankTopN's discipline for the
// Prometheus ranked tables: never trust the server's ordering, always re-rank.
func rankSelectorsByStreams(stats []LokiSelectorStats) {
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Streams != stats[j].Streams {
			return stats[i].Streams > stats[j].Streams
		}
		return stats[i].Selector < stats[j].Selector
	})
}

// fetchIndexStats issues GET {base}/loki/api/v1/index/stats?query=<selector>
// (plus start/end when startParam is set) and decodes the flat
// {streams, chunks, entries, bytes} response.
func (c *LokiClient) fetchIndexStats(ctx context.Context, selector, startParam, endParam string) (lokiIndexStatsResponse, error) {
	q := url.Values{}
	q.Set("query", selector)
	if startParam != "" {
		q.Set("start", startParam)
		q.Set("end", endParam)
	}

	body, err := c.getOK(ctx, lokiIndexStatsPath+"?"+q.Encode())
	if err != nil {
		return lokiIndexStatsResponse{}, err
	}
	var raw lokiIndexStatsResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return lokiIndexStatsResponse{}, fmt.Errorf("decode %s response: %w", lokiIndexStatsPath, err)
	}
	return raw, nil
}

// getOK issues GET {base}{path} and returns the body only on HTTP 200,
// mirroring the Prometheus Client's capped-read, non-200-is-error discipline.
func (c *LokiClient) getOK(ctx context.Context, path string) ([]byte, error) {
	reqURL := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request %s: %w", path, err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readCappedBody(resp.Body, maxResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("read %s body: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return body, nil
}
