//go:build chdb

package promql_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	"golang.org/x/tools/txtar"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

func TestSubqueryTimestampPreservesSelectedSample_ChDB(t *testing.T) {
	const fixturePermissions = 0o600
	const query = "max_over_time(timestamp(up)[5m:1m])"
	const window = 5 * time.Minute
	const sampleBeforeStart = 37 * time.Second
	const nextSampleOffset = 2*time.Minute + 17*time.Second
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(window)
	firstSample, nextSample := start.Add(-sampleBeforeStart), start.Add(nextSampleOffset)
	seedFixture, err := spec.Load("../../test/spec/promql/timestamp_of_metric_subquery_inner.txtar")
	if err != nil {
		t.Fatal(err)
	}
	seed, ok := seedFixture.Section("seed")
	if !ok {
		t.Fatal("timestamp regression fixture must provide its sample seed")
	}
	var rows [][]any
	for at := start; !at.After(end); at = at.Add(time.Minute) {
		sample := firstSample
		if !at.Before(nextSample) {
			sample = nextSample
		}
		rows = append(rows, []any{map[string]string{"job": "api"}, at.Format(time.RFC3339), at.Format(time.RFC3339), sample.Unix()})
	}
	expected, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	archive := &txtar.Archive{Files: []txtar.File{
		{Name: "query.promql", Data: []byte(query)},
		{Name: "seed", Data: []byte(seed)},
		{Name: "expected_rows", Data: expected},
		{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: /api/v1/query_range\nscope: full\n")},
	}}
	path := filepath.Join(t.TempDir(), "timestamp.txtar")
	if err := os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
		t.Fatal(err)
	}
	fixture, err := spec.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	plan = spec.AssertScanTimeBoundAccepts(t, plan)
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	actual := spec.RunRoundTripSQL(t, fixture, sql, args)
	spec.RunParity(t, fixture, spec.ParityEval{Start: start, End: end, Step: time.Minute}, actual)
}
