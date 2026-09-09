//go:build chdb

// /index/volume groups on the label set it SERVES, not on the one the
// store holds (issue #3246).
//
// Cerberus groups in ClickHouse over raw OTel attribute keys and rewrites
// them to Prometheus label names on the way out, and that rewrite is not
// injective: `a.b` and `a_b` are two stored keys and one served name.
// Nothing merged two groups that landed on the same served name, so the
// endpoint answered a vector carrying two samples under one byte-identical
// label set, each holding part of one series' byte volume.
//
// Upstream has no such split because Loki normalises at INGEST — its OTLP
// handler rewrites attribute names before the stream is written, so the
// two spellings are ONE stream by the time any volume is accumulated, and
// its volume is the sum. Cerberus normalises on read, so it has to reach
// the same place by grouping on the rewritten key.
//
// Only an engine can show any of this: a stub querier hands back rows the
// SQL never grouped.

package loki_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
)

// normGroupingRow is one seeded stream: the attribute entries it carries
// beyond `job="api"`, written as CH `map(...)` arguments, and the byte
// length of its body (which IS the group's volume, since
// `sum(length(Body))` is what the endpoint aggregates).
type normGroupingRow struct {
	attrs string
	bytes int
}

// normGroupingSeed writes the given streams under one `{job="api"}`
// selector at one timestamp, so each case below states its storage shape
// in one line and nothing else varies between them.
func normGroupingSeed(rows ...normGroupingRow) string {
	values := make([]string, 0, len(rows))
	for _, r := range rows {
		values = append(values, fmt.Sprintf(
			"(toDateTime64('2026-05-14 12:00:00.000', 9), '%s', 'svc', map('job','api',%s))",
			strings.Repeat("x", r.bytes), r.attrs,
		))
	}
	return `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    ServiceName String,
    ResourceAttributes Map(String, String)
) ENGINE = Memory;
INSERT INTO otel_logs (Timestamp, Body, ServiceName, ResourceAttributes) VALUES
    ` + strings.Join(values, ",\n    ") + ";"
}

// assertVolumeSample fails unless the sample carries exactly wantMetric
// and wantValue, naming both sides so a wrong merge reads as a wrong
// number rather than as a reflect dump.
func assertVolumeSample(t *testing.T, got loki.VectorSample, wantMetric map[string]string, wantValue string) {
	t.Helper()
	if !sameLabelMap(got.Metric, wantMetric) {
		t.Errorf("sample label set = %v, want %v", got.Metric, wantMetric)
	}
	if fmt.Sprint(got.Value[1]) != wantValue {
		t.Errorf("sample value for %v = %v, want %s", got.Metric, got.Value[1], wantValue)
	}
}

// TestIndexVolume_ChDB_TwoStoredKeysServeOneSample is the wrong answer
// #3246 recorded, measured through the handler.
//
// Two streams under one selector, one carrying the attribute key `a.b` and
// the other `a_b`, at ten and five body bytes. Both serve under the label
// set `{a_b="1", job="api"}`, so the answer upstream gives is ONE sample
// worth 15 B. Cerberus gave two samples, 10 and 5, with byte-identical
// label maps — a vector that carries the same key twice, of which a
// consumer keying by label set sees one silently win.
//
// It discriminates: with the served-key regrouping removed from
// buildIndexVolumeSQL the response is two samples, and the count assertion
// names both the length and the two values it saw.
func TestIndexVolume_ChDB_TwoStoredKeysServeOneSample(t *testing.T) {
	srvURL := newVolumeServer(t, normGroupingSeed(
		normGroupingRow{attrs: "'a.b','1'", bytes: 10},
		normGroupingRow{attrs: "'a_b','1'", bytes: 5},
	))

	got := queryVolume(t, srvURL, 100)
	if len(got) != 1 {
		t.Fatalf("got %d samples %v, want exactly 1 — `a.b` and `a_b` serve under one label set, "+
			"so they are one series and their volumes sum", len(got), got)
	}
	assertVolumeSample(t, got[0], map[string]string{"a_b": "1", "job": "api"}, "15")
}

// TestIndexVolume_ChDB_MergedGroupOutranksTheCap is the half that a
// Go-side merge over the returned rows cannot answer, and the reason the
// regrouping is in the SQL.
//
// The cut is a function of the volumes, and merging changes them. Here the
// two halves of the `a_b` series are 10 B each and a third stream is 12 B,
// so before merging the 12 B stream is the single largest group and after
// merging it is second. At `limit=1` the answer is therefore the 20 B
// merged series — which a cut taken over the STORED volumes has already
// discarded both halves of, whatever Go does with what comes back.
func TestIndexVolume_ChDB_MergedGroupOutranksTheCap(t *testing.T) {
	srvURL := newVolumeServer(t, normGroupingSeed(
		normGroupingRow{attrs: "'a.b','1'", bytes: 10},
		normGroupingRow{attrs: "'a_b','1'", bytes: 10},
		normGroupingRow{attrs: "'pod','solo'", bytes: 12},
	))

	got := queryVolume(t, srvURL, 1)
	if len(got) != 1 {
		t.Fatalf("got %d samples %v, want exactly 1 at limit=1", len(got), got)
	}
	assertVolumeSample(t, got[0], map[string]string{"a_b": "1", "job": "api"}, "20")
}

// TestIndexVolume_ChDB_OneStreamTwoSpellingsChargesOneLabel is the
// aggregateBy=labels face of the same rewrite. A single stream carrying
// BOTH `a.b` and `a_b` is, upstream, a stream with one `a_b` label — the
// ingest-time rewrite collapsed the pair before the stream existed — so it
// owes that label ONE charge of its bytes.
//
// Exploding the stored keys charged `a_b` twice, once under each spelling,
// and the response carried two samples both named `a_b`. It discriminates
// for the same reason its sibling does: it counts the samples and the
// value, and both move when the ARRAY JOIN goes back to the stored keys.
func TestIndexVolume_ChDB_OneStreamTwoSpellingsChargesOneLabel(t *testing.T) {
	srvURL := newVolumeServer(t, normGroupingSeed(
		normGroupingRow{attrs: "'a.b','dotted','a_b','natural'", bytes: 10},
	))

	got := queryVolumeAggregateBy(t, srvURL, 100, "labels")
	names := make([]string, 0, len(got))
	for _, s := range got {
		for k := range s.Metric {
			names = append(names, k+"="+fmt.Sprint(s.Value[1]))
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples %v, want exactly 2 (`a_b` and `job`, one charge each)", len(got), names)
	}
	assertVolumeSample(t, got[0], map[string]string{"a_b": ""}, "10")
	assertVolumeSample(t, got[1], map[string]string{"job": ""}, "10")
}
