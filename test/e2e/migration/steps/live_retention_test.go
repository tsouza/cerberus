package steps

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"

	"github.com/tsouza/cerberus/internal/migrate"
)

// The runway/retention pairs the MIG-14 comparison is driven with below. They
// exist because the live scenario CANNOT reach its own blocker branch: the
// tier-1 collector provisions a month of retention and the committed corpus
// reads back minutes, so `have < want` is unreachable there no matter how
// wrong the code is. These drive both directions of that comparison directly.
const (
	runwayCorpusNeeds    = 90 * 24 * time.Hour
	ttlShorterThanRunway = 30 * 24 * time.Hour
	ttlLongerThanRunway  = 365 * 24 * time.Hour
)

// The archetype and query one comparison is driven over. Nothing is read off
// disk: the point is the arithmetic between a runway and a retention, and the
// fixtures that feed it are exercised by the tier-0 and tier-1 lanes.
const (
	retentionArchetype = "three-signal"
	retentionSource    = "rule:a.yml/g/rps"
	retentionExpr      = "sum(rate(http_requests_total[5m]))"
)

// TestRetentionBySignalReadsTheCollectorsBareColumnTTL pins the dialect that
// broke the live read: the OTel-CH exporter emits `TTL <column> +
// toIntervalX(n)` with no toDateTime(...) wrapper, and the collector is the
// only schema authority on the tier-1 stack. A parser that understood only
// cerberus's own wrapped form read every live table as carrying no retention.
func TestRetentionBySignalReadsTheCollectorsBareColumnTTL(t *testing.T) {
	t.Parallel()

	metrics := firstTwoMetricsTables()
	stmts := []string{
		"CREATE TABLE otel." + metrics[0] + " (a Int) ENGINE = MergeTree PARTITION BY toDate(TimeUnix) TTL TimeUnix + toIntervalDay(30) SETTINGS index_granularity = 8192",
		"CREATE TABLE otel." + metrics[1] + " (a Int) ENGINE = MergeTree TTL TimeUnix + toIntervalDay(30)",
		"CREATE TABLE otel.otel_logs (a Int) ENGINE = MergeTree TTL Timestamp + toIntervalHour(6)",
	}
	got, err := retentionBySignal("the live collector-provisioned tables", stmts)
	if err != nil {
		t.Fatal(err)
	}
	if want := 30 * 24 * time.Hour; got[signalMetrics] != want {
		t.Errorf("metrics retention = %v, want %v", got[signalMetrics], want)
	}
	if want := 6 * time.Hour; got[signalLogs] != want {
		t.Errorf("logs retention = %v, want %v", got[signalLogs], want)
	}
}

// TestRetentionBySignalRefusesDDLItCannotRead is the regression pin for the
// fail-open this parser used to have: statements it understands nothing of
// came back as an empty map and a nil error, which every caller then read as
// "this deployment provisions no retention" — a refusal the operator never
// earned, indistinguishable from one they did.
func TestRetentionBySignalRefusesDDLItCannotRead(t *testing.T) {
	t.Parallel()

	for name, stmts := range map[string][]string{
		"a dialect the pattern misses": {
			"CREATE TABLE otel.otel_logs (a Int) ENGINE = MergeTree TTL Timestamp INTERVAL 6 HOUR",
		},
		"tables that carry no TTL at all": {
			"CREATE TABLE otel.otel_logs (a Int) ENGINE = MergeTree",
			"CREATE TABLE otel.otel_traces (a Int) ENGINE = MergeTree",
		},
		"a TTL on nothing this harness recognises as a signal": {
			"CREATE TABLE otel.something_else (a Int) ENGINE = MergeTree TTL t + toIntervalDay(1)",
		},
		"no statements at all": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := retentionBySignal("the live clickhouse database otel", stmts)
			if err == nil {
				t.Fatalf("unreadable DDL yielded %v and no error, so a caller would read it as an unset retention", got)
			}
		})
	}
}

// retentionWorld builds the World state MIG-14's live comparison reads: one
// archetype, one PromQL query, and the runway that query reads back.
func retentionWorld(t *testing.T, ttl time.Duration) *World {
	t.Helper()

	w := NewWorld(t.TempDir(), "")
	w.archetypes = []string{retentionArchetype}
	w.corpus = map[string]migrate.Corpus{retentionArchetype: {
		Queries: []migrate.CorpusQuery{{
			Expr:   retentionExpr,
			Source: retentionSource,
			Kind:   migrate.KindRecord,
			Lang:   migrate.LangPromQL,
		}},
	}}
	w.lookback = map[string]migrate.Lookback{retentionArchetype: {
		Longest: model.Duration(runwayCorpusNeeds),
		ByQuery: []migrate.QueryLookback{{
			Expr:   retentionExpr,
			Source: retentionSource,
			Kind:   migrate.KindRecord,
			Reach:  model.Duration(runwayCorpusNeeds),
		}},
	}}
	w.retention = signalRetention{read: true, bySignal: map[string]time.Duration{signalMetrics: ttl}}
	return w
}

// TestLiveRetentionBlocksARunwayLongerThanTheTTL walks MIG-14's blocker branch,
// which its own Tier-1 scenario cannot reach: the tier-1 collector's month of
// retention dwarfs the committed corpus's reach, so the live scenario only
// ever exercises the covered direction. Both directions are asserted here, so
// the comparison is known to discriminate rather than merely to have passed.
func TestLiveRetentionBlocksARunwayLongerThanTheTTL(t *testing.T) {
	t.Parallel()

	err := retentionWorld(t, ttlShorterThanRunway).thenLiveRetentionCoversLookback()
	if err == nil {
		t.Fatalf("a retention of %s passed a corpus that reads back %s", ttlShorterThanRunway, runwayCorpusNeeds)
	}
	if want := "short of"; !strings.Contains(err.Error(), want) {
		t.Errorf("the blocker reads %q, which does not say the retention falls %s the runway", err, want)
	}

	if err := retentionWorld(t, ttlLongerThanRunway).thenLiveRetentionCoversLookback(); err != nil {
		t.Fatalf("a retention of %s failed a corpus that reads back only %s: %v", ttlLongerThanRunway, runwayCorpusNeeds, err)
	}
}

// TestLiveRetentionBlocksASignalItNeverRead proves the other half of the same
// fail-closed rule: a signal the corpus needs a runway for, whose live tables
// yielded no retention, blocks. Absence is never read as "unbounded, therefore
// fine" — that is the exact shape a silent parse failure produces.
func TestLiveRetentionBlocksASignalItNeverRead(t *testing.T) {
	t.Parallel()

	w := retentionWorld(t, ttlLongerThanRunway)
	w.retention = signalRetention{read: true, bySignal: map[string]time.Duration{}}
	if err := w.thenLiveRetentionCoversLookback(); err == nil {
		t.Fatal("a signal with no live retention at all passed the runway comparison")
	}
}

// TestLiveRetentionRefusesACorpusThatReadsBackNothing proves the vacuity guard:
// a corpus whose every query reaches back no distance would make the
// comparison true for any retention, including one misread as zero, so it is a
// failure rather than a pass nobody earned.
func TestLiveRetentionRefusesACorpusThatReadsBackNothing(t *testing.T) {
	t.Parallel()

	w := retentionWorld(t, ttlLongerThanRunway)
	lb := w.lookback[retentionArchetype]
	lb.Longest = 0
	lb.ByQuery[0].Reach = 0
	w.lookback[retentionArchetype] = lb
	if err := w.thenLiveRetentionCoversLookback(); err == nil {
		t.Fatal("a corpus that reads back no distance at all passed the runway comparison")
	}
}
