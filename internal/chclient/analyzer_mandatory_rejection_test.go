package chclient

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// analyzerMandatoryMessage is the exception text ClickHouse 26.9 answers
// `SETTINGS enable_analyzer=0` with, captured from 26.9.25.15 over HTTP (the
// native protocol answers the same text without the trailing build).
const analyzerMandatoryMessage = "Setting 'enable_analyzer' cannot be disabled: the analyzer is mandatory since v26.9 and the old query analysis " +
	"is no longer supported. Remove 'enable_analyzer = 0' from the query, the session, the settings profile and the client configuration. " +
	"To compare with the old query analysis, use a ClickHouse version older than v26.9. (SETTING_CONSTRAINT_VIOLATION) (version 26.9.25.15 (official build))"

// TestParseAnalyzerMandatory pins the refusal read off a node whose analyzer
// is mandatory, and that no other SETTING_CONSTRAINT_VIOLATION — a hardened
// profile constraining a setting, enable_analyzer included — reads as one.
func TestParseAnalyzerMandatory(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ex   *clickhouse.Exception
		want unknownName
	}{
		{
			"HTTP 26.9",
			&clickhouse.Exception{Code: chCodeSettingConstraintViolation, Message: analyzerMandatoryMessage},
			unknownName{kind: unknownNameSetting, name: "enable_analyzer", nodeVersion: "26.9.25.15"},
		},
		{
			"native 26.9, the alias",
			&clickhouse.Exception{Code: chCodeSettingConstraintViolation, Message: "Setting 'allow_experimental_analyzer' cannot be disabled: " +
				"the analyzer is mandatory since v26.9 and the old query analysis is no longer supported."},
			unknownName{kind: unknownNameSetting, name: "allow_experimental_analyzer"},
		},
	}
	for _, tc := range cases {
		got, ok := parseAnalyzerMandatory(fmt.Errorf("wrapped: %w", tc.ex))
		if !ok || got != tc.want {
			t.Errorf("%s: parseAnalyzerMandatory = %+v, %v; want %+v", tc.name, got, ok, tc.want)
		}
	}

	for name, err := range map[string]error{
		"profile constraint":           &clickhouse.Exception{Code: chCodeSettingConstraintViolation, Message: "Setting max_threads shouldn't be greater than 8"},
		"profile pins enable_analyzer": &clickhouse.Exception{Code: chCodeSettingConstraintViolation, Message: "Setting enable_analyzer should not be changed"},
		"same text under another code": &clickhouse.Exception{Code: chCodeUnknownSetting, Message: analyzerMandatoryMessage},
		"no exception":                 errors.New("dial: refused"),
		"nil":                          nil,
	} {
		if got, ok := parseAnalyzerMandatory(err); ok {
			t.Errorf("%s: parseAnalyzerMandatory = %+v; want no refusal", name, got)
		}
	}
}

// analyzerMandatoryConn refuses every statement the way a 26.9 node refuses
// enable_analyzer=0.
type analyzerMandatoryConn struct {
	staleConn
}

func (c *analyzerMandatoryConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	c.calls.Add(1)
	return nil, &clickhouse.Exception{Code: chCodeSettingConstraintViolation, Message: analyzerMandatoryMessage}
}

// TestQueryStrings_AnalyzerMandatoryRefusalRequestsReprobe: a node whose
// analyzer is mandatory refusing enable_analyzer=0 reaches the hook — the
// early re-probe trigger — once, marked AnalyzerMandatory, while the query
// itself is not retried and the refusal is not counted as an unknown name.
// A capability canary's refusal reaches nothing.
func TestQueryStrings_AnalyzerMandatoryRefusalRequestsReprobe(t *testing.T) {
	t.Parallel()
	conn := &analyzerMandatoryConn{}
	c := newWithConn(conn)
	rec, reader := withObservedRejections(c)

	ctx := WithQuerySetting(context.Background(), "enable_analyzer", 0)
	_, err := c.QueryStrings(ctx, "SELECT 1")
	var ex *clickhouse.Exception
	if !errors.As(err, &ex) || ex.Code != chCodeSettingConstraintViolation {
		t.Fatalf("err = %#v; want the SETTING_CONSTRAINT_VIOLATION returned", err)
	}
	if got := conn.calls.Load(); got != 1 {
		t.Errorf("statements sent = %d; want 1, no retry", got)
	}
	want := UnknownNameRejection{Kind: unknownNameSetting, Name: "enable_analyzer", NodeVersion: "26.9.25.15", AnalyzerMandatory: true}
	if got := rec.all(); len(got) != 1 || got[0] != want {
		t.Errorf("hook received %+v; want [%+v]", got, want)
	}
	if _, exported, points := counterSum(t, reader, unknownNameCounter, map[string]string{"name": "enable_analyzer"}); !exported || points != 0 {
		t.Errorf("unknown-name counter: exported %v with %d enable_analyzer series; want it exported with none", exported, points)
	}

	if _, err := c.QueryStrings(withCapabilityCanary(ctx), "SELECT 1"); err == nil {
		t.Fatal("canary: want the refusal returned")
	}
	if got := rec.all(); len(got) != 1 {
		t.Errorf("hook received %+v after a canary's refusal; want the one earlier rejection only", got)
	}
}
