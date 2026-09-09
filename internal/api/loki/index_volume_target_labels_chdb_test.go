//go:build chdb

// Behavioural coverage for /index/volume's `targetLabels` projection
// against real ClickHouse semantics (chDB). The bug this pins is a
// WRONG ANSWER, not a wrong shape: the endpoint scoped the request
// through one label-resolution rule and projected the result through a
// different one, so every row matched and none could be described.
//
// The seed writes the storage shape the OTel-CH exporter actually
// produces — `service.name` hoisted into the dedicated `ServiceName`
// column, absent from the `ResourceAttributes` map. A stub querier can
// never exhibit this: it takes CH's evaluation of the projection to
// decide what the label map for each group is.

package loki_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// hoistedServiceSeed mirrors the OTel-CH default logs table for the
// columns /index/volume reads, including the top-level ServiceName
// column the exporter hoists `service.name` into. The two rows share a
// `job` label (so one selector matches both) and differ only in the
// hoisted service, which is what the request asks to be grouped by.
const hoistedServiceSeed = `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    ServiceName String,
    ResourceAttributes Map(String, String)
) ENGINE = Memory;
INSERT INTO otel_logs (Timestamp, Body, ServiceName, ResourceAttributes) VALUES
    (toDateTime64('2026-05-14 12:00:00.000', 9), '0123456789', 'checkout', map('job','api')),
    (toDateTime64('2026-05-14 12:00:01.000', 9), '01234',      'payments', map('job','api'));`

// hoistedServiceWindow frames both seeded rows.
func hoistedServiceWindow() (int64, int64) {
	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	return base.Add(-time.Minute).Unix(), base.Add(time.Minute).Unix()
}

// TestIndexVolume_ChDB_TargetLabelsResolvesHoistedColumn is the
// wrong-answer regression. `targetLabels=service_name` used to project
// `mapFilter((k, v) -> k IN ('service_name'), ResourceAttributes)` — the
// LITERAL map key — while the selector scoping the same request resolved
// `service_name` through the dedicated `ServiceName` column. On the
// storage shape the OTel-CH exporter produces, the map holds no
// `service_name` key at all, so the projection evaluated to the EMPTY
// map for every row: two services' byte volumes merged into one
// unlabelled `metric: {}` sample and the caller lost both the split and
// the names.
//
// Reference Loki reads the value off the stream's own labels
// (pkg/ingester/instance.go:889-903) — the same labels the matchers were
// evaluated against — so the projection must resolve exactly as the
// selector does.
func TestIndexVolume_ChDB_TargetLabelsResolvesHoistedColumn(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, hoistedServiceSeed)
	h := loki.New(c, schema.DefaultOTelLogs(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	start, end := hoistedServiceWindow()
	var parsed struct {
		Data loki.QueryData `json:"data"`
	}
	getJSON(t, fmt.Sprintf(
		`%s/loki/api/v1/index/volume?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d`+
			`&targetLabels=service_name&aggregateBy=labels`,
		srv.URL, start, end,
	), &parsed)

	raw, err := json.Marshal(parsed.Data.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	var samples []loki.VectorSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatalf("decode vector: %v", err)
	}

	got := map[string]string{}
	for _, s := range samples {
		if len(s.Metric) == 0 {
			t.Fatalf("a projected row carries NO labels — the whole tenant's volume "+
				"collapsed into one unlabelled sample: %+v", samples)
		}
		value, _ := s.Value[1].(string)
		got[s.Metric["service_name"]] = value
	}
	// Body lengths are 10 and 5; each service contributes exactly one row.
	want := map[string]string{"checkout": "10", "payments": "5"}
	if len(got) != len(want) {
		t.Fatalf("expected one row per hoisted service %v; got %v (samples %+v)", want, got, samples)
	}
	for svc, bytes := range want {
		if got[svc] != bytes {
			t.Errorf("service %q: byte volume %q, want %q (all %v)", svc, got[svc], bytes, got)
		}
	}
}
