package drain_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/drain"
)

// findCluster returns the first cluster whose template contains every
// given substring, or nil.
func findCluster(clusters []*drain.Cluster, subs ...string) *drain.Cluster {
	for _, c := range clusters {
		s := c.String()
		ok := true
		for _, sub := range subs {
			if !contains(s, sub) {
				ok = false
				break
			}
		}
		if ok {
			return c
		}
	}
	return nil
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestDrain_FoldsVariableTokens — structurally identical lines that
// differ only in numeric IDs collapse into one cluster, with the
// varying positions generalised to `<_>` and the constant positions
// preserved verbatim.
func TestDrain_FoldsVariableTokens(t *testing.T) {
	t.Parallel()
	d := drain.New(drain.DefaultConfig())
	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	lines := []string{
		"GET /api/users/1 status=200 latency=5ms",
		"GET /api/users/2 status=200 latency=7ms",
		"GET /api/users/42 status=200 latency=4ms",
		"GET /api/users/1337 status=200 latency=11ms",
	}
	for i, l := range lines {
		d.Train(l, base.Add(time.Duration(i)*time.Second).UnixNano())
	}

	clusters := d.Clusters()
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d: %+v", len(clusters), clusters)
	}
	got := clusters[0].String()
	const want = "GET <_> status=200 <_>"
	if got != want {
		t.Fatalf("template = %q, want %q", got, want)
	}
	if clusters[0].Count() != 4 {
		t.Fatalf("count = %d, want 4", clusters[0].Count())
	}
	var sum int64
	for _, s := range clusters[0].Samples() {
		sum += s.Count
	}
	if sum != 4 {
		t.Fatalf("sample count sum = %d, want 4", sum)
	}
}

// TestDrain_SeparatesDistinctShapes — lines whose constant (non-numeric)
// leading tokens differ land in distinct clusters; the variable-only
// positions still generalise within each.
func TestDrain_SeparatesDistinctShapes(t *testing.T) {
	t.Parallel()
	d := drain.New(drain.DefaultConfig())
	ts := time.Now().UnixNano()
	for _, l := range []string{
		"GET /api/foo/1 status=200 latency=5ms",
		"GET /api/foo/2 status=200 latency=7ms",
		"POST /bar status=500 latency=22ms",
		"POST /bar status=500 latency=18ms",
	} {
		d.Train(l, ts)
	}

	clusters := d.Clusters()
	if len(clusters) != 2 {
		t.Fatalf("expected 2 clusters, got %d: %+v", len(clusters), clusters)
	}
	get := findCluster(clusters, "GET", "200", "<_>")
	if get == nil {
		t.Fatalf("no GET/200 cluster: %+v", clusters)
	}
	post := findCluster(clusters, "POST", "/bar", "500")
	if post == nil {
		t.Fatalf("no POST/bar/500 cluster: %+v", clusters)
	}
	// The POST cluster keeps the constant `/bar` literal (no digit, never
	// varied) and generalises only the latency suffix.
	if post.String() != "POST /bar status=500 <_>" {
		t.Fatalf("POST template = %q, want %q", post.String(), "POST /bar status=500 <_>")
	}
}

// TestDrain_EmptyLinesIgnored — blank / whitespace-only lines never
// create a cluster.
func TestDrain_EmptyLinesIgnored(t *testing.T) {
	t.Parallel()
	d := drain.New(drain.DefaultConfig())
	ts := time.Now().UnixNano()
	d.Train("", ts)
	d.Train("   \t  ", ts)
	if n := len(d.Clusters()); n != 0 {
		t.Fatalf("expected 0 clusters from empty input, got %d", n)
	}
}

// TestDrain_SampleBucketing — observations bucket onto the configured
// resolution grid: four lines within one bucket window collapse to a
// single sample whose count is the total, timestamped at the bucket
// start in whole seconds.
func TestDrain_SampleBucketing(t *testing.T) {
	t.Parallel()
	cfg := drain.DefaultConfig()
	cfg.SampleResolution = 10 * time.Second
	d := drain.New(cfg)
	// Bucket-aligned base (multiple of 10s) so the truncated timestamp is
	// the base itself.
	base := time.Unix(1778760000, 0).UTC()
	for i := 0; i < 4; i++ {
		d.Train("GET /x/1 ok done", base.Add(time.Duration(i)*time.Second).UnixNano())
	}
	clusters := d.Clusters()
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}
	samples := clusters[0].Samples()
	if len(samples) != 1 {
		t.Fatalf("expected 1 bucket, got %d: %+v", len(samples), samples)
	}
	if samples[0].TimestampUnixSec != 1778760000 {
		t.Fatalf("bucket ts = %d, want 1778760000", samples[0].TimestampUnixSec)
	}
	if samples[0].Count != 4 {
		t.Fatalf("bucket count = %d, want 4", samples[0].Count)
	}
}

// TestDrain_SplitsAcrossBuckets — observations far apart in time land in
// separate buckets while still folding into one cluster, and the bucket
// counts still sum to the line count.
func TestDrain_SplitsAcrossBuckets(t *testing.T) {
	t.Parallel()
	cfg := drain.DefaultConfig()
	cfg.SampleResolution = 10 * time.Second
	d := drain.New(cfg)
	base := time.Unix(1778760000, 0).UTC()
	d.Train("GET /x/1 ok done", base.UnixNano())
	d.Train("GET /x/2 ok done", base.Add(time.Minute).UnixNano())
	clusters := d.Clusters()
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}
	samples := clusters[0].Samples()
	if len(samples) != 2 {
		t.Fatalf("expected 2 buckets, got %d: %+v", len(samples), samples)
	}
	if samples[0].TimestampUnixSec >= samples[1].TimestampUnixSec {
		t.Fatalf("samples not ascending: %+v", samples)
	}
	var sum int64
	for _, s := range samples {
		sum += s.Count
	}
	if sum != 2 {
		t.Fatalf("sample sum = %d, want 2", sum)
	}
}
