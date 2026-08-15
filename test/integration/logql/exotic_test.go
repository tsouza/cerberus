//go:build chdb_agpl_oracle

package logql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	oraclelogql "github.com/tsouza/cerberus/test/property/oracle/logql"
	"github.com/tsouza/cerberus/test/spec/wire"
)

// TestExoticLogQL is the chDB-backed LogQL semantic integration suite.
//
// It seeds one rich fixed OTel logs fixture into an ephemeral chDB session,
// mounts the real loki.Handler, and runs every ExoticMatrix query through
// both cerberus's parse -> lower -> optimize -> emit -> execute HTTP pipeline
// and the independent from-scratch property oracle. CompareOutcomes checks
// the resulting stream-row multiset exactly.
//
// This is orthogonal to LogQL roundtrip: roundtrip pins self-derived SQL and
// expected_rows, while this suite computes its expected answer at runtime
// without GOLDEN_UPDATE and therefore catches semantic pipeline drift.
func TestExoticLogQL(t *testing.T) {
	ddl, model := RichSeed()
	cli := chclienttest.NewChDB(t)
	cli.Seed(t, ddl)

	handler := loki.New(cli, schema.DefaultOTelLogs(), nil)
	mux := http.NewServeMux()
	handler.Mount(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	dataset := property.Dataset{DDL: ddl, Logs: model}
	for _, tc := range ExoticMatrix {
		t.Run(tc.name, func(t *testing.T) {
			query := property.Query{String: tc.logql, EvalTs: seedAnchor.Add(time.Hour).Unix()}
			want := oraclelogql.Evaluate(dataset, query)
			if want.Err != nil {
				t.Fatalf("matrix contains unsupported oracle query %q: %v", query.String, want.Err)
			}
			got := runRange(t.Context(), server.URL, query)
			if diff := property.CompareOutcomes(want, got); diff != "" {
				t.Fatalf("exotic drift\n--- query ---\n%s\n--- diff (want=oracle got=cerberus) ---\n%s", query.String, diff)
			}
		})
	}
}

func runRange(ctx context.Context, baseURL string, query property.Query) property.Outcome {
	start := seedAnchor.Add(-time.Minute).Unix()
	end := seedAnchor.Add(time.Hour).Unix()
	url := fmt.Sprintf("%s/loki/api/v1/query_range?query=%s&start=%d&end=%d&step=60", baseURL, wire.EscapeQuery(query.String, ""), start, end)
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
	var parsed struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string        `json:"resultType"`
			Result     []loki.Stream `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return property.Outcome{Err: fmt.Errorf("decode body: %w; status=%d body=%s", err, resp.StatusCode, body)}
	}
	if parsed.Status != "success" {
		return property.Outcome{Err: fmt.Errorf("cerberus returned status=%q errorType=%q err=%q", parsed.Status, parsed.ErrorType, parsed.Error)}
	}
	if parsed.Data.ResultType != "streams" {
		return property.Outcome{Err: fmt.Errorf("cerberus returned resultType=%q, want streams", parsed.Data.ResultType)}
	}
	out := property.Outcome{Rows: make([]property.OutcomeRow, 0, len(modelRows(parsed.Data.Result)))}
	for _, stream := range parsed.Data.Result {
		for _, value := range stream.Values {
			timestamp, err := strconv.ParseInt(value.Timestamp, 10, 64)
			if err != nil {
				return property.Outcome{Err: fmt.Errorf("parse timestamp %q: %w", value.Timestamp, err)}
			}
			out.Rows = append(out.Rows, property.OutcomeRow{
				Labels:      cloneLabels(stream.Stream),
				TimestampMs: timestamp / int64(time.Millisecond),
				Line:        value.Line,
			})
		}
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if out.Rows[i].TimestampMs != out.Rows[j].TimestampMs {
			return out.Rows[i].TimestampMs < out.Rows[j].TimestampMs
		}
		return out.Rows[i].Line < out.Rows[j].Line
	})
	return out
}

func modelRows(streams []loki.Stream) []loki.StreamValue {
	var rows []loki.StreamValue
	for _, stream := range streams {
		rows = append(rows, stream.Values...)
	}
	return rows
}

func cloneLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
