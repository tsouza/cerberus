package steps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// faultReplicas is how many times the heavy range query is replayed to
// compute p50/p95/p99 — small (this is a Docker Compose harness, not a load
// rig), but real: every replica is a genuine HTTP round trip against the live
// stack, never a stubbed timing.
const faultReplicas = 9

// faultConcurrency bounds how many of the faultReplicas run at once, so the
// replay exercises more than strictly-sequential request handling without
// pretending this small stack models "production QPS" literally.
const faultConcurrency = 3

// faultQueryTimeout bounds a single heavy-query round trip. It is what makes
// "graceful degradation" observable as a clean error rather than a hang: a
// fault-injected backend that never answers must still fail this test within
// a bounded time, not stall the suite.
const faultQueryTimeout = 30 * time.Second

// faultComposeService is the compose service MIG-08 fault-injects — the
// ClickHouse container cerberus reads every sample from, so pausing it is the
// most direct way to force cerberus's own query path to degrade without
// touching the reference backend the rollback assertion depends on staying
// healthy.
const faultComposeService = "clickhouse"

// faultComposeFile is the Tier-1 stack's compose file, relative to the
// repository root.
const faultComposeFile = "test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml"

// faultUnpauseWait bounds how long the recovery probe waits for cerberus to
// answer again after ClickHouse is unpaused.
const faultUnpauseWait = 60 * time.Second

// faultState is MIG-08's one heavy-query replay plus which compose service
// (if any) fault injection currently has paused, so the harness can always
// restore it — including from World's own After hook, if the scenario's own
// restoring Then step never runs.
type faultState struct {
	latencies     []time.Duration
	pausedService string
}

// registerFaultInjectionSteps binds MIG-08's Given/When/Then steps.
func (w *World) registerFaultInjectionSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the operator replays the heaviest query against cerberus repeatedly$`, w.whenReplayHeavyQuery)
	ctx.Step(`^every replay succeeds and cerberus reports latency percentiles$`, w.thenReplayLatencyPercentiles)
	ctx.Step(`^the operator pauses the ClickHouse container$`, w.whenPauseClickHouse)
	ctx.Step(`^cerberus degrades the same heavy query cleanly instead of hanging$`, w.thenHeavyQueryDegradesCleanly)
	ctx.Step(`^the reference Prometheus still answers a rollback query directly$`, w.thenReferenceStillAnswersDirectly)
	ctx.Step(`^the operator resumes the ClickHouse container and cerberus recovers$`, w.thenResumeAndRecover)
}

// heavyRangeQueryURL builds the widest-window, highest-cardinality query this
// harness's seeded fixture supports: a range sum grouped by the archetype's
// declared high-churn label, over the manifest's own published window — the
// closed [VerifyStart, VerifyEnd] interval every live-backend scenario reads
// from, never a live one ending at `now`.
func (w *World) heavyRangeQueryURL(base string, decl seed.Declaration, m seed.Manifest) (string, error) {
	step, err := m.StepDuration()
	if err != nil {
		return "", err
	}
	promQuery := fmt.Sprintf("sum by (%s) (%s)", decl.ChurnLabel, decl.GaugeMetric)
	v := url.Values{}
	v.Set("query", promQuery)
	v.Set("start", strconv.FormatInt(m.VerifyStart.Unix(), 10))
	v.Set("end", strconv.FormatInt(m.VerifyEnd.Unix(), 10))
	v.Set("step", step.String())
	return base + "/api/v1/query_range?" + v.Encode(), nil
}

// doHeavyQuery issues one GET against reqURL with faultQueryTimeout, so a
// degraded backend fails this call within a bound instead of hanging the
// scenario.
func doHeavyQuery(reqURL string) (status int, body []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), faultQueryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("migration harness: build the heavy-query request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// A transport-level error (including a context deadline) is data
		// here, not a harness failure: MIG-08 asserts on it directly.
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return resp.StatusCode, nil, fmt.Errorf("migration harness: read the heavy-query response: %w", readErr)
	}
	return resp.StatusCode, b, nil
}

// whenReplayHeavyQuery replays the heaviest query this fixture supports
// faultReplicas times at faultConcurrency, recording each successful round
// trip's latency. A single failure under normal (non-faulted) conditions
// fails the step outright — this is the baseline the later fault-injected
// replay is contrasted against.
func (w *World) whenReplayHeavyQuery() error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	archetype, err := w.singleArchetype("the operator replays the heaviest query against cerberus repeatedly")
	if err != nil {
		return err
	}
	decl, err := seed.LoadDeclaration(harnessPath(w.root, archetypeDir, archetype, "seed", "fixture.json"))
	if err != nil {
		return fmt.Errorf("migration harness: %w", err)
	}
	manifest, err := w.live.LoadManifest()
	if err != nil {
		return err
	}
	reqURL, err := w.heavyRangeQueryURL(w.live.CerberusURL, decl, manifest)
	if err != nil {
		return err
	}

	var (
		mu       sync.Mutex
		lats     = make([]time.Duration, 0, faultReplicas)
		firstErr error
		sem      = make(chan struct{}, faultConcurrency)
		wg       sync.WaitGroup
	)
	for i := 0; i < faultReplicas; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			status, body, err := doHeavyQuery(reqURL)
			elapsed := time.Since(start)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("migration harness: baseline heavy-query replay failed: %w", err)
				}
				return
			}
			if status != http.StatusOK {
				if firstErr == nil {
					firstErr = fmt.Errorf("migration harness: baseline heavy-query replay returned HTTP %d: %s", status, string(body))
				}
				return
			}
			lats = append(lats, elapsed)
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	w.fault.latencies = lats
	return nil
}

// percentile returns the p-th percentile (0..1) of a COPY of lats, sorted
// ascending; it never mutates the caller's slice.
func percentile(lats []time.Duration, p float64) time.Duration {
	sorted := append([]time.Duration(nil), lats...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// Percentile points MIG-08's report names explicitly, so the assertion below
// reads as the same p50/p95/p99 the PASS bullet documents.
const (
	p50 = 0.50
	p95 = 0.95
	p99 = 0.99
)

// thenReplayLatencyPercentiles asserts the baseline replay actually produced
// one latency per successful replica (never a hollow "it returned 200 once"
// pass) and that p50/p95/p99 are computable and non-negative — the numbers
// MIG-08's PASS bullet requires be captured.
func (w *World) thenReplayLatencyPercentiles() error {
	if len(w.fault.latencies) != faultReplicas {
		return fmt.Errorf("migration harness: recorded %d latenc(y/ies), want %d — a hollow replay proves nothing",
			len(w.fault.latencies), faultReplicas)
	}
	for _, p := range []float64{p50, p95, p99} {
		if v := percentile(w.fault.latencies, p); v < 0 {
			return fmt.Errorf("migration harness: computed a negative latency percentile: %s", v)
		}
	}
	return nil
}

// dockerCompose runs `docker compose -f faultComposeFile <args...>` rooted at
// the repository root, returning combined output on failure so a scenario
// failure names the real docker error rather than just a non-zero exit.
func dockerCompose(root string, args ...string) error {
	full := append([]string{"compose", "-f", faultComposeFile}, args...)
	cmd := exec.Command("docker", full...) //nolint:gosec // harness-authored fixed compose file + fixed subcommand
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("migration harness: docker %v: %w: %s", full, err, string(out))
	}
	return nil
}

// whenPauseClickHouse fault-injects by pausing (not killing) the ClickHouse
// container — reversible, and it leaves the reference Prometheus this
// scenario's rollback assertion depends on completely untouched. It records
// the pause on World so the harness can always restore it, including from
// World's own After hook if this scenario never reaches its own restoring
// Then step.
func (w *World) whenPauseClickHouse() error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	if err := dockerCompose(w.root, "pause", faultComposeService); err != nil {
		return err
	}
	w.fault.pausedService = faultComposeService
	return nil
}

// thenHeavyQueryDegradesCleanly replays the SAME heavy query against
// cerberus while ClickHouse is paused, asserting it fails within
// faultQueryTimeout with either a transport error (connection reset/refused)
// or a non-2xx status — "graceful degradation": the gateway must return an
// answer, even an error one, within a bound, never hang indefinitely.
func (w *World) thenHeavyQueryDegradesCleanly() error {
	if w.fault.pausedService == "" {
		return fmt.Errorf("migration harness: no fault has been injected for this scenario")
	}
	archetype, err := w.singleArchetype("cerberus degrades the same heavy query cleanly instead of hanging")
	if err != nil {
		return err
	}
	decl, err := seed.LoadDeclaration(harnessPath(w.root, archetypeDir, archetype, "seed", "fixture.json"))
	if err != nil {
		return fmt.Errorf("migration harness: %w", err)
	}
	manifest, err := w.live.LoadManifest()
	if err != nil {
		return err
	}
	reqURL, err := w.heavyRangeQueryURL(w.live.CerberusURL, decl, manifest)
	if err != nil {
		return err
	}
	status, body, err := doHeavyQuery(reqURL)
	if err == nil && status == http.StatusOK {
		return fmt.Errorf(
			"migration harness: the heavy query still returned HTTP 200 with ClickHouse paused — no degradation was observed (body: %s)",
			string(body),
		)
	}
	return nil
}

// thenReferenceStillAnswersDirectly asserts the reference Prometheus —
// entirely untouched by the ClickHouse fault — still answers a real query
// for the same fixture data. This is MIG-08's "working datasource-flip
// rollback": while cerberus degrades, the reference backend an operator could
// flip Grafana back to is proven still healthy.
func (w *World) thenReferenceStillAnswersDirectly() error {
	if w.fault.pausedService == "" {
		return fmt.Errorf("migration harness: no fault has been injected for this scenario")
	}
	archetype, err := w.singleArchetype("the reference Prometheus still answers a rollback query directly")
	if err != nil {
		return err
	}
	decl, err := seed.LoadDeclaration(harnessPath(w.root, archetypeDir, archetype, "seed", "fixture.json"))
	if err != nil {
		return fmt.Errorf("migration harness: %w", err)
	}
	manifest, err := w.live.LoadManifest()
	if err != nil {
		return err
	}
	reqURL, err := w.heavyRangeQueryURL(w.live.PromURL, decl, manifest)
	if err != nil {
		return err
	}
	status, body, err := doHeavyQuery(reqURL)
	if err != nil {
		return fmt.Errorf("migration harness: the reference Prometheus rollback query failed while cerberus was faulted: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("migration harness: the reference Prometheus rollback query returned HTTP %d: %s", status, string(body))
	}
	return nil
}

// thenResumeAndRecover unpauses ClickHouse and polls the SAME heavy query
// against cerberus until it succeeds again, proving the degradation was
// transient rather than a stack the scenario is about to leave broken for
// whatever runs next.
func (w *World) thenResumeAndRecover() error {
	if err := w.restorePausedService(); err != nil {
		return err
	}
	archetype, err := w.singleArchetype("the operator resumes the ClickHouse container and cerberus recovers")
	if err != nil {
		return err
	}
	decl, err := seed.LoadDeclaration(harnessPath(w.root, archetypeDir, archetype, "seed", "fixture.json"))
	if err != nil {
		return fmt.Errorf("migration harness: %w", err)
	}
	manifest, err := w.live.LoadManifest()
	if err != nil {
		return err
	}
	reqURL, err := w.heavyRangeQueryURL(w.live.CerberusURL, decl, manifest)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(faultUnpauseWait)
	var lastErr error
	for {
		status, body, err := doHeavyQuery(reqURL)
		if err == nil && status == http.StatusOK {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("HTTP %d: %s", status, string(body))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("migration harness: cerberus never recovered within %s after ClickHouse resumed: %v",
				faultUnpauseWait, lastErr)
		}
		time.Sleep(bridgePollInterval)
	}
}

// restorePausedService unpauses whatever compose service fault injection
// currently has paused, if any. It is idempotent — called both by the
// scenario's own recovery Then step and, as a safety net, by World's After
// hook — so a scenario that never reaches its own recovery step (an earlier
// assertion failed first) still never leaves the shared Tier-1 stack
// degraded for whatever scenario runs next.
func (w *World) restorePausedService() error {
	if w.fault.pausedService == "" {
		return nil
	}
	service := w.fault.pausedService
	w.fault.pausedService = ""
	if err := dockerCompose(w.root, "unpause", service); err != nil {
		return fmt.Errorf("migration harness: restore the fault-injected %s service: %w", service, err)
	}
	return nil
}
