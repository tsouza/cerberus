package steps

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/tolerances"
	"github.com/tsouza/cerberus/test/e2e/migration/tolerant"
)

// downsampleProbe is MIG-20's state: the declared band this run compares
// against, and the comparator's verdict once the When step has run it.
type downsampleProbe struct {
	established bool
	band        tolerances.Band
	bucket      time.Duration
	result      tolerant.Result
	resultSet   bool
}

// registerDownsampleSteps binds MIG-20: the dedicated tolerant comparator
// docs/migration-testing.md section 5 mode 3 describes, deliberately outside
// `cerberus migrate verify`'s zero-diverge gate.
func (w *World) registerDownsampleSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the declared downsample tolerance for the long-range comparison$`, w.givenDownsampleBand)
	ctx.Step(`^the operator compares cerberus's counter growth against a downsampled reconstruction of the reference's raw samples$`,
		w.whenCompareDownsampled)
	ctx.Step(`^the observed delta stays within the declared downsample band$`, w.thenWithinDownsampleBand)
	ctx.Step(`^the comparison measured real counter growth on both the raw and the downsampled side$`,
		w.thenDownsampleMeasuredRealGrowth)
}

// givenDownsampleBand binds the pre-declared band and bucket width from the
// tolerances registry — never a number invented in this step or in the
// feature file.
func (w *World) givenDownsampleBand() error {
	if !w.liveSet {
		return fmt.Errorf("the tier-1 stack has not been established; scenario must establish it first")
	}
	w.downsample = downsampleProbe{
		established: true,
		band:        tolerances.MIG20Downsample,
		bucket:      tolerances.MIG20DownsampleBucket,
	}
	return nil
}

// downsampleQueryExpr sums the fixture's counter metric across every series
// so both backends answer with exactly one combined series — the comparator
// judges total counter growth, not any one label combination's.
func downsampleQueryExpr(counterMetric string) string {
	return "sum(" + counterMetric + ")"
}

// whenCompareDownsampled fetches cerberus's own raw-path answer and the
// reference's raw samples over the same fixture window, reconstructs what a
// downsampled aggregate of the reference would show, and runs the declared
// comparator between the two.
func (w *World) whenCompareDownsampled() error {
	if !w.downsample.established {
		return fmt.Errorf("no downsample band has been established; the scenario must establish one first")
	}
	archetype, err := w.singleArchetype("the downsample comparison")
	if err != nil {
		return err
	}
	manifest, err := w.live.LoadManifest(archetype)
	if err != nil {
		return err
	}
	step, err := manifest.StepDuration()
	if err != nil {
		return err
	}
	expr := downsampleQueryExpr(manifest.CounterMetric)

	cerberusSamples, err := queryPromRangeSamples(w.live.CerberusURL, expr, manifest.VerifyStart, manifest.VerifyEnd, step)
	if err != nil {
		return fmt.Errorf("migration harness: query cerberus's raw-path answer: %w", err)
	}
	referenceSamples, err := queryPromRangeSamples(w.live.PromURL, expr, manifest.VerifyStart, manifest.VerifyEnd, step)
	if err != nil {
		return fmt.Errorf("migration harness: query the reference's raw samples: %w", err)
	}

	rawIncrease, ok := tolerant.Increase(cerberusSamples)
	if !ok {
		return fmt.Errorf("cerberus's raw-path answer carries fewer than two points; nothing to compare")
	}
	downsampled, err := tolerant.DownsampleCounterAware(referenceSamples, manifest.VerifyStart, manifest.VerifyEnd, w.downsample.bucket)
	if err != nil {
		return fmt.Errorf("migration harness: reconstruct the downsampled aggregate: %w", err)
	}
	res, err := tolerant.Compare(rawIncrease, downsampled, w.downsample.band)
	if err != nil {
		return fmt.Errorf("migration harness: %w", err)
	}
	w.downsample.result = res
	w.downsample.resultSet = true
	return nil
}

// promRangeMatrix is the standard Prometheus range-query envelope: one
// combined series expected (see downsampleQueryExpr), but every returned
// series' points are merged in case the aggregation still produced more than
// one — merging rather than picking the first is what keeps this reading
// what the backend actually answered instead of silently discarding data.
type promRangeMatrix struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Values [][2]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// queryPromRangeSamples issues one query_range against base and decodes every
// returned point into tolerant.Sample.
func queryPromRangeSamples(base, expr string, start, end time.Time, step time.Duration) ([]tolerant.Sample, error) {
	q := base + "/api/v1/query_range?query=" + url.QueryEscape(expr) +
		"&start=" + strconv.FormatInt(start.Unix(), 10) +
		"&end=" + strconv.FormatInt(end.Unix(), 10) +
		"&step=" + strconv.FormatFloat(step.Seconds(), 'f', -1, 64)
	body, err := correlationGET(q)
	if err != nil {
		return nil, err
	}
	var resp promRangeMatrix
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode query_range response: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("query_range status %q, want success", resp.Status)
	}
	var out []tolerant.Sample
	for _, series := range resp.Data.Result {
		for _, pair := range series.Values {
			var ts float64
			if err := json.Unmarshal(pair[0], &ts); err != nil {
				return nil, fmt.Errorf("decode sample timestamp: %w", err)
			}
			var valStr string
			if err := json.Unmarshal(pair[1], &valStr); err != nil {
				return nil, fmt.Errorf("decode sample value: %w", err)
			}
			v, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				return nil, fmt.Errorf("parse sample value %q: %w", valStr, err)
			}
			out = append(out, tolerant.Sample{T: time.Unix(int64(ts), 0).UTC(), V: v})
		}
	}
	return out, nil
}

// thenWithinDownsampleBand asserts the comparator's verdict.
func (w *World) thenWithinDownsampleBand() error {
	if err := w.requireDownsampleCompared(); err != nil {
		return err
	}
	if !w.downsample.result.WithinBand {
		return fmt.Errorf("the downsample comparison exceeded the declared band: %s", w.downsample.result.Detail)
	}
	return nil
}

// thenDownsampleMeasuredRealGrowth asserts the run compared something real: a
// band satisfied by two series that both measured zero growth would be a
// vacuous pass — see docs/migration-testing.md section 5, "a zero diverge
// count is necessary, not sufficient", the same principle applied to this
// mode's own comparator.
func (w *World) thenDownsampleMeasuredRealGrowth() error {
	if err := w.requireDownsampleCompared(); err != nil {
		return err
	}
	if w.downsample.result.RawIncrease <= 0 {
		return fmt.Errorf("cerberus's raw-path answer measured no counter growth: %s", w.downsample.result.Detail)
	}
	if w.downsample.result.DownsampledIncrease <= 0 {
		return fmt.Errorf("the downsampled reconstruction measured no counter growth: %s", w.downsample.result.Detail)
	}
	return nil
}

// requireDownsampleCompared fails when the When step never ran.
func (w *World) requireDownsampleCompared() error {
	if !w.downsample.established {
		return fmt.Errorf("no downsample band has been established; the scenario must establish one first")
	}
	if !w.downsample.resultSet {
		return fmt.Errorf("the downsample comparison has not been run; the scenario must run it first")
	}
	return nil
}
