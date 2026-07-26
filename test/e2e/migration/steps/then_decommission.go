package steps

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cucumber/godog"
	"github.com/prometheus/common/model"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// residualReaderQueries is how many times the planted residual reader
// queries the incumbent. It lives in Go, not in feature text, per the "no
// number in step text" discipline: the relation the Then asserts — the audit
// refuses on a non-zero delta — is what the scenario names; this is the data
// the reader actually generates.
const residualReaderQueries = 3

// decommissionStageOrder pins the teardown order docs/migration-testing.md
// section 6's DECOMMISSION row documents for MIG-25: the old read path first,
// then the ruler/Alertmanager, then the ingest/write leg last. It is declared
// once, as data, so a future edit that silently reorders it is a diff a
// reviewer sees rather than a runbook fact that only ever lived in prose.
var decommissionStageOrder = []string{"read-path", "ruler-alertmanager", "ingest-write"}

// queryHandlerRequestsMetric is the reference Prometheus's own self-exposed
// counter for requests its query handler has served — the signal a
// decommission audit reads to detect a reader still depending on the
// incumbent's read path.
const queryHandlerRequestsMetric = `prometheus_http_requests_total{handler="/api/v1/query"`

// scrapeMetricsTimeout bounds one /metrics scrape, and scrapeMetricsBodyLimit
// caps how much of it is parsed.
const (
	scrapeMetricsTimeout   = 10 * time.Second
	scrapeMetricsBodyLimit = 8 << 20
)

// complianceRetentionFixture is the committed per-archetype declaration of
// the compliance-retention mandate MIG-26's tier-1 half compares the live
// ClickHouse retention against.
const complianceRetentionFixture = "compliance-retention.json"

// decommissionRun is one archetype's MIG-25 run: the baseline read-traffic
// counter, the residual delta a planted reader produced, and the audit's own
// refuse/allow verdict.
type decommissionRun struct {
	metric        string
	baseline      float64
	residualDelta float64
	refused       bool
}

// retentionGateRun is one archetype's MIG-26 tier-1 run: the declared
// compliance-retention mandate and what the live ClickHouse retention proves
// against it.
type retentionGateRun struct {
	mandate       time.Duration
	liveRetention time.Duration
	liveProven    bool
	meetsMandate  bool
}

// complianceRetention is the committed fixture shape: the compliance window
// an operator must prove ClickHouse's retention covers, per signal.
type complianceRetention struct {
	Metrics model.Duration `json:"metrics"`
}

// registerDecommissionSteps binds the MIG-25 (residual-read-traffic audit)
// and MIG-26 tier-1 (live-retention-vs-compliance-mandate) steps.
func (w *World) registerDecommissionSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the incumbent's read-traffic baseline captured before any residual reader runs$`, w.givenReadTrafficBaseline)
	ctx.Step(`^the incumbent's read traffic since the baseline is exactly zero, before any residual reader has run$`, w.thenBaselineDeltaIsZero)
	ctx.Step(`^a residual reader keeps querying the incumbent after cutover would have begun$`, w.whenResidualReaderRuns)
	ctx.Step(`^the operator audits incumbent read traffic for decommission authorization$`, w.whenAuditReadTraffic)
	ctx.Step(`^the audit refuses authorization while the incumbent still serves the residual reader's traffic$`, w.thenAuditRefuses)
	ctx.Step(`^the audit's authorization artifact names the exact residual request count it refused on$`, w.thenAuditNamesResidualCount)
	ctx.Step(`^the staged decommission order still gates the read path before the ruler and the ingest leg$`, w.thenStagedOrderPinned)

	ctx.Step(`^the declared compliance-retention mandate for each tagged archetype$`, w.givenComplianceMandate)
	ctx.Step(`^the operator reads the retention ClickHouse actually provisions, live$`, w.whenReadLiveComplianceRetention)
	ctx.Step(`^the live retention does not yet prove the compliance-retention mandate is met$`, w.thenLiveRetentionDoesNotProveMandate)
}

// --- MIG-25: decommission is blocked while the incumbent still serves reads

// scrapeCounterSum fetches baseURL's /metrics exposition and sums every
// sample whose rendered key matches selector — a bare metric name for an
// unlabelled sample, or "name{partial-label" for a family whose rendered
// label set contains the fragment. This mirrors the matching convention
// test/e2e/migration/seed/settle.go's own settle gates use; it is
// reimplemented here, rather than imported, because it is a private helper
// in that package and this audit needs exactly one counter, not the settle
// machinery built around it.
func scrapeCounterSum(ctx context.Context, baseURL, selector string) (float64, error) {
	reqCtx, cancel := context.WithTimeout(ctx, scrapeMetricsTimeout)
	defer cancel()
	target := strings.TrimRight(baseURL, "/") + "/metrics"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return 0, fmt.Errorf("migration harness: build metrics scrape request for %s: %w", target, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("migration harness: scrape %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("migration harness: %s returned %d", target, resp.StatusCode)
	}
	var total float64
	var matched bool
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, scrapeMetricsBodyLimit))
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), bufio.MaxScanTokenSize)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rawValue, ok := strings.Cut(line, " ")
		if !ok || !matchesSelector(name, selector) {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(rawValue), 64)
		if err != nil {
			continue
		}
		total += value
		matched = true
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("migration harness: read %s: %w", target, err)
	}
	if !matched {
		return 0, fmt.Errorf("migration harness: %s exposes no metric matching %q", target, selector)
	}
	return total, nil
}

// matchesSelector reports whether an exposition key matches selector, which
// is either a bare metric name or "name{partial-label-fragment".
func matchesSelector(key, selector string) bool {
	if key == selector {
		return true
	}
	brace := strings.IndexRune(selector, '{')
	if brace < 0 {
		return strings.HasPrefix(key, selector+"{")
	}
	name := selector[:brace]
	labels := selector[brace+1:]
	if !strings.HasPrefix(key, name+"{") {
		return false
	}
	end := strings.IndexRune(key, '}')
	if end < 0 {
		return false
	}
	return strings.Contains(key[len(name)+1:end], labels)
}

func (w *World) givenReadTrafficBaseline() error {
	if err := w.requireArchetypes("the read-traffic baseline"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		count, err := scrapeCounterSum(context.Background(), w.live.PromURL, queryHandlerRequestsMetric)
		if err != nil {
			return fmt.Errorf("archetype %s: capture the read-traffic baseline: %w", a, err)
		}
		w.decommission[a] = decommissionRun{baseline: count}
	}
	return nil
}

// thenBaselineDeltaIsZero proves the audit is not a rubber stamp that always
// refuses: read immediately after the baseline, before any residual reader
// has run, the delta must read exactly zero.
func (w *World) thenBaselineDeltaIsZero() error {
	return w.eachDecommission(func(a string, dr decommissionRun) (decommissionRun, error) {
		count, err := scrapeCounterSum(context.Background(), w.live.PromURL, queryHandlerRequestsMetric)
		if err != nil {
			return dr, fmt.Errorf("archetype %s: re-read read traffic before the residual reader runs: %w", a, err)
		}
		if delta := count - dr.baseline; delta != 0 {
			return dr, fmt.Errorf("archetype %s: read traffic already advanced by %v since the baseline, before any residual reader ran", a, delta)
		}
		return dr, nil
	})
}

// whenResidualReaderRuns plants the reader the story's fixture calls for: a
// client that keeps querying the incumbent after cutover would have begun.
func (w *World) whenResidualReaderRuns() error {
	return w.eachDecommission(func(a string, dr decommissionRun) (decommissionRun, error) {
		m, err := w.live.LoadManifest(a)
		if err != nil {
			return dr, err
		}
		if m.GaugeMetric == "" {
			return dr, fmt.Errorf("archetype %s: the seeder's manifest names no gauge metric for the residual reader to query", a)
		}
		dr.metric = m.GaugeMetric
		for range residualReaderQueries {
			if _, err := lib.QueryInstant(context.Background(), w.live.PromURL, dr.metric, m.VerifyEnd); err != nil {
				return dr, fmt.Errorf("archetype %s: the residual reader's query failed: %w", a, err)
			}
		}
		return dr, nil
	})
}

// whenAuditReadTraffic is the decommission-authorization audit itself: it
// re-reads the same counter and refuses whenever the delta since the
// baseline is non-zero.
func (w *World) whenAuditReadTraffic() error {
	return w.eachDecommission(func(a string, dr decommissionRun) (decommissionRun, error) {
		count, err := scrapeCounterSum(context.Background(), w.live.PromURL, queryHandlerRequestsMetric)
		if err != nil {
			return dr, fmt.Errorf("archetype %s: audit incumbent read traffic: %w", a, err)
		}
		dr.residualDelta = count - dr.baseline
		dr.refused = dr.residualDelta > 0
		return dr, nil
	})
}

func (w *World) thenAuditRefuses() error {
	return w.eachDecommission(func(a string, dr decommissionRun) (decommissionRun, error) {
		if !dr.refused {
			return dr, fmt.Errorf("archetype %s: the audit authorized teardown while the residual reader's traffic delta was %v", a, dr.residualDelta)
		}
		return dr, nil
	})
}

// thenAuditNamesResidualCount is the "authorization artifact" half of the
// PASS bullet: the audit's refusal must name the EXACT residual count it
// measured, matching the count the planted reader actually generated, not
// merely "some" non-zero number.
func (w *World) thenAuditNamesResidualCount() error {
	return w.eachDecommission(func(a string, dr decommissionRun) (decommissionRun, error) {
		if dr.residualDelta != float64(residualReaderQueries) {
			return dr, fmt.Errorf("archetype %s: the audit's refused-on count is %v, not the %d request(s) the planted reader actually issued",
				a, dr.residualDelta, residualReaderQueries)
		}
		return dr, nil
	})
}

func (w *World) thenStagedOrderPinned() error {
	if len(decommissionStageOrder) != 3 {
		return fmt.Errorf("the staged decommission order carries %d stage(s), want exactly the three docs/migration-testing.md documents",
			len(decommissionStageOrder))
	}
	if got, want := decommissionStageOrder[0], "read-path"; got != want {
		return fmt.Errorf("the staged decommission order starts with %q, want %q first", got, want)
	}
	if got, want := decommissionStageOrder[1], "ruler-alertmanager"; got != want {
		return fmt.Errorf("the staged decommission order's second stage is %q, want %q", got, want)
	}
	if got, want := decommissionStageOrder[2], "ingest-write"; got != want {
		return fmt.Errorf("the staged decommission order's third stage is %q, want %q last", got, want)
	}
	return nil
}

// eachDecommission runs fn over every tagged archetype's decommission-audit
// state, writing back whatever fn returns.
func (w *World) eachDecommission(fn func(archetype string, dr decommissionRun) (decommissionRun, error)) error {
	if err := w.requireArchetypes("the decommission audit"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		dr, ok := w.decommission[a]
		if !ok {
			return fmt.Errorf("archetype %s: no read-traffic baseline established; the scenario must establish one first", a)
		}
		next, err := fn(a, dr)
		if err != nil {
			return err
		}
		w.decommission[a] = next
	}
	return nil
}

// --- MIG-26 tier-1: live retention against the compliance mandate ----------

func loadComplianceMandate(path string) (time.Duration, error) {
	data, err := lib.ReadArtifact(path)
	if err != nil {
		return 0, err
	}
	var cr complianceRetention
	if err := lib.DecodeArtifact(data, &cr); err != nil {
		return 0, err
	}
	return time.Duration(cr.Metrics), nil
}

func (w *World) givenComplianceMandate() error {
	if err := w.requireArchetypes("the compliance-retention mandate"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		mandate, err := loadComplianceMandate(w.goldenPath(a, complianceRetentionFixture))
		if err != nil {
			return fmt.Errorf("archetype %s: %w", a, err)
		}
		if mandate <= 0 {
			return fmt.Errorf("archetype %s: the declared compliance-retention mandate is %s, which bounds nothing", a, mandate)
		}
		w.retentionGate[a] = retentionGateRun{mandate: mandate}
	}
	return nil
}

// whenReadLiveComplianceRetention reads the live ClickHouse retention
// (never the schema renderer's own text) and compares it against the
// declared mandate. A signal whose tables carry no TTL clause at all fails
// closed, exactly as MIG-14's thenRetentionCoversLookback already does:
// absence never reads as "unbounded".
func (w *World) whenReadLiveComplianceRetention() error {
	return w.eachRetentionGate(func(a string, rg retentionGateRun) (retentionGateRun, error) {
		retention, err := w.readLiveRetention(context.Background())
		if err != nil {
			return rg, fmt.Errorf("archetype %s: %w", a, err)
		}
		have, ok := retention[signalMetrics]
		rg.liveProven = ok
		rg.liveRetention = have
		rg.meetsMandate = ok && have >= rg.mandate
		return rg, nil
	})
}

func (w *World) thenLiveRetentionDoesNotProveMandate() error {
	return w.eachRetentionGate(func(a string, rg retentionGateRun) (retentionGateRun, error) {
		if rg.meetsMandate {
			return rg, fmt.Errorf("archetype %s: the live retention now proves the compliance-retention mandate is met (%s >= %s); this scenario asserts the gate still refuses",
				a, rg.liveRetention, rg.mandate)
		}
		return rg, nil
	})
}

// eachRetentionGate runs fn over every tagged archetype's live-retention
// comparison state, writing back whatever fn returns.
func (w *World) eachRetentionGate(fn func(archetype string, rg retentionGateRun) (retentionGateRun, error)) error {
	if err := w.requireArchetypes("the live retention gate"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		rg, ok := w.retentionGate[a]
		if !ok {
			return fmt.Errorf("archetype %s: no compliance-retention mandate established; the scenario must establish one first", a)
		}
		next, err := fn(a, rg)
		if err != nil {
			return err
		}
		w.retentionGate[a] = next
	}
	return nil
}
