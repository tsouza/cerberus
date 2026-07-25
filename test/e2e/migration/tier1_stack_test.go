//go:build migration_tier1

// Package migration holds the assertions for the Tier-1 migration substrate
// (Layer 14): the pinned reference stack that `cerberus migrate verify`
// replays a harvested corpus against.
//
// Gated by the `migration_tier1` build tag so the required `check` lane's
// `go test ./...` never reaches for Docker — the same posture the `chdb` and
// `agpl_oracle` tags take.
//
// Run via: just migration-tier1-up && just migration-tier1-run
package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// Endpoint defaults mirror the published port map in
// tiers/tier1-dual/docker-compose.dual.yml. Every one is env-overridable so
// the same assertions can run against a stack published on other ports.
const (
	defaultCHAddr      = "127.0.0.1:27000"
	defaultPromURL     = "http://127.0.0.1:27090"
	defaultLokiURL     = "http://127.0.0.1:27100"
	defaultTempoURL    = "http://127.0.0.1:27200"
	defaultTempoOTLP   = "127.0.0.1:27201"
	defaultCerberusURL = "http://127.0.0.1:27080"
	defaultCHDatabase  = "otel"
	defaultCHUsername  = "cerberus"
	defaultCHPassword  = "cerberus"
	chDatabaseEnvKey   = "TIER1_CH_DATABASE"
	chAddrEnvKey       = "TIER1_CH_ADDR"
	chUsernameEnvKey   = "TIER1_CH_USERNAME"
	chPasswordEnvKey   = "TIER1_CH_PASSWORD"
	promURLEnvKey      = "TIER1_PROM_URL"
	lokiURLEnvKey      = "TIER1_LOKI_URL"
	tempoURLEnvKey     = "TIER1_TEMPO_URL"
	tempoOTLPEnvKey    = "TIER1_TEMPO_OTLP_ADDR"
	cerberusURLEnvKey  = "TIER1_CERBERUS_URL"
)

// Budgets. Each is a bounded deadline on a poll loop — never a sleep, never a
// retry-then-continue: every one of them fails the test when it expires.
const (
	// schemaWait covers a cold boot where the collector's clickhouseexporter
	// has not yet run its create-schema DDL when the test starts.
	schemaWait = 3 * time.Minute
	// referenceReadyWait covers the reference backends' cold start. Loki's
	// single-binary mode alone holds ~15s after start before /ready flips.
	referenceReadyWait = 90 * time.Second
	// cerberusReadyWait covers cerberus serving unready until the collector
	// has provisioned every table its read side expects.
	cerberusReadyWait = 2 * time.Minute
	// readbackWait covers a reference backend's ingest-to-queryable lag: the
	// gap between a successful push and the record appearing in a query.
	readbackWait = 60 * time.Second
	// readbackPoll is the gap between read-back attempts.
	readbackPoll = time.Second
	// httpTimeout bounds one read-back request.
	httpTimeout = 15 * time.Second
	// errBodyLimit caps how much of a failing response body is quoted back.
	errBodyLimit = 4096
	// responseBodyLimit caps a SUCCESSFUL response body. It is far above
	// errBodyLimit on purpose: a parity read returns the whole fixture window
	// (thousands of log entries, hundreds of samples), and a limit that
	// truncated it would surface as a JSON decode error rather than as the
	// oversized response it is.
	responseBodyLimit = 32 << 20
)

// The substrate probe. One metric series, one log stream, one trace: the
// smallest fixture that exercises every write path the parity lane depends on.
// The names are deliberately outside the fixture namespace a migration corpus
// selects, so a probe record can never be mistaken for fixture data.
const (
	probeMetric      = "cerberus_migration_substrate_probe"
	probeMetricLabel = "probe"
	probeMetricValue = 42
	probeJob         = "cerberus-migration-substrate"
	probeLogLine     = "tier1 substrate probe"
	probeService     = "substrate-probe"
	probeSpanName    = "substrate-probe-span"
	probeSpanMillis  = 250
	probeTag         = "tier1"
	// detectedLevelKey is the structured-metadata key reference Loki's level
	// discovery writes; probeLevel is the value the probe declares so nothing
	// is inferred.
	detectedLevelKey = "detected_level"
	probeLevel       = "info"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// TestTier1Substrate asserts the Tier-1 stack satisfies the contract the
// migration parity lane rests on: ClickHouse is schema-provisioned by the
// collector, cerberus serves all three heads off that schema from one address,
// and each reference backend accepts a write and returns exactly what was
// written. Sub-tests share the stack and run in order.
func TestTier1Substrate(t *testing.T) {
	ctx := context.Background()

	t.Run("clickhouse_schema_authority", func(t *testing.T) {
		conn, err := seed.DialCH(ctx, seed.CHConfig{
			Addr:     envOr(chAddrEnvKey, defaultCHAddr),
			Database: envOr(chDatabaseEnvKey, defaultCHDatabase),
			Username: envOr(chUsernameEnvKey, defaultCHUsername),
			Password: envOr(chPasswordEnvKey, defaultCHPassword),
		})
		if err != nil {
			t.Fatalf("dial clickhouse: %v", err)
		}
		defer func() { _ = conn.Close() }()

		var one uint8
		if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Fatalf("SELECT 1: %v", err)
		}
		if one != 1 {
			t.Fatalf("SELECT 1 returned %d, want 1", one)
		}

		database := envOr(chDatabaseEnvKey, defaultCHDatabase)
		if err := seed.WaitForTables(ctx, conn, database, seed.ExporterTables, schemaWait); err != nil {
			t.Fatalf("collector never provisioned the OTel schema: %v", err)
		}
		missing, err := seed.MissingTables(ctx, conn, database, seed.ExporterTables)
		if err != nil {
			t.Fatalf("introspect tables: %v", err)
		}
		if len(missing) != 0 {
			t.Fatalf("tables missing after the wait returned: %v", missing)
		}
	})

	t.Run("cerberus_serves_three_heads", func(t *testing.T) {
		base := envOr(cerberusURLEnvKey, defaultCerberusURL)

		// /readyz is the live schema-drift detector: cerberus auto-create is
		// off, so it only reports ready once it has found every table the
		// collector's exporter created under the names cerberus reads.
		if err := seed.WaitHTTPOK(ctx, base+"/readyz", cerberusReadyWait); err != nil {
			t.Fatalf("cerberus never became ready: %v", err)
		}

		var promStatus struct {
			Status string `json:"status"`
		}
		getJSON(t, ctx, base+"/api/v1/status/buildinfo", &promStatus)
		if promStatus.Status != "success" {
			t.Fatalf("prom head buildinfo status = %q, want %q", promStatus.Status, "success")
		}

		var lokiStatus struct {
			Status string `json:"status"`
		}
		getJSON(t, ctx, base+"/loki/api/v1/labels", &lokiStatus)
		if lokiStatus.Status != "success" {
			t.Fatalf("loki head labels status = %q, want %q", lokiStatus.Status, "success")
		}

		body := getBody(t, ctx, base+"/api/echo")
		if string(body) != "echo" {
			t.Fatalf("tempo head /api/echo = %q, want %q", string(body), "echo")
		}
	})

	t.Run("prometheus_remote_write_roundtrip", func(t *testing.T) {
		base := envOr(promURLEnvKey, defaultPromURL)
		if err := seed.WaitHTTPOK(ctx, base+"/-/ready", referenceReadyWait); err != nil {
			t.Fatalf("reference prometheus never became ready: %v", err)
		}

		anchor := time.Now().UTC().Truncate(time.Second)
		labels := map[string]string{"__name__": probeMetric, probeMetricLabel: probeTag}
		err := seed.WriteSeries(ctx, base, []seed.Series{{
			Labels:  labels,
			Samples: []seed.Sample{{Time: anchor, Value: probeMetricValue}},
		}})
		if err != nil {
			t.Fatalf("remote-write the probe series: %v", err)
		}

		query := base + "/api/v1/query?query=" + url.QueryEscape(probeMetric) +
			"&time=" + strconv.FormatInt(anchor.Unix(), 10)
		var last error
		deadline := time.Now().Add(readbackWait)
		for {
			last = assertPromProbe(ctx, query, anchor)
			if last == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("reference prometheus never returned the written sample within %s: %v", readbackWait, last)
			}
			time.Sleep(readbackPoll)
		}
	})

	t.Run("loki_push_roundtrip", func(t *testing.T) {
		base := envOr(lokiURLEnvKey, defaultLokiURL)
		if err := seed.WaitHTTPOK(ctx, base+"/ready", referenceReadyWait); err != nil {
			t.Fatalf("reference loki never became ready: %v", err)
		}

		anchor := time.Now().UTC().Truncate(time.Millisecond)
		labels := map[string]string{"job": probeJob, "service_name": probeService}
		err := seed.PushStreams(ctx, base, []seed.Stream{{
			Labels: labels,
			Entries: []seed.Entry{{
				Time: anchor,
				Line: probeLogLine,
				// Reference Loki runs level discovery: a record pushed without
				// a level gets `detected_level` synthesised as structured
				// metadata, and query responses surface structured metadata
				// alongside the stream labels. Declaring the level makes the
				// returned label set exact instead of carrying an inferred
				// "unknown" — the same reason the migration fixture writes
				// detected_level on both sides rather than letting each
				// backend derive its own.
				StructuredMetadata: map[string]string{detectedLevelKey: probeLevel},
			}},
		}})
		if err != nil {
			t.Fatalf("push the probe stream: %v", err)
		}
		wantLabels := map[string]string{
			"job":            probeJob,
			"service_name":   probeService,
			detectedLevelKey: probeLevel,
		}

		query := base + "/loki/api/v1/query_range?query=" +
			url.QueryEscape(`{job="`+probeJob+`"}`) +
			"&start=" + strconv.FormatInt(anchor.Add(-time.Minute).UnixNano(), 10) +
			"&end=" + strconv.FormatInt(anchor.Add(time.Minute).UnixNano(), 10) +
			"&direction=forward&limit=10"
		var last error
		deadline := time.Now().Add(readbackWait)
		for {
			last = assertLokiProbe(ctx, query, anchor, wantLabels)
			if last == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("reference loki never returned the pushed entry within %s: %v", readbackWait, last)
			}
			time.Sleep(readbackPoll)
		}
	})

	t.Run("tempo_otlp_roundtrip", func(t *testing.T) {
		base := envOr(tempoURLEnvKey, defaultTempoURL)
		if err := seed.WaitHTTPOK(ctx, base+"/ready", referenceReadyWait); err != nil {
			t.Fatalf("reference tempo never became ready: %v", err)
		}

		// Identifiers are hash-derived from a fixed seed string plus the
		// anchor, so they are byte-stable within a run and distinct between
		// runs against a stack that was not torn down.
		anchor := time.Now().UTC().Truncate(time.Second)
		digest := sha256.Sum256([]byte(probeSpanName + anchor.Format(time.RFC3339Nano)))
		var traceID [16]byte
		var spanID [8]byte
		copy(traceID[:], digest[:16])
		copy(spanID[:], digest[16:24])

		err := seed.PushTraces(ctx, envOr(tempoOTLPEnvKey, defaultTempoOTLP), []seed.Trace{{
			ServiceName: probeService,
			Spans: []seed.Span{{
				TraceID:    traceID,
				SpanID:     spanID,
				Name:       probeSpanName,
				Kind:       otlptrace.Span_SPAN_KIND_SERVER,
				StatusCode: otlptrace.Status_STATUS_CODE_OK,
				Start:      anchor,
				End:        anchor.Add(probeSpanMillis * time.Millisecond),
				Attributes: map[string]string{probeMetricLabel: probeTag},
			}},
		}})
		if err != nil {
			t.Fatalf("push the probe trace: %v", err)
		}

		traceURL := base + "/api/traces/" + hex.EncodeToString(traceID[:])
		var last error
		deadline := time.Now().Add(readbackWait)
		for {
			last = assertTempoProbe(ctx, traceURL)
			if last == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("reference tempo never returned the pushed trace within %s: %v", readbackWait, last)
			}
			time.Sleep(readbackPoll)
		}
	})
}

// assertPromProbe requires the reference to hold exactly one series carrying
// exactly the written label set, value and timestamp.
func assertPromProbe(ctx context.Context, query string, anchor time.Time) error {
	var got struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := fetchJSON(ctx, query, &got); err != nil {
		return err
	}
	if got.Status != "success" {
		return fmt.Errorf("query status = %q, want %q", got.Status, "success")
	}
	if len(got.Data.Result) != 1 {
		return fmt.Errorf("got %d series, want exactly 1", len(got.Data.Result))
	}
	res := got.Data.Result[0]
	want := map[string]string{"__name__": probeMetric, probeMetricLabel: probeTag}
	if len(res.Metric) != len(want) {
		return fmt.Errorf("label set %v, want %v", res.Metric, want)
	}
	for k, v := range want {
		if res.Metric[k] != v {
			return fmt.Errorf("label %s = %q, want %q", k, res.Metric[k], v)
		}
	}
	if len(res.Value) != 2 {
		return fmt.Errorf("sample has %d fields, want 2", len(res.Value))
	}
	var ts float64
	if err := json.Unmarshal(res.Value[0], &ts); err != nil {
		return fmt.Errorf("decode sample timestamp: %w", err)
	}
	if int64(ts) != anchor.Unix() {
		return fmt.Errorf("sample timestamp = %d, want %d", int64(ts), anchor.Unix())
	}
	var raw string
	if err := json.Unmarshal(res.Value[1], &raw); err != nil {
		return fmt.Errorf("decode sample value: %w", err)
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fmt.Errorf("parse sample value %q: %w", raw, err)
	}
	if value != probeMetricValue {
		return fmt.Errorf("sample value = %v, want %v", value, float64(probeMetricValue))
	}
	return nil
}

// assertLokiProbe requires the reference to hold exactly one stream with
// exactly the pushed labels, carrying exactly the pushed line at the pushed
// nanosecond.
func assertLokiProbe(ctx context.Context, query string, anchor time.Time, want map[string]string) error {
	var got struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := fetchJSON(ctx, query, &got); err != nil {
		return err
	}
	if got.Status != "success" {
		return fmt.Errorf("query status = %q, want %q", got.Status, "success")
	}
	if len(got.Data.Result) != 1 {
		return fmt.Errorf("got %d streams, want exactly 1", len(got.Data.Result))
	}
	res := got.Data.Result[0]
	if len(res.Stream) != len(want) {
		return fmt.Errorf("stream labels %v, want %v", res.Stream, want)
	}
	for k, v := range want {
		if res.Stream[k] != v {
			return fmt.Errorf("stream label %s = %q, want %q", k, res.Stream[k], v)
		}
	}
	if len(res.Values) != 1 {
		return fmt.Errorf("got %d entries, want exactly 1", len(res.Values))
	}
	entry := res.Values[0]
	if len(entry) < 2 {
		return fmt.Errorf("entry has %d fields, want at least 2", len(entry))
	}
	ns, err := strconv.ParseInt(entry[0], 10, 64)
	if err != nil {
		return fmt.Errorf("parse entry timestamp %q: %w", entry[0], err)
	}
	if ns != anchor.UnixNano() {
		return fmt.Errorf("entry timestamp = %d, want %d", ns, anchor.UnixNano())
	}
	if entry[1] != probeLogLine {
		return fmt.Errorf("entry line = %q, want %q", entry[1], probeLogLine)
	}
	return nil
}

// assertTempoProbe requires the reference to return the pushed trace with
// exactly one span under the pushed name.
func assertTempoProbe(ctx context.Context, traceURL string) error {
	var got struct {
		Batches []struct {
			ScopeSpans []struct {
				Spans []struct {
					Name string `json:"name"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"batches"`
	}
	if err := fetchJSON(ctx, traceURL, &got); err != nil {
		return err
	}
	var names []string
	for _, b := range got.Batches {
		for _, ss := range b.ScopeSpans {
			for _, s := range ss.Spans {
				names = append(names, s.Name)
			}
		}
	}
	if len(names) != 1 {
		return fmt.Errorf("got %d spans %v, want exactly 1", len(names), names)
	}
	if names[0] != probeSpanName {
		return fmt.Errorf("span name = %q, want %q", names[0], probeSpanName)
	}
	return nil
}

// fetchJSON GETs url, requiring a 200 with a JSON body, and decodes it into
// out. A non-200 carries the backend's own body into the returned error.
func fetchJSON(ctx context.Context, url string, out any) error {
	body, err := fetch(ctx, url)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s body %q: %w", url, truncateBody(body), err)
	}
	return nil
}

// fetch GETs url with an Accept: application/json header (Tempo negotiates
// protobuf without it) and requires a 200.
func fetch(ctx context.Context, url string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, responseBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read %s body: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, truncateBody(body))
	}
	return body, nil
}

// truncateBody caps a failing response body quoted into an error message.
func truncateBody(body []byte) string {
	if len(body) <= errBodyLimit {
		return string(body)
	}
	return string(body[:errBodyLimit])
}

// getJSON is fetchJSON's fatal-on-error form for the single-shot assertions
// that have no retry semantics.
func getJSON(t *testing.T, ctx context.Context, url string, out any) {
	t.Helper()
	if err := fetchJSON(ctx, url, out); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
}

// getBody is fetch's fatal-on-error form.
func getBody(t *testing.T, ctx context.Context, url string) []byte {
	t.Helper()
	body, err := fetch(ctx, url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return body
}
