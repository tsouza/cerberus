//go:build integration

package chserver

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// classicServerMaxThreads is the max_threads the default profile of the
// servers below carries: the value ClickHouse sizes from a 16-core host. The
// classic-histogram ceilings are calibrated on two threads, and a statement
// on more threads peaks higher (internal/promql/classic_bucket_window_bound.go,
// "Threads").
const classicServerMaxThreads = 16

// classicCalibratedThreads is the thread count every statement carrying a
// classic-histogram check must run with, whatever the server's default.
const classicCalibratedThreads = "2"

// classicServerThreadsConfig sets the default profile's max_threads to
// classicServerMaxThreads.
func classicServerThreadsConfig() testcontainers.ContainerCustomizer {
	config := fmt.Sprintf("<clickhouse><profiles><default><max_threads>%d</max_threads></default></profiles></clickhouse>",
		classicServerMaxThreads)
	return testcontainers.WithFiles(testcontainers.ContainerFile{
		Reader:            strings.NewReader(config),
		ContainerFilePath: "/etc/clickhouse-server/users.d/max-threads.xml",
		FileMode:          configFileMode,
	})
}

// The admitted shapes, one near each ceiling. Each is admitted by the
// default ceilings at the 1 GiB cap.
var (
	// classicThreadsEstimate is 58,000 series of 20 samples: an estimate of
	// 802,921,600 against the 805,306,368 window ceiling.
	classicThreadsEstimate = classicWindowShape{series: 58_000, samples: 20, spacing: 2 * time.Second}
	// classicThreadsPerSeries is 185 series of 2,950 samples: 150,450
	// entries per series against the 152,520 per-series ceiling. On 26.8 it
	// peaks at 738 MiB on two threads and runs out of the 1 GiB cap on
	// sixteen.
	classicThreadsPerSeries = classicWindowShape{series: 185, samples: 2_950, spacing: 100 * time.Millisecond}
	// classicThreadsMerge is 58,000 disjoint 50-bucket series of one sample:
	// 2,958,000 merged entries against the 2,982,616 bucket-merge ceiling.
	classicThreadsMerge = classicMergeShape{series: 58_000, width: 50, samples: 1, groups: 1}
)

// classicThreadsBuild is the build the probes run on: the bundled 26.8 LTS
// build, on which the thread measurements of
// internal/promql/classic_bucket_window_bound.go were taken. The thread
// bound is a statement setting, the same on every build; the memory of each
// ceiling on each calibrated build is pinned by
// TestClassicBucketWindow_CeilingsAcrossBuilds and
// TestClassicBucketMerge_DisjointLayoutCeilingAcrossBuilds.
const classicThreadsBuild = "clickhouse/clickhouse-server:26.8.36.3-alpine"

// TestClassicBucket_CeilingsHoldOnAManyThreadServer serves a shape near each
// classic-histogram ceiling — the window estimate, the per-series window
// entries, and the bucket merge — through the production handler under the
// 1 GiB cap, on a server whose default profile runs a statement on
// classicServerMaxThreads threads. Each must answer with the quantile its
// seed implies, and each statement must have run on the two threads the
// ceilings are calibrated at.
func TestClassicBucket_CeilingsHoldOnAManyThreadServer(t *testing.T) {
	ctx := context.Background()
	s := startServer(ctx, t, classicThreadsBuild, classicServerThreadsConfig())
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatalf("%s: apply DDL: %v", s.image, err)
	}
	s.requireDefaultMaxThreads(ctx, t)
	mux := classicMergeMux(ctx, t, s, "auto")

	estimate := classicThreadsEstimate.seed(ctx, t, s)
	t.Run("window-estimate", func(t *testing.T) {
		qid := classicThreadsQueryID(s, "window-estimate")
		code, body := serveClassicWindow(chclient.WithQueryID(ctx, qid), mux, "rate", estimate, "5m")
		assertClassicWindowAnswer(t, s, code, body)
		s.assertClassicThreads(ctx, t, qid)
	})
	perSeries := classicThreadsPerSeries.seed(ctx, t, s)
	t.Run("window-per-series", func(t *testing.T) {
		qid := classicThreadsQueryID(s, "window-per-series")
		code, body := serveClassicWindow(chclient.WithQueryID(ctx, qid), mux, "rate", perSeries, "5m")
		assertClassicWindowAnswer(t, s, code, body)
		s.assertClassicThreads(ctx, t, qid)
	})
	merge := s.seedClassicMergeShape(ctx, t, classicThreadsMerge)
	t.Run("bucket-merge", func(t *testing.T) {
		qid := classicThreadsQueryID(s, "bucket-merge")
		code, body := serveClassicMerge(chclient.WithQueryID(ctx, qid), mux, merge, classicThreadsMerge)
		if code != http.StatusOK {
			t.Fatalf("%s: HTTP %d: %s; want the admitted shape answered", s.version, code, body)
		}
		assertClassicMergeQuantile(t, classicThreadsMerge, body)
		s.assertClassicThreads(ctx, t, qid)
	})
}

// requireDefaultMaxThreads checks the server applies classicServerMaxThreads
// to a statement that sets nothing itself, so the probes run on a server
// that would give them that many threads.
func (s *server) requireDefaultMaxThreads(ctx context.Context, t *testing.T) {
	t.Helper()
	var got string
	if err := s.admin.Conn().QueryRow(ctx, "SELECT toString(getSetting('max_threads'))").Scan(&got); err != nil {
		t.Fatalf("%s: read max_threads: %v", s.image, err)
	}
	if got != strconv.Itoa(classicServerMaxThreads) {
		t.Fatalf("%s: default max_threads %s; want %d", s.image, got, classicServerMaxThreads)
	}
}

// assertClassicThreads checks the statement qid finished on
// classicCalibratedThreads threads.
func (s *server) assertClassicThreads(ctx context.Context, t *testing.T, qid string) {
	t.Helper()
	s.flushLogs(ctx, t)
	var threads string
	var peak uint64
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT Settings['max_threads'], memory_usage FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?", qid).Scan(&threads, &peak); err != nil {
		t.Fatalf("%s: read query_log for %s: %v", s.image, qid, err)
	}
	if threads != classicCalibratedThreads {
		t.Fatalf("%s %s: ran with max_threads %q; want %s, the thread count the ceilings are calibrated at",
			s.image, qid, threads, classicCalibratedThreads)
	}
	t.Logf("%s %s: peak memory_usage %.1f MiB", s.version, qid, float64(peak)/(1<<20))
}

// classicThreadsQueryID is a query id unique to one probe on one build.
func classicThreadsQueryID(s *server, probe string) string {
	return fmt.Sprintf("classic-threads-%s-%s-%d", s.version, probe, time.Now().UnixNano())
}
