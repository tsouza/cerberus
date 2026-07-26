package steps

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// boundaryPreMargin is how far before ClickHouse's own ingest-start MIG-23
// probes: one full sample step. The fixture's geometry makes exactly that
// instant the place the two backends provably disagree — ClickHouse's first
// row lands AT SeedStart, while reference Prometheus carries an
// incumbent-only sample on every step of seed.PreIngestWindow below it, the
// last of which sits one SampleStep before the boundary.
const boundaryPreMargin = seed.SampleStep

// cutoverProbe is one archetype's MIG-22 run: the probe query and the instant
// it is asked at, plus every answer a retarget-and-roll-back collects.
//
//   - baseline   — the incumbent, before anything moves.
//   - retargeted — cerberus, same request, cerberus's base URL.
//   - alongside  — the incumbent again, WHILE cerberus is serving the read:
//     the "additional datasource, not a replacement" claim measured rather
//     than assumed.
//   - rolledBack — the incumbent after the rollback.
//   - afterRollback — cerberus after the rollback, so a rollback that took
//     cerberus down with it is a failure and not an unobserved side effect.
type cutoverProbe struct {
	query         string
	at            time.Time
	baseline      lib.ProbeResult
	retargeted    lib.ProbeResult
	alongside     lib.ProbeResult
	rolledBack    lib.ProbeResult
	afterRollback lib.ProbeResult
}

// boundaryProbe is one archetype's MIG-23 run: the ingest-start boundary in
// both its declared and its measured form, the instants probed either side of
// it, the oracles every answer is held to, and where the live retention
// horizon sits relative to the boundary.
type boundaryProbe struct {
	metric string

	// declaredStart is the boundary the seeder published (the manifest's
	// SeedStart); measuredStart is what the live ClickHouse itself reports as
	// the earliest instant it holds for this metric. Requiring the two to
	// agree is what makes the split point OBSERVED off the substrate rather
	// than assumed from the fixture's own paperwork.
	declaredStart time.Time
	measuredStart time.Time

	// preInstant sits one sample step below the boundary — inside the span
	// only the incumbent holds. postInstant sits inside the span ClickHouse
	// holds, so the same probe metric is asked on both sides of the split.
	preInstant  time.Time
	postInstant time.Time

	// The oracles, all read off the seeder's manifest rather than off the
	// other side's answer: how many rows ClickHouse must hold at or after the
	// boundary, how many series cerberus must answer with above it, and how
	// many the incumbent must answer with below it.
	wantRowsAtOrAfter uint64
	wantSeries        int
	wantIncumbentPre  int

	rowsBefore    uint64
	rowsAtOrAfter uint64

	cerberusPost lib.ProbeResult
	cerberusPre  lib.ProbeResult
	incumbentPre lib.ProbeResult

	// retentionHorizon is the oldest instant the live ClickHouse still keeps:
	// now minus the TTL its own tables carry.
	liveRetention    time.Duration
	retentionHorizon time.Time
}

// registerCutoverSteps binds the MIG-22 (flip-and-revert) and MIG-23
// (ingest-start boundary) steps.
func (w *World) registerCutoverSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the seeded fixture's own probe metric for each tagged archetype$`, w.givenProbeMetric)
	ctx.Step(`^the operator queries the probe metric against the incumbent before anything is retargeted$`, w.whenProbeIncumbentBaseline)
	ctx.Step(`^the operator retargets the identical probe request at cerberus, changing nothing but the base URL$`, w.whenRetargetProbeAtCerberus)
	ctx.Step(`^cerberus answers the retargeted probe with series of its own, agreeing with the incumbent's own earlier answer$`,
		w.thenRetargetedAgreesWithBaseline)
	ctx.Step(`^the incumbent still answers the same probe itself while cerberus is serving it, because cerberus was stood up alongside it rather than in place of it$`,
		w.thenIncumbentServesAlongsideCerberus)
	ctx.Step(`^the operator retargets the identical probe request back at the incumbent, changing nothing but the base URL$`,
		w.whenRetargetProbeBackAtIncumbent)
	ctx.Step(`^the incumbent's answer after the rollback still agrees with its own earlier answer$`, w.thenRolledBackAgreesWithBaseline)
	ctx.Step(`^cerberus still answers the probe itself after the rollback$`, w.thenCerberusStillAnswersAfterRollback)
	ctx.Step(`^the retarget and the rollback dialled one identical request path at the two different hosts, cerberus's and the incumbent's$`,
		w.thenRetargetDifferedOnlyByHost)

	ctx.Step(`^the seeded fixture's own ingest-start instant for each tagged archetype$`, w.givenIngestStart)
	ctx.Step(`^the operator measures ClickHouse's own earliest held instant and counts its rows either side of the boundary$`,
		w.whenCensusBoundaryRows)
	ctx.Step(`^the boundary ClickHouse itself reports is the boundary the fixture declared, so the split point is observed rather than assumed$`,
		w.thenMeasuredBoundaryMatchesDeclared)
	ctx.Step(`^ClickHouse holds no rows before its own ingest-start$`, w.thenNoRowsBeforeBoundary)
	ctx.Step(`^ClickHouse holds every row the seeder declared at or after ingest-start$`, w.thenRowsAtOrAfterBoundary)
	ctx.Step(`^the operator queries the probe metric at a post-boundary instant against cerberus$`, w.whenProbePostBoundaryCerberus)
	ctx.Step(`^cerberus answers the post-boundary instant with every series the seeder declared$`, w.thenCerberusAnswersPostBoundary)
	ctx.Step(`^the operator queries the probe metric at the pre-boundary instant against cerberus$`, w.whenProbePreBoundaryCerberus)
	ctx.Step(`^cerberus returns no series at all for the pre-boundary instant, tracing back to ClickHouse holding nothing there$`,
		w.thenCerberusEmptyPreBoundary)
	ctx.Step(`^the operator queries the same pre-boundary instant against the incumbent$`, w.whenProbePreBoundaryIncumbent)
	ctx.Step(`^the incumbent answers the pre-boundary instant with every series the seeder left it from before ingest-start$`,
		w.thenIncumbentAnswersPreBoundary)
	ctx.Step(`^the operator reads the live ClickHouse retention and compares it against the measured ingest-start$`,
		w.whenCompareLiveRetentionToBoundary)
	ctx.Step(`^the retention comparison refuses to call the split removable, because ClickHouse's retention has not yet aged past its own ingest-start$`,
		w.thenSplitNotYetRemovable)
	ctx.Step(`^ClickHouse's retention still covers the pre-boundary instant, so cerberus answering nothing there is an ingest-start gap rather than an expired one$`,
		w.thenRetentionCoversPreBoundaryInstant)
}

// --- MIG-22: the read moves and rolls back by a base-URL change alone ------
//
// What this half does NOT do, and why: the tier-1 stack runs no Grafana, so
// nothing here provisions a datasource, edits one, or rolls a provisioning
// file back. What it drives is the property a datasource flip rests on — the
// identical Prometheus-wire request, dialled at cerberus's base URL and then
// at the incumbent's, answers identically at both while both stay live and
// serving. Driving a real provisioned datasource waits on the Grafana leg
// being added to the tier-1 stack; until then the scenario's own text says
// what it checks rather than claiming a flip it never performs.

// givenProbeMetric selects the seeded fixture's own gauge metric as the
// probe every later step queries — read from the manifest the seeder
// published, never a literal the scenario invented, so the probe always names
// a metric this exact run actually wrote.
func (w *World) givenProbeMetric() error {
	if err := w.requireArchetypes("the probe metric"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		m, err := w.live.LoadManifest(a)
		if err != nil {
			return err
		}
		if m.GaugeMetric == "" {
			return fmt.Errorf("archetype %s: the seeder's manifest names no gauge metric; the scenario has nothing to probe", a)
		}
		w.manifest[a] = m
		w.cutover[a] = cutoverProbe{query: m.GaugeMetric, at: m.VerifyEnd}
	}
	return nil
}

// whenProbeIncumbentBaseline records what the incumbent answers before
// anything is retargeted — the ground truth every later probe is compared
// against, and the one answer this scenario requires to carry series before
// any comparison is worth making.
func (w *World) whenProbeIncumbentBaseline() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		res, err := lib.QueryInstant(context.Background(), w.live.PromURL, cp.query, cp.at)
		if err != nil {
			return cp, fmt.Errorf("archetype %s: probe the incumbent before anything is retargeted: %w", a, err)
		}
		if !res.Vector.NonEmpty() {
			return cp, fmt.Errorf("archetype %s: the incumbent's own probe returned no series at all, so nothing compared against it would decide anything", a)
		}
		cp.baseline = res
		return cp, nil
	})
}

// whenRetargetProbeAtCerberus re-issues the identical probe at cerberus's base
// URL. The retarget is nothing but a different host on the exact same request
// — which is the whole claim, and why thenRetargetDifferedOnlyByHost checks
// the recorded request URLs rather than trusting this wiring.
func (w *World) whenRetargetProbeAtCerberus() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		if cp.baseline.Vector == nil {
			return cp, fmt.Errorf("archetype %s: no incumbent answer recorded; the scenario must probe the incumbent first", a)
		}
		res, err := lib.QueryInstant(context.Background(), w.live.CerberusURL, cp.query, cp.at)
		if err != nil {
			return cp, fmt.Errorf("archetype %s: probe cerberus with the retargeted request: %w", a, err)
		}
		cp.retargeted = res
		return cp, nil
	})
}

// thenRetargetedAgreesWithBaseline asserts cerberus answered with series of
// its own AND that those series are the incumbent's, value for value. The
// cardinality check is stated rather than inherited: an operator moving a
// panel cares that cerberus returns data, not merely that two answers were
// comparable.
func (w *World) thenRetargetedAgreesWithBaseline() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		if !cp.retargeted.Vector.NonEmpty() {
			return cp, fmt.Errorf("archetype %s: cerberus answered the retargeted probe with no series at all", a)
		}
		if !cp.retargeted.Vector.Equal(cp.baseline.Vector) {
			return cp, fmt.Errorf("archetype %s: cerberus's answer (%d series) disagrees with the incumbent's own (%d series)",
				a, len(cp.retargeted.Vector), len(cp.baseline.Vector))
		}
		return cp, nil
	})
}

// thenIncumbentServesAlongsideCerberus proves the move was additive rather
// than destructive, by measurement and not by reachability: with cerberus
// already serving the read, the incumbent still answers the same probe with
// the same series it did before. A backend that had been swapped out would
// answer nothing here — and a bare readiness endpoint would not have noticed.
func (w *World) thenIncumbentServesAlongsideCerberus() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		res, err := lib.QueryInstant(context.Background(), w.live.PromURL, cp.query, cp.at)
		if err != nil {
			return cp, fmt.Errorf("archetype %s: the incumbent stopped answering while cerberus served the read: %w", a, err)
		}
		if !res.Vector.NonEmpty() {
			return cp, fmt.Errorf("archetype %s: the incumbent answered with no series while cerberus served the read, so it was displaced rather than joined", a)
		}
		if !res.Vector.Equal(cp.baseline.Vector) {
			return cp, fmt.Errorf("archetype %s: the incumbent's answer changed while cerberus served the read", a)
		}
		cp.alongside = res
		return cp, nil
	})
}

// whenRetargetProbeBackAtIncumbent re-issues the identical probe at the
// incumbent's base URL — the rollback is, symmetrically, nothing but the host
// changing back.
func (w *World) whenRetargetProbeBackAtIncumbent() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		res, err := lib.QueryInstant(context.Background(), w.live.PromURL, cp.query, cp.at)
		if err != nil {
			return cp, fmt.Errorf("archetype %s: probe the incumbent after the rollback: %w", a, err)
		}
		cp.rolledBack = res
		return cp, nil
	})
}

func (w *World) thenRolledBackAgreesWithBaseline() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		if !cp.rolledBack.Vector.NonEmpty() {
			return cp, fmt.Errorf("archetype %s: the incumbent answered the rolled-back probe with no series at all", a)
		}
		if !cp.rolledBack.Vector.Equal(cp.baseline.Vector) {
			return cp, fmt.Errorf("archetype %s: the incumbent's answer after the rollback disagrees with its own earlier answer", a)
		}
		return cp, nil
	})
}

// thenCerberusStillAnswersAfterRollback is the rollback's own cost measured:
// rolling the read back to the incumbent must leave cerberus exactly as it
// was, still serving the same series, so the step is reversible again in the
// other direction rather than one-way.
func (w *World) thenCerberusStillAnswersAfterRollback() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		res, err := lib.QueryInstant(context.Background(), w.live.CerberusURL, cp.query, cp.at)
		if err != nil {
			return cp, fmt.Errorf("archetype %s: cerberus stopped answering after the rollback: %w", a, err)
		}
		if !res.Vector.NonEmpty() {
			return cp, fmt.Errorf("archetype %s: cerberus answered with no series after the rollback", a)
		}
		if !res.Vector.Equal(cp.baseline.Vector) {
			return cp, fmt.Errorf("archetype %s: cerberus's answer changed across the rollback", a)
		}
		cp.afterRollback = res
		return cp, nil
	})
}

// thenRetargetDifferedOnlyByHost is the "one-line rollback" claim made
// mechanical, over the request URLs the probes RECORDED as dialled rather than
// over anything re-derived here:
//
//   - the two backends must actually be two backends (a stack that published
//     one URL for both would make every agreement above trivially true);
//   - the retargeted request must have been dialled at cerberus's own host and
//     the rolled-back one at the incumbent's, so a retarget that quietly went
//     back to the incumbent fails instead of agreeing with itself;
//   - all three requests must share one byte-identical path, so the move
//     touched the host and nothing else.
func (w *World) thenRetargetDifferedOnlyByHost() error {
	if w.live.CerberusURL == w.live.PromURL {
		return fmt.Errorf("cerberus and the incumbent are published at the same base url %q, so no retarget could move anything", w.live.PromURL)
	}
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		dialled := []struct {
			what string
			res  lib.ProbeResult
			host string
		}{
			{"the incumbent's own answer", cp.baseline, w.live.PromURL},
			{"the retarget at cerberus", cp.retargeted, w.live.CerberusURL},
			{"the incumbent alongside cerberus", cp.alongside, w.live.PromURL},
			{"the rollback to the incumbent", cp.rolledBack, w.live.PromURL},
			{"cerberus after the rollback", cp.afterRollback, w.live.CerberusURL},
		}
		var want string
		for _, d := range dialled {
			if !strings.HasPrefix(d.res.RequestURL, d.host) {
				return cp, fmt.Errorf("archetype %s: %s was dialled at %q, not at %q", a, d.what, d.res.RequestURL, d.host)
			}
			path, err := lib.RequestPath(d.res.RequestURL)
			if err != nil {
				return cp, fmt.Errorf("archetype %s: %w", a, err)
			}
			if want == "" {
				want = path
				continue
			}
			if path != want {
				return cp, fmt.Errorf("archetype %s: %s dialled %q where every other probe dialled %q; a move that is one line changes the host and nothing else",
					a, d.what, path, want)
			}
		}
		return cp, nil
	})
}

// eachCutover runs fn over every tagged archetype's cutover state, writing
// back whatever fn returns so a chain of When/Then steps accumulates onto the
// same record.
func (w *World) eachCutover(fn func(archetype string, cp cutoverProbe) (cutoverProbe, error)) error {
	if err := w.requireArchetypes("the cutover assertion"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		cp, ok := w.cutover[a]
		if !ok {
			return fmt.Errorf("archetype %s: no probe metric selected; the scenario must establish one first", a)
		}
		next, err := fn(a, cp)
		if err != nil {
			return err
		}
		w.cutover[a] = next
	}
	return nil
}

// --- MIG-23: historical reads split at ClickHouse's ingest-start -----------
//
// The router that performs the split is operator-owned infrastructure — an
// ingress rule, a proxy route, a per-datasource pattern in Grafana — and
// lives outside this repository: cerberus ships no boundary configuration and
// no backend-routing layer, and these steps deliberately assert against none.
// What they do assert is the substrate fact such a router is built on, and
// which no cerberus feature supplies: ClickHouse's ingest-start is a real,
// measurable instant; cerberus answers with the fixture's full cardinality
// above it and with nothing below it; the incumbent answers with the history
// the seeder left it below it; and ClickHouse's own retention has not yet
// aged past the boundary, so the incumbent cannot be retired yet.

// givenIngestStart reads the declared ingest-start boundary and every oracle
// the scenario is held to off the seeder's manifest — the seeder's own account
// of what it wrote — rather than off a literal the scenario invented or off
// the answers it is about to compare.
//
// It also refuses to start at all unless the fixture actually left the
// incumbent history below the boundary. Without that history both backends are
// equally empty before SeedStart, and the pre-boundary contrast this whole
// scenario rests on would be two empty answers agreeing with each other.
func (w *World) givenIngestStart() error {
	if err := w.requireArchetypes("the ingest-start boundary"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		m, err := w.live.LoadManifest(a)
		if err != nil {
			return err
		}
		if m.GaugeMetric == "" {
			return fmt.Errorf("archetype %s: the seeder's manifest names no gauge metric; the scenario has nothing to probe", a)
		}
		if m.Series.Gauge <= 0 || m.SamplesPerSeries <= 0 {
			return fmt.Errorf("archetype %s: the seeder declared %d gauge series over %d samples each; "+
				"there is no positive cardinality for cerberus's answer to be held to",
				a, m.Series.Gauge, m.SamplesPerSeries)
		}
		if m.PreIngestSeries <= 0 || m.PreIngestSamplesPerSeries <= 0 {
			return fmt.Errorf("archetype %s: the seeder left the incumbent %d series of %d samples before ingest-start; "+
				"with no incumbent-only history the split this scenario asserts has no content",
				a, m.PreIngestSeries, m.PreIngestSamplesPerSeries)
		}
		preInstant := m.SeedStart.Add(-boundaryPreMargin)
		if preInstant.Before(m.PreIngestStart) || !preInstant.Before(m.SeedStart) {
			return fmt.Errorf("archetype %s: the pre-boundary instant %s falls outside the span only the incumbent holds (%s until %s)",
				a, preInstant.UTC(), m.PreIngestStart.UTC(), m.SeedStart.UTC())
		}
		w.manifest[a] = m
		w.boundary[a] = boundaryProbe{
			metric:        m.GaugeMetric,
			declaredStart: m.SeedStart,
			preInstant:    preInstant,
			// The last instant the fixture wrote, which is also the last one
			// the parity lane queries: as deep inside ClickHouse's own span as
			// the fixture goes.
			postInstant: m.VerifyEnd,
			// One ClickHouse row per declared series per declared sample; both
			// factors are checked positive immediately above, so the widening
			// carries no sign surprise.
			wantRowsAtOrAfter: uint64(m.Series.Gauge) * uint64(m.SamplesPerSeries),
			wantSeries:        m.Series.Gauge,
			wantIncumbentPre:  m.PreIngestSeries,
		}
	}
	return nil
}

// boundaryCensusSQL reads, in ONE pass over the physical table cerberus itself
// reads, the four facts this scenario's boundary claims rest on: how many rows
// the probe metric has at all, the earliest instant any of them carries, and
// how many land either side of the declared boundary.
//
// The total travels with the rest because `min()` over an empty set answers
// with the epoch rather than an error, and an epoch "measurement" silently
// satisfying a "nothing before the boundary" check is exactly the hollow shape
// this scenario is being held to.
const boundaryCensusSQL = "SELECT count(), min(TimeUnix), countIf(TimeUnix < ?), countIf(TimeUnix >= ?) " +
	"FROM otel_metrics_gauge WHERE MetricName = ?"

// boundaryCensus is what one boundaryCensusSQL pass returned.
type boundaryCensus struct {
	rows      uint64
	earliest  time.Time
	before    uint64
	atOrAfter uint64
}

// censusBoundaryRows dials the live ClickHouse and takes the boundary census
// for metric around boundary.
func (w *World) censusBoundaryRows(ctx context.Context, metric string, boundary time.Time) (boundaryCensus, error) {
	dialCtx, cancel := context.WithTimeout(ctx, liveCHDialBudget)
	defer cancel()
	conn, err := seed.DialCH(dialCtx, seed.CHConfig{
		Addr:     w.live.CHAddr,
		Database: w.live.CHDatabase,
		Username: w.live.CHUsername,
		Password: w.live.CHPassword,
	})
	if err != nil {
		return boundaryCensus{}, fmt.Errorf("migration harness: dial the live clickhouse to census the boundary: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var c boundaryCensus
	if err := conn.QueryRow(ctx, boundaryCensusSQL, boundary, boundary, metric).
		Scan(&c.rows, &c.earliest, &c.before, &c.atOrAfter); err != nil {
		return boundaryCensus{}, fmt.Errorf("migration harness: census rows either side of ingest-start: %w", err)
	}
	if c.rows == 0 {
		return boundaryCensus{}, fmt.Errorf(
			"migration harness: the live clickhouse holds no %s rows at all, so it has no ingest-start to measure", metric,
		)
	}
	return c, nil
}

func (w *World) whenCensusBoundaryRows() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		c, err := w.censusBoundaryRows(context.Background(), bp.metric, bp.declaredStart)
		if err != nil {
			return bp, fmt.Errorf("archetype %s: %w", a, err)
		}
		bp.measuredStart = c.earliest
		bp.rowsBefore, bp.rowsAtOrAfter = c.before, c.atOrAfter
		return bp, nil
	})
}

// thenMeasuredBoundaryMatchesDeclared is the observability half of the story:
// the split point an operator would configure their router with is the same
// instant the substrate itself reports, so it can be read off the live cluster
// instead of being carried around as a deployment note that silently rots.
func (w *World) thenMeasuredBoundaryMatchesDeclared() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		if !bp.measuredStart.Equal(bp.declaredStart) {
			return bp, fmt.Errorf(
				"archetype %s: clickhouse reports its earliest %s row at %s, but the seeder declared ingest-start at %s; "+
					"a boundary the substrate does not agree with cannot be the one a router splits on",
				a, bp.metric, bp.measuredStart.UTC(), bp.declaredStart.UTC(),
			)
		}
		return bp, nil
	})
}

func (w *World) thenNoRowsBeforeBoundary() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		if bp.rowsBefore != 0 {
			return bp, fmt.Errorf("archetype %s: clickhouse holds %d row(s) before its own ingest-start, so the boundary is not where the fixture says it is",
				a, bp.rowsBefore)
		}
		return bp, nil
	})
}

// thenRowsAtOrAfterBoundary holds ClickHouse's own side of the split to the
// seeder's declared volume — one row per declared series per declared sample —
// not merely to "something landed". A half-written ClickHouse side would
// otherwise satisfy every remaining assertion in this scenario.
func (w *World) thenRowsAtOrAfterBoundary() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		if bp.rowsAtOrAfter != bp.wantRowsAtOrAfter {
			return bp, fmt.Errorf("archetype %s: clickhouse holds %d %s row(s) at or after ingest-start, but the seeder declared %d",
				a, bp.rowsAtOrAfter, bp.metric, bp.wantRowsAtOrAfter)
		}
		return bp, nil
	})
}

func (w *World) whenProbePostBoundaryCerberus() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		res, err := lib.QueryInstant(context.Background(), w.live.CerberusURL, bp.metric, bp.postInstant)
		if err != nil {
			return bp, fmt.Errorf("archetype %s: probe cerberus at the post-boundary instant: %w", a, err)
		}
		bp.cerberusPost = res
		return bp, nil
	})
}

// thenCerberusAnswersPostBoundary is the positive half of the split: above the
// boundary cerberus answers with the fixture's full declared cardinality. It is
// what keeps the pre-boundary emptiness meaningful — a cerberus answering every
// instant with nothing at all would otherwise satisfy this scenario end to end.
func (w *World) thenCerberusAnswersPostBoundary() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		if len(bp.cerberusPost.Vector) != bp.wantSeries {
			return bp, fmt.Errorf("archetype %s: cerberus answered the post-boundary instant %s with %d series, but the seeder declared %d",
				a, bp.postInstant.UTC(), len(bp.cerberusPost.Vector), bp.wantSeries)
		}
		return bp, nil
	})
}

func (w *World) whenProbePreBoundaryCerberus() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		res, err := lib.QueryInstant(context.Background(), w.live.CerberusURL, bp.metric, bp.preInstant)
		if err != nil {
			return bp, fmt.Errorf("archetype %s: probe cerberus at the pre-boundary instant: %w", a, err)
		}
		bp.cerberusPre = res
		return bp, nil
	})
}

func (w *World) thenCerberusEmptyPreBoundary() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		if bp.cerberusPre.Vector.NonEmpty() {
			return bp, fmt.Errorf("archetype %s: cerberus answered the pre-boundary instant with %d series, but clickhouse holds nothing before ingest-start",
				a, len(bp.cerberusPre.Vector))
		}
		return bp, nil
	})
}

func (w *World) whenProbePreBoundaryIncumbent() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		res, err := lib.QueryInstant(context.Background(), w.live.PromURL, bp.metric, bp.preInstant)
		if err != nil {
			return bp, fmt.Errorf("archetype %s: probe the incumbent at the pre-boundary instant: %w", a, err)
		}
		bp.incumbentPre = res
		return bp, nil
	})
}

// thenIncumbentAnswersPreBoundary holds the incumbent's pre-boundary answer to
// the history the seeder actually left it there — the manifest's own
// pre-ingest cardinality, a positive number — rather than to the mere fact
// that a request was dialled at its URL. Paired with the two probes above it,
// this is the entire story made mechanical: at ONE instant, cerberus answers
// with nothing and the incumbent answers with everything, so the read path an
// operator points at that instant is not a matter of taste.
func (w *World) thenIncumbentAnswersPreBoundary() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		if len(bp.incumbentPre.Vector) != bp.wantIncumbentPre {
			return bp, fmt.Errorf("archetype %s: the incumbent answered the pre-boundary instant %s with %d series, but the seeder left it %d there; "+
				"an incumbent that cannot answer below the boundary makes the split unserviceable",
				a, bp.preInstant.UTC(), len(bp.incumbentPre.Vector), bp.wantIncumbentPre)
		}
		return bp, nil
	})
}

// whenCompareLiveRetentionToBoundary reads the live ClickHouse retention
// (never the schema renderer's own text — see readLiveRetention) and turns it
// into the oldest instant ClickHouse still keeps: now minus that TTL. A signal
// whose tables carry no TTL clause at all fails closed here, exactly as
// thenRetentionCoversLookback already does for MIG-14 — absence never reads as
// "unbounded", and it never reads as "the gate held" either.
//
// Where that horizon sits decides both remaining assertions. Once it reaches
// ingest-start, ClickHouse has begun expiring rows from around the boundary
// itself: every query it can still legally answer starts after ingest-start
// anyway, the special case collapses into ordinary TTL-bounded routing, and
// the split has nothing left to add. While it stays below the pre-boundary
// instant, the reverse holds — the instant the incumbent just answered is one
// ClickHouse would still be keeping if it had ever ingested it, so the gap is
// an ingest-start gap and no TTL change closes it.
func (w *World) whenCompareLiveRetentionToBoundary() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		retention, err := w.readLiveRetention(context.Background())
		if err != nil {
			return bp, fmt.Errorf("archetype %s: %w", a, err)
		}
		have, ok := retention[signalMetrics]
		if !ok {
			return bp, fmt.Errorf("archetype %s: the live clickhouse metrics tables carry no TTL clause at all, "+
				"so there is no retention to compare against ingest-start; absence is not a passing gate", a)
		}
		bp.liveRetention = have
		bp.retentionHorizon = time.Now().UTC().Add(-have)
		return bp, nil
	})
}

func (w *World) thenSplitNotYetRemovable() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		if !bp.retentionHorizon.Before(bp.measuredStart) {
			return bp, fmt.Errorf(
				"archetype %s: the live retention (%s) already reaches back to %s, at or past clickhouse's own ingest-start %s; "+
					"this scenario asserts the split is still required this soon after cutover",
				a, bp.liveRetention, bp.retentionHorizon.UTC(), bp.measuredStart.UTC(),
			)
		}
		return bp, nil
	})
}

// thenRetentionCoversPreBoundaryInstant attributes the emptiness. cerberus
// answering nothing at the pre-boundary instant only proves the ingest-start
// story if that instant is one ClickHouse would still be RETAINING had it ever
// held it; if the live TTL had already expired past it, the same empty answer
// would be explained by retention and the scenario's conclusion would not
// follow from its evidence.
func (w *World) thenRetentionCoversPreBoundaryInstant() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		if bp.retentionHorizon.After(bp.preInstant) {
			return bp, fmt.Errorf(
				"archetype %s: the live retention (%s) only reaches back to %s, later than the pre-boundary instant %s; "+
					"clickhouse's empty answer there would be expiry rather than an ingest-start gap",
				a, bp.liveRetention, bp.retentionHorizon.UTC(), bp.preInstant.UTC(),
			)
		}
		return bp, nil
	})
}

// eachBoundary runs fn over every tagged archetype's boundary state, writing
// back whatever fn returns.
func (w *World) eachBoundary(fn func(archetype string, bp boundaryProbe) (boundaryProbe, error)) error {
	if err := w.requireArchetypes("the boundary assertion"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		bp, ok := w.boundary[a]
		if !ok {
			return fmt.Errorf("archetype %s: no ingest-start boundary established; the scenario must establish one first", a)
		}
		next, err := fn(a, bp)
		if err != nil {
			return err
		}
		w.boundary[a] = next
	}
	return nil
}
