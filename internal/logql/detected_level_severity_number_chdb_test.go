//go:build chdb

package logql

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chsql"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestDetectedLevel_SeverityNumberResolvesWhenNoTextIsPresent drives
// `{job="api"} | detected_level="<level>"` over rows that carry an OTLP
// SeverityNumber and NO textual level anywhere — no detected_level or
// level structured-metadata key, an empty SeverityText — and asserts each
// row resolves to the level reference Loki derives from the number
// (pkg/distributor/field_detection.go's detectLogLevelFromLogEntry: the
// OTLP ranges 1-4 trace, 5-8 debug, 9-12 info, 13-16 warn, 17-20 error,
// 21-24 fatal; 0 and anything past 24 unknown). Before the number arm,
// every such row was `unknown`.
//
// It also pins the precedence: a row carrying BOTH a SeverityText and a
// SeverityNumber that disagree resolves from the text, as upstream's
// allowed level fields (severity_text among them) come before the number.
func TestDetectedLevel_SeverityNumberResolvesWhenNoTextIsPresent(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec("CREATE OR REPLACE TABLE otel_logs (" +
		"Timestamp DateTime64(9), Body String, " +
		"SeverityText LowCardinality(String) DEFAULT '', SeverityNumber Int32 DEFAULT 0, " +
		"ServiceName String DEFAULT '', " +
		"ResourceAttributes Map(String,String), LogAttributes Map(String,String)" +
		") ENGINE = Memory"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// One row per OTLP severity number at both ends of each range, plus
	// the unspecified 0 and an out-of-range value; Body names the number
	// so a wrong match reads as a number, not a row index.
	rows := []struct {
		number int
		text   string
		want   string
	}{
		{0, "", "unknown"},
		{1, "", "trace"},
		{4, "", "trace"},
		{5, "", "debug"},
		{8, "", "debug"},
		{9, "", "info"},
		{12, "", "info"},
		{13, "", "warn"},
		{16, "", "warn"},
		{17, "", "error"},
		{20, "", "error"},
		{21, "", "fatal"},
		{24, "", "fatal"},
		{25, "", "unknown"},
		// Text present and disagreeing with the number: text wins.
		{17, "warning", "warn"},
	}
	for _, r := range rows {
		if _, err := db.Exec(fmt.Sprintf(
			"INSERT INTO otel_logs (Timestamp, Body, SeverityText, SeverityNumber, ResourceAttributes) "+
				"VALUES (now64(9), 'n=%d;t=%s', '%s', %d, map('job','api'))", r.number, r.text, r.text, r.number,
		)); err != nil {
			t.Fatalf("seed %+v: %v", r, err)
		}
	}

	s := schema.DefaultOTelLogs()
	for _, r := range rows {
		t.Run(fmt.Sprintf("number=%d text=%q", r.number, r.text), func(t *testing.T) {
			query := fmt.Sprintf(`{job="api"} | detected_level="%s"`, r.want)
			expr, err := syntax.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", query, err)
			}
			body := fmt.Sprintf("n=%d;t=%s", r.number, r.text)
			var n int
			if err := db.QueryRow("SELECT count() FROM ("+sqlStr+") WHERE Body = ?", append(args, body)...).Scan(&n); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
			if n != 1 {
				t.Errorf("SeverityNumber=%d SeverityText=%q resolved to a detected_level other than %q (matched %d rows)",
					r.number, r.text, r.want, n)
			}
		})
	}
}
