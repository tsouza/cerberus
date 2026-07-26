package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/cucumber/godog"
)

// labelValuesFetchTimeout bounds one label-values HTTP round-trip.
const labelValuesFetchTimeout = 15 * time.Second

// labelValuesResponseLimit caps a label-values response body, generously
// above what the fixture's small, bounded label cardinality can ever
// produce.
const labelValuesResponseLimit = 1 << 20

// mappedLabels are the labels MIG-11's resource-attribute-mapping claim
// covers: service_name is the fixture's OTel `service.name` resource
// attribute resolved through cerberus's dedicated ServiceName column (the
// story's "dots→underscores" example — service.name -> service_name);
// http_status_code is a plain datapoint attribute, carried to prove the
// generic Attributes-map label surface and grouping agree too.
var mappedLabels = []string{"service_name", "http_status_code"}

// registerLabelMappingSteps binds the MIG-11 label/metric-name-mapping
// steps. The corpus selection and the shared verify assertions are already
// registered by registerVerifySteps; this file adds only the assertion
// MIG-11 needs beyond a zero-diverge verify report: that the label VALUE
// SETS a mapped resource attribute exposes agree between the two backends,
// which a verify run over grouped queries alone does not directly name.
func (w *World) registerLabelMappingSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the reference and cerberus agree on the mapped label value sets$`, w.thenLabelValuesAgree)
}

// thenLabelValuesAgree fetches `/api/v1/label/<name>/values` from both the
// live reference Prometheus and the live cerberus for every mapped label,
// and asserts the two returned value sets are identical. Set equality
// between two independently-queried live backends is the exact-parity
// predicate (docs/migration-testing.md section 6.2 kind 1) — no epsilon
// applies to a label value.
func (w *World) thenLabelValuesAgree() error {
	if !w.liveSet {
		return fmt.Errorf("the tier-1 stack has not been established; the scenario must establish it first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), labelValuesFetchTimeout)
	defer cancel()

	for _, label := range mappedLabels {
		refValues, err := fetchLabelValues(ctx, w.live.PromURL, label)
		if err != nil {
			return fmt.Errorf("reference prometheus: %w", err)
		}
		cerberusValues, err := fetchLabelValues(ctx, w.live.CerberusURL, label)
		if err != nil {
			return fmt.Errorf("cerberus: %w", err)
		}
		if !equalStringSets(refValues, cerberusValues) {
			return fmt.Errorf("label %q: reference returned %v, cerberus returned %v", label, refValues, cerberusValues)
		}
	}
	return nil
}

// labelValuesResponse is the Prometheus HTTP API's
// `/api/v1/label/<name>/values` wire shape, which cerberus's PromQL head
// also implements.
type labelValuesResponse struct {
	Status string   `json:"status"`
	Data   []string `json:"data"`
}

// fetchLabelValues GETs a label-values endpoint and returns its data array,
// sorted so the caller can compare two independently-fetched sets by value.
func fetchLabelValues(ctx context.Context, baseURL, label string) ([]string, error) {
	target := strings.TrimRight(baseURL, "/") + "/api/v1/label/" + url.PathEscape(label) + "/values"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", target, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, labelValuesResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("read %s body: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d: %s", target, resp.StatusCode, string(body))
	}
	var out labelValuesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode %s body %q: %w", target, string(body), err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("%s returned status %q, want %q", target, out.Status, "success")
	}
	values := append([]string{}, out.Data...)
	sort.Strings(values)
	return values, nil
}

// equalStringSets compares two already-sorted string slices for exact
// equality.
func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
