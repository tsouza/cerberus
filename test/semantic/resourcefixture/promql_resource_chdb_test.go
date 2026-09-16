//go:build chdb

// PromQL leg of SIGNAL-RESOURCE-001 (cerberus issue #3457): seeds Fixture
// into a custom-named otel_metrics_sum table and proves the bare selector
// both selects on and exposes the simple key (team) and the dotted key
// (k8s.namespace.name), the latter normalized to its underscored wire label
// per an EXPLICIT PromResourceLabels allow-list, and that the returned
// instant-query timestamp carries PromQL's own native millisecond
// resolution rather than the row's full seeded nanosecond value.
package resourcefixture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

const promqlFixtureMetricName = "checkout_requests_total"

// promqlFixtureDDL declares promqlFixtureMetricsTable with exactly the
// columns the PromQL sum-table read path projects — ResourceAttributes
// carries both fixture keys; there is no dedicated top-level column for
// either, so the generic resource-attribute arm is what this test
// exercises (see fixture.go's TeamKey doc for why Team avoids the
// dedicated-column special case service.name would trigger).
var promqlFixtureDDL = fmt.Sprintf(`CREATE TABLE %s (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String),
    ServiceName String,
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`, promqlFixtureMetricsTable)

func promqlFixtureSeed() string {
	return fmt.Sprintf(
		`INSERT INTO %s (MetricName, Attributes, ResourceAttributes, TimeUnix, Value) VALUES ('%s', map(), map('%s', '%s', '%s', '%s'), toDateTime64('%s', 9), 42);`,
		promqlFixtureMetricsTable, promqlFixtureMetricName,
		TeamKey, Fixture.Team, NamespaceKey, Fixture.Namespace,
		Fixture.Timestamp.UTC().Format(TSFormat),
	)
}

type promqlFixtureVectorSample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]json.Number    `json:"value"`
}

// TestSignalResource_PromQL_ChDB is SIGNAL-RESOURCE-001's PromQL evidence.
func TestSignalResource_PromQL_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, promqlFixtureDDL)
	c.Seed(t, promqlFixtureSeed())

	s := schema.DefaultOTelMetrics()
	s.SumTable = promqlFixtureMetricsTable
	s.PromResourceLabels = promqlFixtureResourceLabels

	h := prom.New(c, s, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// The query `time` itself carries the fixture's full nanosecond
	// instant (not a whole-second value) so the assertion below proves
	// PromQL's OWN wire format collapses it to millisecond resolution —
	// Prometheus's instant-query response reports the EVALUATION
	// timestamp, not the underlying sample's raw stored timestamp, so
	// feeding a sub-millisecond `time` is what actually exercises that
	// native precision contract.
	evalAt := Fixture.Timestamp.Add(time.Minute)
	evalAtSeconds := float64(evalAt.UnixNano()) / 1e9
	query := fmt.Sprintf(`%s{team=%q,%s=%q}`, promqlFixtureMetricName, Fixture.Team, NamespaceWireLabel, Fixture.Namespace)
	u := fmt.Sprintf("%s/api/v1/query?query=%s&time=%s", srv.URL, url.QueryEscape(query), strconv.FormatFloat(evalAtSeconds, 'f', 9, 64))

	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	var parsed struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string                      `json:"resultType"`
			Result     []promqlFixtureVectorSample `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != http.StatusOK || parsed.Status != "success" {
		t.Fatalf("query %q: status=%d api_status=%q err=%s", query, resp.StatusCode, parsed.Status, parsed.Error)
	}
	if len(parsed.Data.Result) != 1 {
		t.Fatalf("selector matched %d series, want 1: %+v", len(parsed.Data.Result), parsed.Data.Result)
	}
	sample := parsed.Data.Result[0]

	if got := sample.Metric["team"]; got != Fixture.Team {
		t.Errorf(`metric["team"] = %q, want %q (simple shared key, no normalization)`, got, Fixture.Team)
	}
	if got := sample.Metric[NamespaceWireLabel]; got != Fixture.Namespace {
		t.Errorf("metric[%q] = %q, want %q (dotted key, PromQL-normalized wire label)", NamespaceWireLabel, got, Fixture.Namespace)
	}
	if _, present := sample.Metric[NamespaceKey]; present {
		t.Errorf("metric carries the RAW dotted key %q on the wire; PromQL must expose only the sanitized form %q", NamespaceKey, NamespaceWireLabel)
	}

	// PromQL's own native timestamp precision: Prometheus's instant-query
	// response reports the EVALUATION timestamp (evalAt), rounded to
	// milliseconds — never the sub-millisecond `time` this request sent,
	// and never a coarser (second-level) rounding either.
	wantSeconds := float64(evalAt.UnixMilli()) / 1000
	gotSeconds, err := sample.Value[0].Float64()
	if err != nil {
		t.Fatalf("value[0] %q not a number: %v", sample.Value[0], err)
	}
	if diff := gotSeconds - wantSeconds; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("value[0] = %v, want %v (millisecond-rounded fixture instant, PromQL's native wire precision)", gotSeconds, wantSeconds)
	}
}
