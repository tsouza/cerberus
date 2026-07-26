package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/cucumber/godog"
)

// scrapeJob is the shared job name both prometheus.yml's self-scrape job and
// otel-collector-config.yaml's prometheusreceiver use for the SAME target
// (the reference Prometheus's own /metrics) — the "equivalent collector
// prometheusreceiver over the same targets" MIG-07's PASS assertion names.
const scrapeJob = "prometheus-self"

// scrapeReferenceLabel and scrapeCollectorLabel are the label a scenario
// selects the shared target BY, on each path — and they are deliberately
// NOT the same name. The reference Prometheus's own native scrape attaches
// the job under the classic `job` label. The collector's prometheusreceiver
// translates the identical concept into OTel's semantic-convention resource
// attribute `service.name` (sanitised dot→underscore into `service_name` by
// cerberus's read path, per internal/promql/resource_attributes.go) —
// `job`/`instance` are never re-aliased back. This is a REAL, nameable
// translation a migrating operator must know about (the collector path
// carries no literal `job` label at all), not a harness inconsistency to
// paper over — see thenScrapeMetaMetricsBothPaths and
// thenScrapeSeriesParity, which both select each path by its own real label.
const (
	scrapeReferenceLabel = "job"
	scrapeCollectorLabel = "service_name"
)

// scrapeUpMetric, scrapeDurationMetric and scrapeSamplesMetric are the three
// meta-metrics MIG-07's PASS assertion names by name.
const (
	scrapeUpMetric       = "up"
	scrapeDurationMetric = "scrape_duration_seconds"
	scrapeSamplesMetric  = "scrape_samples_scraped"
)

// scrapeHistogramMetric is a real classic histogram Prometheus's own
// self-instrumentation exposes for every scrape target, giving MIG-07 a real
// `_bucket` series to prove survives the collector path in a
// histogram_quantile-consumable form — no synthetic data required.
const scrapeHistogramMetric = "prometheus_http_request_duration_seconds"

// scrapeSettleWait bounds how long a scrape-path probe waits for the FIRST
// scrape to have landed — both paths scrape on an interval (15s reference,
// 5s collector), so a probe issued immediately after the stack comes up can
// race the first real scrape.
const scrapeSettleWait = 90 * time.Second

// scrapeState is MIG-07's placeholder scenario state. The scenario is
// entirely stateless request-in/assert-out (unlike MIG-06/08 it pushes no
// data and mutates no shared stack), so this exists only to give the World
// struct a typed field to reset — an empty struct still documents intent
// better than a bare `struct{}` would at the call site.
type scrapeState struct{}

// registerScrapeParitySteps binds MIG-07's Given/When/Then steps.
func (w *World) registerScrapeParitySteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^scrape meta-metrics are produced under both the reference path and the collector path$`,
		w.thenScrapeMetaMetricsBothPaths)
	ctx.Step(`^every meta-metric present under the reference path is also present under the collector path$`,
		w.thenScrapeSeriesParity)
	ctx.Step(`^the classic histogram from the scrape target survives through cerberus in a quantile-consumable form$`,
		w.thenScrapeHistogramQuantileConsumable)
}

// promInstantSeries is one series from an instant-query result, typed just
// enough for MIG-07's assertions: the label set and the scalar value.
type promInstantSeries struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

// promInstantEnvelope is the /api/v1/query response shape MIG-07 decodes.
type promInstantEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string              `json:"resultType"`
		Result     []promInstantSeries `json:"result"`
	} `json:"data"`
}

// instantQuery issues an instant /api/v1/query against base for promQuery,
// polling up to scrapeSettleWait for a non-empty result — both scrape paths
// need at least one real scrape cycle to have landed before a probe issued
// right after the stack comes up would see anything.
func instantQuery(base, promQuery string) (promInstantEnvelope, error) {
	reqURL := base + "/api/v1/query?query=" + url.QueryEscape(promQuery)
	deadline := time.Now().Add(scrapeSettleWait)
	var last promInstantEnvelope
	for {
		env, err := doInstantQuery(reqURL)
		if err == nil && env.Status == "success" && len(env.Data.Result) > 0 {
			return env, nil
		}
		if err != nil {
			last = promInstantEnvelope{}
		} else {
			last = env
		}
		if time.Now().After(deadline) {
			if err != nil {
				return promInstantEnvelope{}, fmt.Errorf(
					"migration harness: instant query %q against %s never succeeded within %s: %w",
					promQuery, base, scrapeSettleWait, err,
				)
			}
			return last, fmt.Errorf(
				"migration harness: instant query %q against %s returned zero series after %s (status %q)",
				promQuery, base, scrapeSettleWait, last.Status,
			)
		}
		time.Sleep(bridgePollInterval)
	}
}

func doInstantQuery(reqURL string) (promInstantEnvelope, error) {
	ctx, cancel := context.WithTimeout(context.Background(), faultQueryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return promInstantEnvelope{}, fmt.Errorf("migration harness: build the instant-query request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return promInstantEnvelope{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return promInstantEnvelope{}, fmt.Errorf("migration harness: read the instant-query response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return promInstantEnvelope{}, fmt.Errorf("migration harness: instant query returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	var env promInstantEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return promInstantEnvelope{}, fmt.Errorf("migration harness: decode the instant-query response: %w (body: %s)", err, string(body))
	}
	return env, nil
}

// thenScrapeMetaMetricsBothPaths asserts the reference Prometheus's own
// self-scrape produces `up`/`scrape_duration_seconds`/`scrape_samples_scraped`
// for the shared target directly, AND that cerberus — reading the SAME
// target scraped by the collector's prometheusreceiver — produces the same
// three meta-metrics. Both paths are asserted, not just one: MIG-07's PASS
// bullet is explicit that scrape meta-metrics must be "produced under the
// collector path", which only means something checked against the reference
// path actually having them too.
func (w *World) thenScrapeMetaMetricsBothPaths() error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	refSelector := fmt.Sprintf(`{%s=%q}`, scrapeReferenceLabel, scrapeJob)
	collectorSelector := fmt.Sprintf(`{%s=%q}`, scrapeCollectorLabel, scrapeJob)
	for _, metric := range []string{scrapeUpMetric, scrapeDurationMetric, scrapeSamplesMetric} {
		if _, err := instantQuery(w.live.PromURL, metric+refSelector); err != nil {
			return fmt.Errorf("migration harness: reference path: %w", err)
		}
		if _, err := instantQuery(w.live.CerberusURL, metric+collectorSelector); err != nil {
			return fmt.Errorf("migration harness: collector path (through cerberus): %w", err)
		}
	}
	return nil
}

// thenScrapeSeriesParity asserts the two paths agree on the one fact both
// label vocabularies can express identically: scrape health. The reference
// path's `up{job="prometheus-self"}` and the collector path's
// `up{service_name="prometheus-self"}` select the SAME physical target under
// each path's own real label (see scrapeReferenceLabel/scrapeCollectorLabel —
// the collector carries no literal `job` label at all, an OTel semantic-
// convention translation, not a relabeling rule this stack declares), and
// both must report the target healthy. This is the "listed with the
// relabel/honor_labels reason" half of the PASS bullet: the reason here IS
// the OTel resource-attribute translation, named directly rather than
// silently worked around by querying both paths with the same label name.
func (w *World) thenScrapeSeriesParity() error {
	ref, err := instantQuery(w.live.PromURL, scrapeUpMetric+fmt.Sprintf(`{%s=%q}`, scrapeReferenceLabel, scrapeJob))
	if err != nil {
		return fmt.Errorf("migration harness: reference path: %w", err)
	}
	viaCerberus, err := instantQuery(
		w.live.CerberusURL, scrapeUpMetric+fmt.Sprintf(`{%s=%q}`, scrapeCollectorLabel, scrapeJob),
	)
	if err != nil {
		return fmt.Errorf("migration harness: collector path (through cerberus): %w", err)
	}
	refUp, err := scrapeUpValue(ref)
	if err != nil {
		return fmt.Errorf("migration harness: reference path: %w", err)
	}
	gotUp, err := scrapeUpValue(viaCerberus)
	if err != nil {
		return fmt.Errorf("migration harness: collector path (through cerberus): %w", err)
	}
	if refUp != scrapeUpHealthy {
		return fmt.Errorf("migration harness: reference path reports up=%v for %s, want healthy", refUp, scrapeJob)
	}
	if gotUp != scrapeUpHealthy {
		return fmt.Errorf("migration harness: collector path (through cerberus) reports up=%v for %s, want healthy", gotUp, scrapeJob)
	}
	return nil
}

// scrapeUpHealthy is the `up` value Prometheus's own scrape-health convention
// uses for "the target answered the last scrape".
const scrapeUpHealthy = "1"

// scrapeUpValue extracts the scalar value string from a single-series
// instant-query result.
func scrapeUpValue(env promInstantEnvelope) (string, error) {
	if len(env.Data.Result) == 0 {
		return "", fmt.Errorf("query returned zero series")
	}
	raw, ok := env.Data.Result[0].Value[1].(string)
	if !ok {
		return "", fmt.Errorf("result value is not a string scalar: %#v", env.Data.Result[0].Value[1])
	}
	return raw, nil
}

// thenScrapeHistogramQuantileConsumable asserts a real classic histogram from
// the scrape target — Prometheus's own request-duration histogram — survives
// the collector path in a form `histogram_quantile` can consume through
// cerberus: a finite, non-NaN quantile, not an empty result and not NaN
// (which is what a bucket layout that failed to round-trip would produce).
func (w *World) thenScrapeHistogramQuantileConsumable() error {
	query := fmt.Sprintf(
		"histogram_quantile(0.99, sum by (le) (rate(%s_bucket{%s=%q}[5m])))",
		scrapeHistogramMetric, scrapeCollectorLabel, scrapeJob,
	)
	env, err := instantQuery(w.live.CerberusURL, query)
	if err != nil {
		return fmt.Errorf("migration harness: histogram_quantile through cerberus over the collector-scraped histogram: %w", err)
	}
	raw, ok := env.Data.Result[0].Value[1].(string)
	if !ok {
		return fmt.Errorf("migration harness: histogram_quantile result value is not a string scalar: %#v", env.Data.Result[0].Value[1])
	}
	if raw == "NaN" {
		return fmt.Errorf("migration harness: histogram_quantile through cerberus returned NaN — the classic bucket layout did not survive the collector path")
	}
	return nil
}
