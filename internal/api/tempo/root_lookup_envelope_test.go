package tempo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

type envelopeQuerier struct {
	rows        []string
	envelopeErr error
	rootErr     error
	envelopeSQL string
	rootSQL     string
	rootArgs    []any
	rootCalls   int
}

func (q *envelopeQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	q.envelopeSQL = sql
	return q.rows, q.envelopeErr
}
func (q *envelopeQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	q.rootSQL = sql
	q.rootArgs = args
	q.rootCalls++
	return nil, q.rootErr
}

func TestResolveTraceRoots_TimestampEnvelope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		enabled     bool
		rows        []string
		envelopeErr error
		rootErr     error
		bounded     bool
		wantErr     error
	}{
		{name: "disabled"},
		{name: "complete", enabled: true, rows: []string{`["1777629600000000000","1777629601000000000","1"]`}, bounded: true},
		{name: "empty", enabled: true, rows: []string{`["0","0","0"]`}},
		{name: "no rows", enabled: true},
		{name: "missing table", enabled: true, envelopeErr: &clickhouse.Exception{Code: 60}},
		{name: "cancelled", enabled: true, envelopeErr: context.Canceled, wantErr: context.Canceled},
		{name: "deadline", enabled: true, envelopeErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
		{name: "root failure", enabled: true, rows: []string{`["1777629600000000000","1777629601000000000","1"]`}, rootErr: context.Canceled, bounded: true, wantErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &envelopeQuerier{rows: tc.rows, envelopeErr: tc.envelopeErr, rootErr: tc.rootErr}
			s := schema.DefaultOTelTraces()
			s.TraceIDTsEnabled = tc.enabled
			h := New(q, s, "test", nil)
			// Duplicate short/padded IDs must count as one trace, not two batches.
			_, err := h.resolveTraceRoots(context.Background(), []string{"17", "00000000000000000000000000000017"})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if got := strings.Contains(q.rootSQL, "fromUnixTimestamp64Nano"); got != tc.bounded {
				t.Fatalf("bounded = %v, want %v: %s", got, tc.bounded, q.rootSQL)
			}
			if !tc.enabled && q.envelopeSQL != "" {
				t.Fatal("disabled lookup queried timestamp table")
			}
			if tc.enabled && !strings.Contains(q.envelopeSQL, "uniqExact") {
				t.Fatalf("coverage must count distinct IDs: %s", q.envelopeSQL)
			}
			if tc.wantErr != nil && tc.rootErr == nil && q.rootCalls != 0 {
				t.Fatal("envelope error must not trigger an unbounded retry")
			}
			if tc.bounded {
				wantHi := int64(1777629601000000000) + int64(chplan.TraceIDTsEndPadSeconds)*int64(time.Second)
				found := false
				for _, a := range q.rootArgs {
					if a == wantHi {
						found = true
					}
				}
				if !found {
					t.Fatalf("padded End missing in args: %v", q.rootArgs)
				}
			}
		})
	}
}

func TestRootLookupEnvelope_PartialCoverage(t *testing.T) {
	t.Parallel()
	q := &envelopeQuerier{rows: []string{`["1777629600000000000","1777629601000000000","1"]`}}
	s := schema.DefaultOTelTraces()
	s.TraceIDTsEnabled = true
	h := New(q, s, "test", nil)
	if _, err := h.resolveTraceRoots(context.Background(), []string{"17", "18"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q.rootSQL, "fromUnixTimestamp64Nano") {
		t.Fatalf("partial cohort must retain unbounded lookup: %s", q.rootSQL)
	}
}

func TestRootLookupEnvelope_CustomSchema(t *testing.T) {
	t.Parallel()
	q := &envelopeQuerier{rows: []string{`["0","0","0"]`}}
	s := schema.DefaultOTelTraces()
	s.TraceIDTsTable = "custom_bounds"
	s.TraceIDColumn = "trace_id"
	s.TraceIDTsStartColumn = "lo"
	s.TraceIDTsEndColumn = "hi"
	h := New(q, s, "test", nil)
	if _, _, _, err := h.rootLookupEnvelope(context.Background(), []string{"17"}); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"`custom_bounds`", "`trace_id`", "`lo`", "`hi`"} {
		if !strings.Contains(q.envelopeSQL, needle) {
			t.Fatalf("missing %s: %s", needle, q.envelopeSQL)
		}
	}
}

func TestRootLookupEnvelope_InvalidBounds(t *testing.T) {
	t.Parallel()
	for _, row := range []string{`["2","1","1"]`, `["0","9223372036854775807","1"]`} {
		q := &envelopeQuerier{rows: []string{row}}
		h := New(q, schema.DefaultOTelTraces(), "test", nil)
		if _, _, covered, err := h.rootLookupEnvelope(context.Background(), []string{"17"}); err != nil || covered {
			t.Fatalf("invalid envelope %s: covered=%v, error=%v", row, covered, err)
		}
	}
}

func TestRootLookupEnvelope_MalformedResponse(t *testing.T) {
	t.Parallel()
	for _, rows := range [][]string{{"not json"}, {`["0"]`}, {`["0","0","bad"]`}, {`["bad","0","1"]`}, {`["0","bad","1"]`}, {`["0","0","1"]`, `["0","0","1"]`}} {
		q := &envelopeQuerier{rows: rows}
		h := New(q, schema.DefaultOTelTraces(), "test", nil)
		if _, _, _, err := h.rootLookupEnvelope(context.Background(), []string{"17"}); err == nil {
			t.Fatalf("malformed response accepted: %v", rows)
		}
	}
}

func TestRootLookupEnvelope_NoConfiguredTable(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	s.TraceIDTsTable = ""
	q := &envelopeQuerier{}
	h := New(q, s, "test", nil)
	if _, _, covered, err := h.rootLookupEnvelope(context.Background(), []string{"17"}); err != nil || covered || q.envelopeSQL != "" {
		t.Fatalf("incomplete configuration: covered=%v, error=%v, SQL=%s", covered, err, q.envelopeSQL)
	}
}
