//go:build integration

package chserver

import (
	"context"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// wholeMapFilterQueries filter on a stored label, which reads LogAttributes
// whole and on the line, in either order, with and without a detected_level
// filter ahead of them. The label filter is a negation, which no index on the
// attribute values serves, and the stream selector reads one key of
// ResourceAttributes.
var wholeMapFilterQueries = []string{
	`sum(count_over_time({job="wide"} | tenant!="tenant-7" |= "status=500" [1h]))`,
	`sum(count_over_time({job="wide"} | detected_level="error" | tenant!="tenant-7" |= "status=500" [1h]))`,
	`sum(count_over_time({job="wide"} |= "status=500" | tenant!="tenant-7" [1h]))`,
}

// The seeded stream: wholeMapFilterDroppedRows lines of tenant-7 over the
// window's first wholeMapFilterDroppedSpan, then wholeMapFilterKeptRows
// lines of tenant-1 in its last minute. Every line is an error line that
// says status=500, so neither the level filter nor the line filter drops
// any, and carries wholeMapFilterResourceKeys resource attributes, so the
// ResourceAttributes column dwarfs every other.
const (
	wholeMapFilterDroppedRows  = 98_304 // twelve 8192-row granules
	wholeMapFilterKeptRows     = 1_000
	wholeMapFilterDroppedSpan  = 20 * time.Hour
	wholeMapFilterResourceKeys = 20
)

// TestRangeAggregationWholeMapFilterSparesTheResource runs
// wholeMapFilterQueries on a server that orders a multi-step PREWHERE
// itself, with the strategies its build resolves to, and requires each to
// count the tenant-1 lines while reading fewer bytes than the
// ResourceAttributes column holds. The tenant filter keeps only the last
// minute's lines. Run where the table is read, with the line filter, it is
// a PREWHERE step, and the scan reads ResourceAttributes, which the stage's
// series key reads whole, only for the granules it keeps. Left above the
// scan, the line filter is the scan's only step past the stream selector,
// and the scan reads ResourceAttributes for every line.
func TestRangeAggregationWholeMapFilterSparesTheResource(t *testing.T) {
	ctx := context.Background()
	s := startServer(ctx, t, logFilterReorderImage)
	if chopt.PrewhereKeepsWhereOrder(s.version) {
		t.Fatalf("%s keeps WHERE order in PREWHERE; the test needs a build that reorders it", s.version)
	}
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("apply the logs schema: %v", err)
	}
	seedWholeMapFilter(ctx, t, s)
	var resourceBytes uint64
	if err := s.admin.Conn().QueryRow(ctx, "SELECT data_uncompressed_bytes FROM system.columns "+
		"WHERE database = currentDatabase() AND table = 'otel_logs' AND name = 'ResourceAttributes'").Scan(&resourceBytes); err != nil {
		t.Fatalf("measure the ResourceAttributes column: %v", err)
	}
	mux := logFilterOrderMux(t, s, true)
	for _, query := range wholeMapFilterQueries {
		t.Run(query, func(t *testing.T) {
			var body struct {
				Data struct {
					Result []struct {
						Values [][2]any `json:"values"`
					} `json:"result"`
				} `json:"data"`
			}
			s.dropConditionCache(ctx, t)
			readBytes := queryLogFilterOrder(ctx, t, s, mux, url.Values{
				"query": {query},
				"step":  {strconv.FormatFloat(logFilterOrderStep.Seconds(), 'f', -1, 64)},
			}, &body)
			total := 0
			for _, series := range body.Data.Result {
				for _, v := range series.Values {
					text, ok := v[1].(string)
					if !ok {
						t.Fatalf("sample value %v is not a string", v[1])
					}
					n, err := strconv.Atoi(text)
					if err != nil {
						t.Fatalf("sample value %v: %v", v[1], err)
					}
					total += n
				}
			}
			if total != wholeMapFilterKeptRows {
				t.Errorf("the query counted %d lines; want the %d tenant-1 lines", total, wholeMapFilterKeptRows)
			}
			t.Logf("%s: the query read %d bytes; ResourceAttributes holds %d", s.version, readBytes, resourceBytes)
			if readBytes >= resourceBytes {
				t.Errorf("the query read %d bytes, no fewer than the %d ResourceAttributes holds: "+
					"it read the resource attributes of lines the tenant filter drops", readBytes, resourceBytes)
			}
		})
	}
}

// seedWholeMapFilter seeds the stream wholeMapFilterQueries select.
func seedWholeMapFilter(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	const insert = `INSERT INTO otel_logs (Timestamp, ServiceName, SeverityText, SeverityNumber, Body, ResourceAttributes, LogAttributes) ` +
		`SELECT toDateTime64(?, 9) + toIntervalNanosecond(number * ?), 'tempo', 'ERROR', 17, ` +
		`concat('request failed status=500 ', toString(number)), ` +
		`mapConcat(map('job', 'wide'), ` +
		`mapFromArrays(arrayMap(i -> concat('resource.attribute.', toString(i)), range(?)), ` +
		`arrayMap(i -> concat('resource-value-', toString(i), '-of-a-long-lived-deployment'), range(?)))), ` +
		`map('tenant', ?) FROM numbers(?)`
	droppedStep := wholeMapFilterDroppedSpan / wholeMapFilterDroppedRows
	s.exec(ctx, t, insert, logFilterOrderStart, droppedStep.Nanoseconds(), wholeMapFilterResourceKeys, wholeMapFilterResourceKeys, "tenant-7", wholeMapFilterDroppedRows)
	keptStep := time.Minute / wholeMapFilterKeptRows
	s.exec(ctx, t, insert, logFilterOrderEnd.Add(-time.Minute), keptStep.Nanoseconds(), wholeMapFilterResourceKeys, wholeMapFilterResourceKeys, "tenant-1", wholeMapFilterKeptRows)
	s.exec(ctx, t, "OPTIMIZE TABLE otel_logs FINAL")
}
