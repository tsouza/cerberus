// Package migrateinventory probes LIVE migration sources for the runtime
// cardinality facts that a migration preview cannot learn offline. Cerberus's
// offline `migrate explain` can show the SQL and physical tables a query
// touches, but the one thing that actually drives ClickHouse memory risk —
// how many series a metric fans out to, how wide a label is, how much a log
// stream churns — is data, not config; it lives only in the running source.
//
// Each head's client REFUSES to infer cardinality from static config —
// Prometheus's scrape config lists targets and relabel rules, not the
// realized series count after those rules run against real endpoints; Loki's
// config says nothing about how many streams a tenant actually writes. The
// only honest source is the running system itself:
//
//   - Client probes the source Prometheus's /api/v1/status/tsdb, which ranks
//     head-block cardinality — a point-in-time TSDB snapshot.
//   - LokiClient probes a Loki source's per-selector /loki/api/v1/index/stats
//     endpoint. Loki exposes no whole-tenant top-N cardinality call, so the
//     operator supplies the stream selectors to rank; the ranking covers
//     exactly that supplied set, by streams matched.
//   - Tempo has no client in this package. Its span/block storage exposes
//     neither a TSDB-style head snapshot (Prometheus) nor a per-selector
//     stats endpoint (Loki); its Inventory section is always the fixed,
//     specifically-reasoned out-of-scope entry in TempoInventory rather than
//     a fabricated cardinality number Tempo cannot honestly back.
//
// Honesty contract: everything here is a source runtime fact. It ranks OOM
// RISK — a high-cardinality metric, label, or selector is a candidate worth
// reviewing, never a prediction of cerberus's exact memory — and it never
// silently drops a caveat: enrichment and per-selector failures land in
// Notes, and a head the operator never asked about is simply absent from the
// report, never presented as "checked, nothing found."
package migrateinventory

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

// DefaultTop is the number of highest-cardinality metrics and labels ranked
// when --top is not supplied. It is a readable slice of the risk surface — big
// enough to catch the fat tail, small enough to scan by eye.
const DefaultTop = 50

// defaultHTTPTimeout bounds a single probe request so one hung source can't
// stall the whole inventory.
const defaultHTTPTimeout = 30 * time.Second

// maxResponseBytes caps how much of a probe response body the inventory will
// buffer. The TSDB-status and enrichment responses are bounded in practice, so a
// body beyond this cap signals a misbehaving or wrong endpoint, not a valid
// reply; failing loudly beats letting an unbounded stream OOM the process.
const maxResponseBytes = 256 << 20 // 256 MiB

// Prometheus HTTP API paths the probe talks to. status/tsdb is the mandatory
// cardinality source; the label-names and metadata endpoints are optional
// enrichment.
const (
	statusTSDBPath       = "/api/v1/status/tsdb"
	metricNamesPath      = "/api/v1/label/__name__/values"
	metadataPath         = "/api/v1/metadata"
	promStatusSuccessful = "success"
)

// Options controls a probe: how many entries to rank and an optional
// operator-declared observation window recorded as report context.
type Options struct {
	// Top ranks the highest-cardinality N metrics and labels. Must be > 0.
	Top int
	// Window, when set, is a duration string recorded verbatim in the report
	// as the operator's churn-reasoning context. TSDB status is a point-in-time
	// head snapshot, so this frames the numbers; it is not a server-side filter.
	Window string
}

// Validate checks the options are usable before any request is issued.
func (o Options) Validate() error {
	if o.Top <= 0 {
		return fmt.Errorf("top must be positive, got %d", o.Top)
	}
	if o.Window != "" {
		if _, err := time.ParseDuration(o.Window); err != nil {
			return fmt.Errorf("window %q is not a valid duration: %w", o.Window, err)
		}
	}
	return nil
}

// NameValue is one {name, value} cardinality pair from a TSDB status array —
// a metric name to its series count, or a label name to its value cardinality
// or memory footprint.
type NameValue struct {
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

// HeadStats mirrors Prometheus TSDB headStats: the size of the in-memory head
// block that every query reads from.
type HeadStats struct {
	NumSeries     int64 `json:"numSeries"`
	NumLabelPairs int64 `json:"numLabelPairs"`
	ChunkCount    int64 `json:"chunkCount"`
	MinTime       int64 `json:"minTime"`
	MaxTime       int64 `json:"maxTime"`
}

// tsdbStatusResponse is the /api/v1/status/tsdb JSON envelope.
type tsdbStatusResponse struct {
	Status string `json:"status"`
	Data   struct {
		HeadStats                  HeadStats   `json:"headStats"`
		SeriesCountByMetricName    []NameValue `json:"seriesCountByMetricName"`
		LabelValueCountByLabelName []NameValue `json:"labelValueCountByLabelName"`
		MemoryInBytesByLabelName   []NameValue `json:"memoryInBytesByLabelName"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// InventoryVersion is the schema version stamped into every emitted Inventory.
// WriteJSON stamps it and the cutover gate refuses an inventory whose version it
// does not understand, so a schema-drifted or wrong-type artifact blocks rather
// than zero-filling to a silent PASS. Bump it on any breaking change to the JSON
// shape — the optional Loki and Tempo sections are exactly that kind of change,
// since DisallowUnknownFields decoding of an old artifact against a newer struct
// (or vice versa) is the class of drift this version exists to catch.
const InventoryVersion = 2

// Inventory is the ranked risk picture assembled from every source the
// operator pointed the probe at. The Prometheus fields (Head/TopMetrics*/
// TopLabels*) are always present — Inventory always probes a live Prometheus.
// Loki and Tempo are present only when the operator supplied the matching
// source flag; a nil section means "never asked," never "asked and found
// nothing." Every field is a source runtime fact; none predicts cerberus's
// own memory. It ranks candidates worth reviewing before cutover.
type Inventory struct {
	SchemaVersion int    `json:"schema_version"`
	Source        string `json:"source"`
	Window        string `json:"window,omitempty"`
	Top           int    `json:"top"`

	Head HeadStats `json:"head"`

	// TopMetricsBySeries ranks the metrics whose head series count is highest —
	// the OOM candidates a fan-out query would materialize.
	TopMetricsBySeries []NameValue `json:"topMetricsBySeriesCount"`
	// TopLabelsByValues ranks label names by distinct value cardinality — wide
	// labels drive group-by and join fan-out.
	TopLabelsByValues []NameValue `json:"topLabelsByValueCardinality"`
	// TopLabelsByMemory ranks label names by head memory footprint (bytes).
	TopLabelsByMemory []NameValue `json:"topLabelsByMemoryBytes"`

	// MetricNameTotal is the total distinct metric-name count from the optional
	// /label/__name__/values enrichment. -1 means the enrichment was not
	// obtained (see Notes for why).
	MetricNameTotal int `json:"metricNameTotal"`
	// MetadataMetricTotal is the count of metrics carrying metadata from the
	// optional /metadata enrichment. -1 means it was not obtained.
	MetadataMetricTotal int `json:"metadataMetricTotal"`

	// Notes surfaces every optional-enrichment failure and honesty caveat. An
	// enrichment that could not be fetched is recorded here — counted, never
	// silently dropped.
	Notes []string `json:"notes,omitempty"`

	// Loki is the optional per-selector stream cardinality/volume section,
	// present only when the operator supplied a Loki source to probe.
	Loki *LokiInventory `json:"loki,omitempty"`
	// Tempo is the optional out-of-scope section, present only when the
	// operator supplied a Tempo source.
	Tempo *TempoInventory `json:"tempo,omitempty"`
}

// Client probes a Prometheus-compatible HTTP API for TSDB cardinality.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient builds a probe client for baseURL with a bounded default client.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// Probe fetches and ranks the source Prometheus TSDB cardinality. The mandatory
// /api/v1/status/tsdb call drives the result: a transport failure, a non-200
// (a source that 404s the endpoint), or an unparseable body is a hard error the
// caller surfaces and exits non-zero on. The optional metric-name and metadata
// enrichments never abort the probe — their failures land in Notes.
func (c *Client) Probe(ctx context.Context, opts Options) (Inventory, error) {
	if err := opts.Validate(); err != nil {
		return Inventory{}, err
	}

	status, err := c.fetchTSDBStatus(ctx, opts.Top)
	if err != nil {
		return Inventory{}, err
	}

	inv := Inventory{
		SchemaVersion:       InventoryVersion,
		Source:              c.BaseURL,
		Window:              opts.Window,
		Top:                 opts.Top,
		Head:                status.Data.HeadStats,
		TopMetricsBySeries:  rankTopN(status.Data.SeriesCountByMetricName, opts.Top),
		TopLabelsByValues:   rankTopN(status.Data.LabelValueCountByLabelName, opts.Top),
		TopLabelsByMemory:   rankTopN(status.Data.MemoryInBytesByLabelName, opts.Top),
		MetricNameTotal:     -1,
		MetadataMetricTotal: -1,
	}

	// Optional enrichment: a total distinct metric-name count. A failure here is
	// recorded, not fatal — the ranked head cardinality already stands on its own.
	if total, err := c.fetchMetricNameTotal(ctx); err != nil {
		inv.Notes = append(inv.Notes, fmt.Sprintf("metric-name total unavailable (%s): %v", metricNamesPath, err))
	} else {
		inv.MetricNameTotal = total
	}

	// Optional enrichment: how many metrics publish metadata (help/type).
	if total, err := c.fetchMetadataTotal(ctx); err != nil {
		inv.Notes = append(inv.Notes, fmt.Sprintf("metadata total unavailable (%s): %v", metadataPath, err))
	} else {
		inv.MetadataMetricTotal = total
	}

	return inv, nil
}

// fetchTSDBStatus issues GET {base}/api/v1/status/tsdb?limit=top and decodes it.
// The limit param asks Prometheus for at least `top` entries per array so the
// local ranking has enough to rank; older servers that ignore it are fine
// because rankTopN re-sorts and truncates whatever it receives.
func (c *Client) fetchTSDBStatus(ctx context.Context, top int) (tsdbStatusResponse, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(top))
	body, err := c.getOK(ctx, statusTSDBPath+"?"+q.Encode())
	if err != nil {
		return tsdbStatusResponse{}, err
	}
	var raw tsdbStatusResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return tsdbStatusResponse{}, fmt.Errorf("decode %s response: %w", statusTSDBPath, err)
	}
	if raw.Status != promStatusSuccessful {
		return tsdbStatusResponse{}, fmt.Errorf("%s returned status %q: %s: %s",
			statusTSDBPath, raw.Status, raw.ErrorType, raw.Error)
	}
	return raw, nil
}

// metricNamesResponse is the /api/v1/label/__name__/values envelope.
type metricNamesResponse struct {
	Status string   `json:"status"`
	Data   []string `json:"data"`
}

// fetchMetricNameTotal returns the count of distinct metric names.
func (c *Client) fetchMetricNameTotal(ctx context.Context) (int, error) {
	body, err := c.getOK(ctx, metricNamesPath)
	if err != nil {
		return 0, err
	}
	var raw metricNamesResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}
	if raw.Status != promStatusSuccessful {
		return 0, fmt.Errorf("status %q", raw.Status)
	}
	return len(raw.Data), nil
}

// metadataResponse is the /api/v1/metadata envelope: a map of metric name to
// its metadata entries.
type metadataResponse struct {
	Status string                       `json:"status"`
	Data   map[string][]json.RawMessage `json:"data"`
}

// fetchMetadataTotal returns the count of metrics that publish metadata.
func (c *Client) fetchMetadataTotal(ctx context.Context) (int, error) {
	body, err := c.getOK(ctx, metadataPath)
	if err != nil {
		return 0, err
	}
	var raw metadataResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}
	if raw.Status != promStatusSuccessful {
		return 0, fmt.Errorf("status %q", raw.Status)
	}
	return len(raw.Data), nil
}

// getOK issues GET {base}{path} and returns the body only on a 200. A non-200
// (including a 404 of an endpoint an old Prometheus lacks) is an error carrying
// the status, so the caller can surface it and exit non-zero.
func (c *Client) getOK(ctx context.Context, path string) ([]byte, error) {
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

// readCappedBody reads r fully but no further than limit bytes, erroring rather
// than buffering an unbounded stream (it reads one byte past limit so an
// over-limit body is detected, never silently truncated into a mis-parse). This
// bounds the inventory-process memory against a misbehaving or wrong source —
// the same capped-read discipline the verify client applies to backend bodies.
func readCappedBody(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response body exceeds %d-byte cap", limit)
	}
	return body, nil
}

// rankTopN sorts a cardinality array by value descending (ties broken by name
// ascending for determinism) and truncates to top. It does not trust the
// server's ordering or truncation — it re-ranks whatever it received so the
// report is stable regardless of the source Prometheus version.
func rankTopN(in []NameValue, top int) []NameValue {
	out := make([]NameValue, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Value != out[j].Value {
			return out[i].Value > out[j].Value
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > top {
		out = out[:top]
	}
	return out
}
