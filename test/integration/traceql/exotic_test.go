//go:build chdb

package traceql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	oracletraceql "github.com/tsouza/cerberus/test/property/oracle/traceql"
	"github.com/tsouza/cerberus/test/spec/wire"
)

const searchWindowMargin = time.Hour

// TestExoticTraceQL is the chDB-backed TraceQL semantic integration suite.
//
// It seeds one rich fixed OTel traces fixture into an ephemeral chDB session,
// mounts the real tempo.Handler, and runs every ExoticMatrix query through
// both cerberus's parse -> lower -> optimize -> emit -> execute HTTP pipeline
// and the independent from-scratch property oracle. CompareOutcomes checks
// the resulting inspected-span multiset exactly.
//
// This is orthogonal to TraceQL roundtrip: roundtrip pins self-derived SQL and
// expected_rows, while this suite computes its expected answer at runtime
// without GOLDEN_UPDATE and therefore catches semantic pipeline drift.
func TestExoticTraceQL(t *testing.T) {
	ddl, model := RichSeed()
	cli := chclienttest.NewChDB(t)
	cli.Seed(t, ddl)

	handler := tempo.New(cli, schema.DefaultOTelTraces(), "v1.0.0-integration", nil)
	mux := http.NewServeMux()
	handler.Mount(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	dataset := property.Dataset{DDL: ddl, Metrics: model}
	for _, tc := range ExoticMatrix {
		t.Run(tc.name, func(t *testing.T) {
			query := property.Query{String: tc.traceql, EvalTs: seedAnchor.Add(searchWindowMargin).Unix()}
			want := oracletraceql.Evaluate(dataset, query)
			if want.Err != nil {
				t.Fatalf("matrix contains unsupported oracle query %q: %v", query.String, want.Err)
			}
			got := runSearch(t.Context(), server.URL, query)
			if diff := property.CompareOutcomes(want, got); diff != "" {
				t.Fatalf("exotic drift\n--- query ---\n%s\n--- diff (want=oracle got=cerberus) ---\n%s", query.String, diff)
			}
		})
	}
}

func runSearch(ctx context.Context, baseURL string, query property.Query) property.Outcome {
	start := seedAnchor.Add(-searchWindowMargin).Unix()
	end := seedAnchor.Add(searchWindowMargin).Unix()
	url := fmt.Sprintf("%s/api/search?q=%s&start=%d&end=%d", baseURL, wire.EscapeQuery(query.String, ""), start, end)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("build request: %w", err)}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("query roundtrip: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("read body: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		return property.Outcome{Err: fmt.Errorf("cerberus returned status=%d body=%s", resp.StatusCode, body)}
	}
	var parsed tempo.SearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return property.Outcome{Err: fmt.Errorf("decode body: %w; status=%d body=%s", err, resp.StatusCode, body)}
	}
	inspectedSpans, err := strconv.Atoi(resp.Header.Get(tempo.HeaderInspectedSpans))
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("%s header %q: %w", tempo.HeaderInspectedSpans, resp.Header.Get(tempo.HeaderInspectedSpans), err)}
	}
	rows := make([]property.OutcomeRow, 0, inspectedSpans)
	for range inspectedSpans {
		rows = append(rows, property.OutcomeRow{Labels: map[string]string{}})
	}
	return property.Outcome{Rows: rows}
}
