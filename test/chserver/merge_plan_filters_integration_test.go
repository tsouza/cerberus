//go:build integration

package chserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// mergePlanFiltersImage is the supported floor, a release that has
// query_plan_merge_filters off by default.
const mergePlanFiltersImage = "clickhouse/clickhouse-server:24.8.14.39-alpine"

// mergePlanFiltersQuery filters on a label its logfmt stage parses, after an
// unwrap, so the filter sits on a value computed above the SELECT that reads
// the table.
const mergePlanFiltersQuery = `count(sum_over_time({service_name="tempo"} | logfmt | unwrap spans | status > 100 [5m]))`

// The seeded stream: mergePlanFiltersDroppedRows lines without a status
// spread over the window's first mergePlanFiltersDroppedSpan, then
// mergePlanFiltersKeptRows lines with status=500 in its last minute. Only the
// detected level reads the severity columns.
const (
	mergePlanFiltersDroppedRows = 98_304 // twelve 8192-row granules
	mergePlanFiltersKeptRows    = 1_000
	mergePlanFiltersDroppedSpan = 90 * time.Minute
	mergePlanFiltersStep        = time.Minute
)

// severityNumberBytes is the width of a SeverityNumber value (UInt8): the
// least a line whose severity is not read leaves unread.
const severityNumberBytes = 1

var (
	mergePlanFiltersStart = time.Date(2026, time.May, 11, 6, 0, 0, 0, time.UTC)
	mergePlanFiltersEnd   = mergePlanFiltersStart.Add(2 * time.Hour)
)

// mergePlanFiltersProfile and mergePlanFiltersUser hold
// query_plan_merge_filters at the server's default under a CONST
// constraint, so a query that sets it is refused.
const (
	mergePlanFiltersProfile  = "merge_filters_fixed"
	mergePlanFiltersUser     = "merge_filters_fixed"
	mergePlanFiltersPassword = "merge_filters_fixed"
)

// TestRangeAggregationFilterAboveTheScanSparesTheSeverity runs
// mergePlanFiltersQuery on the floor server twice, with the rules its build
// resolves to and with query_plan_merge_filters left out of them. The status
// filter keeps only the last minute's lines; with the setting it reaches the
// table's PREWHERE and leaves the severity of every other line unread, so the
// query reads at least severityNumberBytes less per dropped line, and answers
// the same.
//
// A user whose settings profile refuses the setting gets the query answered
// too, with the statement, the bytes and the answer it has without the
// setting.
func TestRangeAggregationFilterAboveTheScanSparesTheSeverity(t *testing.T) {
	ctx := context.Background()
	s := startServer(ctx, t, mergePlanFiltersImage)
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("apply the logs schema: %v", err)
	}
	seedMergePlanFilters(ctx, t, s)

	set, _, err := chopt.Resolve(chopt.Config{Optimizations: chopt.SelectionAuto, Mode: chopt.Permissive}, s.version)
	if err != nil {
		t.Fatalf("resolve the optimizations of %s: %v", s.version, err)
	}
	if !set.Has(chopt.FeatureMergePlanFilters) {
		t.Fatalf("auto on %s leaves out %s; the test needs a build it resolves on", s.version, chopt.FeatureMergePlanFilters)
	}
	rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
	without := rules
	without.MergePlanFilters = false
	admin := s.client(t, adminUser, adminPassword, chclient.Config{})

	merged := queryMergePlanFilters(ctx, t, s, mergePlanFiltersMux(t, s, admin, rules))
	unmerged := queryMergePlanFilters(ctx, t, s, mergePlanFiltersMux(t, s, admin, without))
	t.Logf("%s: merged %d bytes, unmerged %d", s.version, merged.readBytes, unmerged.readBytes)

	if merged.answer != unmerged.answer {
		t.Errorf("with the setting the query answers %s; without it, %s", merged.answer, unmerged.answer)
	}
	if merged.samples == 0 {
		t.Errorf("the query answers %s; want a sample for every step that sees the status=500 lines", merged.answer)
	}
	if spared := int64(unmerged.readBytes) - int64(merged.readBytes); spared < mergePlanFiltersDroppedRows*severityNumberBytes {
		t.Errorf("the query read %d bytes, %d fewer than without the setting; want at least %d fewer, "+
			"the severity number of every line the status filter drops", merged.readBytes, spared, mergePlanFiltersDroppedRows*severityNumberBytes)
	}

	s.exec(ctx, t, "CREATE SETTINGS PROFILE "+mergePlanFiltersProfile+" SETTINGS "+chopt.SettingQueryPlanMergeFilters+" = 0 CONST")
	s.exec(ctx, t, "CREATE USER "+mergePlanFiltersUser+" IDENTIFIED BY '"+mergePlanFiltersPassword+"' SETTINGS PROFILE '"+mergePlanFiltersProfile+"'")
	s.exec(ctx, t, "GRANT SELECT ON "+serverDB+".* TO "+mergePlanFiltersUser)
	fixed := s.client(t, mergePlanFiltersUser, mergePlanFiltersPassword, chclient.Config{})
	rows, err := fixed.Conn().Query(clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{chopt.SettingQueryPlanMergeFilters: 1})), "SELECT 1")
	if err == nil {
		_ = rows.Close()
	}
	if code := exceptionCode(err); code != codeSettingConstraintViolation {
		t.Fatalf("%s=1 as %s: err = %v, code %d; want the profile's SETTING_CONSTRAINT_VIOLATION", chopt.SettingQueryPlanMergeFilters, mergePlanFiltersUser, err, code)
	}
	refused := queryMergePlanFilters(ctx, t, s, mergePlanFiltersMux(t, s, fixed, rules))
	if refused.answer != unmerged.answer || refused.statement != unmerged.statement || refused.readBytes != unmerged.readBytes {
		t.Errorf("under a profile that refuses the setting the query answers %s, reading %d bytes with\n%s\nwant %s, reading %d with\n%s",
			refused.answer, refused.readBytes, refused.statement, unmerged.answer, unmerged.readBytes, unmerged.statement)
	}
}

// mergePlanFiltersScopeQueries are range aggregations whose per-row stage
// leaves above the scan's scope only filters on values the scope computes —
// a detected_level matcher the scope answers, a parsed label's filter on the
// parser's labels — or no filter at all. On 24.8 the setting spares them
// about a thousandth of their bytes at most, and can triple their peak
// memory. The test pins that they carry no setting: the same statement with
// and without it differs only in where the server runs the filters, so the
// setting alone is what raised their memory.
var mergePlanFiltersScopeQueries = []string{
	`sum by (detected_level) (count_over_time({service_name="tempo"} | detected_level=~"warn|error" [5m]))`,
	`sum by (detected_level) (count_over_time({service_name="tempo"} | detected_level!~"info" [5m]))`,
	`sum(count_over_time({service_name="tempo"} | detected_level=~"error|fatal" |= "status=500" [5m]))`,
	`sum(count_over_time({service_name="tempo"} | detected_level="error" |= "status=500" [5m]))`,
	`sum(count_over_time({service_name="tempo"} | detected_level=~"error|fatal" | service_name!="loki" |= "status=500" [5m]))`,
	`sum(rate({service_name="tempo"} | detected_level=~"error|fatal" |= "status=500" [1m]))`,
	`sum by (detected_level) (count_over_time({service_name="tempo"}[5m]))`,
	`sum(count_over_time({service_name="tempo"} | drop detected_level="info" [5m]))`,
	`sum(count_over_time({service_name="tempo"} | logfmt | spans >= 3 [5m]))`,
}

// TestRangeAggregationScopeFiltersRunWithoutMergeFilters pins which plans the
// floor server gets query_plan_merge_filters on: mergePlanFiltersQuery,
// whose status filter reads the labels its unwrap computes above the scan's
// scope, carries it, and each of mergePlanFiltersScopeQueries carries none
// and answers with samples from the seeded stream.
func TestRangeAggregationScopeFiltersRunWithoutMergeFilters(t *testing.T) {
	ctx := context.Background()
	s := startServer(ctx, t, mergePlanFiltersImage)
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("apply the logs schema: %v", err)
	}
	seedMergePlanFilters(ctx, t, s)
	set, _, err := chopt.Resolve(chopt.Config{Optimizations: chopt.SelectionAuto, Mode: chopt.Permissive}, s.version)
	if err != nil {
		t.Fatalf("resolve the optimizations of %s: %v", s.version, err)
	}
	rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
	if !rules.MergePlanFilters {
		t.Fatalf("auto on %s leaves out %s; the test needs a build it resolves on", s.version, chopt.FeatureMergePlanFilters)
	}
	admin := s.client(t, adminUser, adminPassword, chclient.Config{})
	mux := mergePlanFiltersMux(t, s, admin, rules)

	if run := queryMergePlanFilters(ctx, t, s, mux); run.mergeFilters != "1" {
		t.Errorf("%s carries %s=%q; want 1:\n%s", mergePlanFiltersQuery, chopt.SettingQueryPlanMergeFilters, run.mergeFilters, run.statement)
	}
	for _, query := range mergePlanFiltersScopeQueries {
		t.Run(query, func(t *testing.T) {
			run := queryMergePlanFiltersShape(ctx, t, s, mux, query)
			if run.mergeFilters != "" {
				t.Errorf("the statement carries %s=%q; want none:\n%s", chopt.SettingQueryPlanMergeFilters, run.mergeFilters, run.statement)
			}
			if run.samples == 0 {
				t.Errorf("the query answers %s; want samples from the seeded stream", run.answer)
			}
		})
	}
}

// codeSettingConstraintViolation is ClickHouse's SETTING_CONSTRAINT_VIOLATION
// error code.
const codeSettingConstraintViolation = 452

// mergePlanFiltersMux mounts the Loki API over client, with the strategies
// s's build resolves to and rules.
func mergePlanFiltersMux(t *testing.T, s *server, client *chclient.Client, rules engine.SettingsRules) *http.ServeMux {
	t.Helper()
	cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
	cell.SetPrewhereInWhereOrder(chopt.PrewhereKeepsWhereOrder(s.version))
	h := loki.New(client, schema.DefaultOTelLogs(), nil)
	h.Strategies = cell
	h.Engine.SetSettings(rules)
	mux := http.NewServeMux()
	h.Mount(mux)
	return mux
}

// mergePlanFiltersRun is one run of a query over the seeded stream: its
// answer, the samples in it, and the statement the server finished with what
// it read and the value of query_plan_merge_filters it carried ("" when it
// carried none).
type mergePlanFiltersRun struct {
	answer       string
	samples      int
	statement    string
	readBytes    uint64
	mergeFilters string
}

// queryMergePlanFilters sends mergePlanFiltersQuery as a query_range over the
// seeded window through mux.
func queryMergePlanFilters(ctx context.Context, t *testing.T, s *server, mux *http.ServeMux) mergePlanFiltersRun {
	t.Helper()
	return queryMergePlanFiltersShape(ctx, t, s, mux, mergePlanFiltersQuery)
}

// queryMergePlanFiltersShape sends query as a query_range over the seeded
// window through mux.
func queryMergePlanFiltersShape(ctx context.Context, t *testing.T, s *server, mux *http.ServeMux, query string) mergePlanFiltersRun {
	t.Helper()
	params := url.Values{
		"query": {query},
		"start": {strconv.FormatInt(mergePlanFiltersStart.UnixNano(), 10)},
		"end":   {strconv.FormatInt(mergePlanFiltersEnd.UnixNano(), 10)},
		"step":  {strconv.FormatFloat(mergePlanFiltersStep.Seconds(), 'f', -1, 64)},
	}
	qid := "merge-plan-filters-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	var run mergePlanFiltersRun
	req := httptest.NewRequest(http.MethodGet, "/loki/api/v1/query_range?"+params.Encode(), nil).
		WithContext(chclient.WithQueryID(ctx, qid))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Result []struct {
				Values [][2]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	result, err := json.Marshal(body.Data.Result)
	if err != nil {
		t.Fatalf("encode the result: %v", err)
	}
	run.answer = string(result)
	for _, series := range body.Data.Result {
		run.samples += len(series.Values)
	}
	s.flushLogs(ctx, t)
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT read_bytes, query, Settings[?] FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?",
		chopt.SettingQueryPlanMergeFilters, qid).
		Scan(&run.readBytes, &run.statement, &run.mergeFilters); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	return run
}

// seedMergePlanFilters seeds the stream mergePlanFiltersQuery selects.
func seedMergePlanFilters(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	const columns = `INSERT INTO otel_logs (Timestamp, ServiceName, SeverityText, SeverityNumber, Body, ResourceAttributes) `
	const resource = `map('service.name', 'tempo', 'service_name', 'tempo')`
	droppedStep := mergePlanFiltersDroppedSpan / mergePlanFiltersDroppedRows
	s.exec(ctx, t, columns+
		`SELECT toDateTime64(?, 9) + toIntervalNanosecond(number * ?), 'tempo', 'INFO', 9, `+
		`concat('msg=handled spans=', toString(number % 7)), `+resource+` FROM numbers(?)`,
		mergePlanFiltersStart, droppedStep.Nanoseconds(), mergePlanFiltersDroppedRows)
	keptAt := mergePlanFiltersEnd.Add(-time.Minute)
	s.exec(ctx, t, columns+
		`SELECT toDateTime64(?, 9) + toIntervalMillisecond(number), 'tempo', 'ERROR', 17, `+
		`concat('msg=failed status=500 spans=', toString(number % 7)), `+resource+` FROM numbers(?)`,
		keptAt, mergePlanFiltersKeptRows)
	s.exec(ctx, t, "OPTIMIZE TABLE otel_logs FINAL")
}
