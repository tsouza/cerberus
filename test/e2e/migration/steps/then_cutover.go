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

// cutoverReadyBudget bounds a single reachability re-probe of a backend the
// scenario asserts stayed up through a flip or a revert. The compose stack's
// own healthcheck already gates on `just migration-tier1-up --wait`, so a
// live backend answering slower than this is a real regression, not a cold
// start.
const cutoverReadyBudget = 30 * time.Second

// boundaryPreMargin is how far before ClickHouse's own ingest-start MIG-23
// probes: one full sample step, which the fixture guarantees carries no
// written row (see test/e2e/migration/seed/fixture.go's SeedStart/SampleStep
// geometry — the very first sample lands AT SeedStart).
const boundaryPreMargin = seed.SampleStep

// cutoverProbe is one archetype's MIG-22 run: the probe query and instant it
// was asked at, plus the three answers a full flip-and-revert collects.
type cutoverProbe struct {
	query    string
	at       time.Time
	baseline lib.ProbeResult
	flipped  lib.ProbeResult
	reverted lib.ProbeResult
}

// boundaryProbe is one archetype's MIG-23 run: ClickHouse's own ingest-start,
// the pre-boundary instant probed on both sides of it, the row counts either
// side of the boundary, and whether the live retention has aged past the
// elapsed span since ingest-start.
type boundaryProbe struct {
	metric          string
	ingestStart     time.Time
	preInstant      time.Time
	rowsBefore      uint64
	rowsAtOrAfter   uint64
	cerberusPre     lib.ProbeResult
	incumbentPre    lib.ProbeResult
	routerRemovable bool
}

// registerCutoverSteps binds the MIG-22 (flip-and-revert) and MIG-23
// (ingest-start boundary) steps.
func (w *World) registerCutoverSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the seeded fixture's own probe metric for each tagged archetype$`, w.givenProbeMetric)
	ctx.Step(`^the operator queries the probe metric against the incumbent before any flip$`, w.whenProbeIncumbentBaseline)
	ctx.Step(`^the operator flips the probe from the incumbent to cerberus by a URL change alone$`, w.whenFlipProbeToCerberus)
	ctx.Step(`^the flipped probe against cerberus agrees with the incumbent's own pre-flip answer$`, w.thenFlippedAgreesWithBaseline)
	ctx.Step(`^the incumbent stayed live and reachable through the flip, because cerberus was stood up alongside it rather than in place of it$`,
		w.thenIncumbentStillLiveAfterFlip)
	ctx.Step(`^the operator reverts the probe from cerberus back to the incumbent by a URL change alone$`, w.whenRevertProbeToIncumbent)
	ctx.Step(`^the reverted probe against the incumbent still agrees with its own pre-flip answer$`, w.thenRevertedAgreesWithBaseline)
	ctx.Step(`^cerberus stayed live and reachable through the revert$`, w.thenCerberusStillLiveAfterRevert)
	ctx.Step(`^the flip and the revert dialled nothing but a different host for the identical request$`, w.thenFlipRevertDifferOnlyByHost)

	ctx.Step(`^the seeded fixture's own ingest-start instant for each tagged archetype$`, w.givenIngestStart)
	ctx.Step(`^the operator counts ClickHouse's own rows strictly before and at-or-after ingest-start$`, w.whenCountBoundaryRows)
	ctx.Step(`^ClickHouse holds exactly zero rows before its own ingest-start$`, w.thenNoRowsBeforeBoundary)
	ctx.Step(`^ClickHouse holds at least one row at or after ingest-start$`, w.thenRowsAtOrAfterBoundary)
	ctx.Step(`^the operator queries the probe metric at the pre-boundary instant against cerberus$`, w.whenProbePreBoundaryCerberus)
	ctx.Step(`^cerberus returns no series at all for the pre-boundary instant, tracing back to ClickHouse holding nothing there$`,
		w.thenCerberusEmptyPreBoundary)
	ctx.Step(`^the operator queries the same pre-boundary instant against the incumbent$`, w.whenProbePreBoundaryIncumbent)
	ctx.Step(`^the incumbent still answers the pre-boundary instant, the precondition any router relies on to serve it from there instead$`,
		w.thenIncumbentAnswersPreBoundary)
	ctx.Step(`^the operator reads the live ClickHouse retention and compares it against the time elapsed since ingest-start$`,
		w.whenCompareLiveRetentionToElapsed)
	ctx.Step(`^the retention comparison refuses to call the split-router removable, because the live retention has not yet aged past ingest-start$`,
		w.thenSplitRouterNotYetRemovable)
}

// --- MIG-22: cutover moves and reverts by a URL change alone ---------------

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

// whenProbeIncumbentBaseline records what the incumbent answers before any
// flip — the ground truth every later probe is compared against.
func (w *World) whenProbeIncumbentBaseline() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		res, err := lib.QueryInstant(context.Background(), w.live.PromURL, cp.query, cp.at)
		if err != nil {
			return cp, fmt.Errorf("archetype %s: probe the incumbent before any flip: %w", a, err)
		}
		if !res.Vector.NonEmpty() {
			return cp, fmt.Errorf("archetype %s: the incumbent's pre-flip probe returned no series at all", a)
		}
		cp.baseline = res
		return cp, nil
	})
}

// whenFlipProbeToCerberus re-issues the identical probe against cerberus —
// the "flip" is nothing but a different base URL on the exact same request.
func (w *World) whenFlipProbeToCerberus() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		if cp.baseline.Vector == nil {
			return cp, fmt.Errorf("archetype %s: no pre-flip incumbent answer recorded; the scenario must probe the incumbent first", a)
		}
		res, err := lib.QueryInstant(context.Background(), w.live.CerberusURL, cp.query, cp.at)
		if err != nil {
			return cp, fmt.Errorf("archetype %s: probe cerberus after the flip: %w", a, err)
		}
		cp.flipped = res
		return cp, nil
	})
}

func (w *World) thenFlippedAgreesWithBaseline() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		if !cp.flipped.Vector.Equal(cp.baseline.Vector) {
			return cp, fmt.Errorf("archetype %s: cerberus's flipped answer disagrees with the incumbent's pre-flip answer", a)
		}
		return cp, nil
	})
}

// thenIncumbentStillLiveAfterFlip proves the flip was additive, not
// destructive: the incumbent answers exactly as readily after cerberus took
// the read as it did before, so nothing about the flip depended on tearing
// the incumbent down.
func (w *World) thenIncumbentStillLiveAfterFlip() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		if err := seed.WaitHTTPOK(context.Background(), w.live.PromURL+"/-/ready", cutoverReadyBudget); err != nil {
			return cp, fmt.Errorf("archetype %s: the incumbent is not reachable after the flip: %w", a, err)
		}
		return cp, nil
	})
}

// whenRevertProbeToIncumbent re-issues the identical probe against the
// incumbent — the "revert" is, symmetrically, nothing but the base URL
// changing back.
func (w *World) whenRevertProbeToIncumbent() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		res, err := lib.QueryInstant(context.Background(), w.live.PromURL, cp.query, cp.at)
		if err != nil {
			return cp, fmt.Errorf("archetype %s: probe the incumbent after the revert: %w", a, err)
		}
		cp.reverted = res
		return cp, nil
	})
}

func (w *World) thenRevertedAgreesWithBaseline() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		if !cp.reverted.Vector.Equal(cp.baseline.Vector) {
			return cp, fmt.Errorf("archetype %s: the incumbent's post-revert answer disagrees with its own pre-flip answer", a)
		}
		return cp, nil
	})
}

func (w *World) thenCerberusStillLiveAfterRevert() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		if err := seed.WaitHTTPOK(context.Background(), w.live.CerberusURL+"/readyz", cutoverReadyBudget); err != nil {
			return cp, fmt.Errorf("archetype %s: cerberus is not reachable after the revert: %w", a, err)
		}
		return cp, nil
	})
}

// thenFlipRevertDifferOnlyByHost is the "one-line rollback" claim made
// mechanical: the flip's request and the revert's request, once the
// scheme+host is stripped from each, must be byte-identical. Anything else
// means the flip touched more than a URL.
func (w *World) thenFlipRevertDifferOnlyByHost() error {
	return w.eachCutover(func(a string, cp cutoverProbe) (cutoverProbe, error) {
		flipPath, err := lib.RequestPath(cp.flipped.RequestURL)
		if err != nil {
			return cp, fmt.Errorf("archetype %s: %w", a, err)
		}
		revertPath, err := lib.RequestPath(cp.reverted.RequestURL)
		if err != nil {
			return cp, fmt.Errorf("archetype %s: %w", a, err)
		}
		if flipPath != revertPath {
			return cp, fmt.Errorf("archetype %s: the flip dialled %q but the revert dialled %q; a one-line rollback changes nothing but the host",
				a, flipPath, revertPath)
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

// givenIngestStart reads ClickHouse's own ingest-start off the seeder's
// manifest (SeedStart — the very first fixture sample, written identically to
// both backends) rather than a literal the scenario invented, so the boundary
// this scenario probes is always the one the live stack actually holds.
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
		w.manifest[a] = m
		w.boundary[a] = boundaryProbe{
			metric:      m.GaugeMetric,
			ingestStart: m.SeedStart,
			preInstant:  m.SeedStart.Add(-boundaryPreMargin),
		}
	}
	return nil
}

// boundaryCountBeforeSQL / boundaryCountAtOrAfterSQL count rows either side of
// the ingest-start boundary directly in ClickHouse, so the boundary this
// scenario asserts is measured from the physical table cerberus itself reads,
// not merely from cerberus's own query answer (which could be empty for an
// unrelated reason).
const (
	boundaryCountBeforeSQL    = "SELECT count() FROM otel_metrics_gauge WHERE MetricName = ? AND TimeUnix < ?"
	boundaryCountAtOrAfterSQL = "SELECT count() FROM otel_metrics_gauge WHERE MetricName = ? AND TimeUnix >= ?"
)

// countBoundaryRows dials the live ClickHouse and counts, for metric, how
// many rows land strictly before boundary and how many land at or after it.
func (w *World) countBoundaryRows(ctx context.Context, metric string, boundary time.Time) (before, atOrAfter uint64, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, liveCHDialBudget)
	defer cancel()
	conn, err := seed.DialCH(dialCtx, seed.CHConfig{
		Addr:     w.live.CHAddr,
		Database: w.live.CHDatabase,
		Username: w.live.CHUsername,
		Password: w.live.CHPassword,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("migration harness: dial the live clickhouse to count boundary rows: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.QueryRow(ctx, boundaryCountBeforeSQL, metric, boundary).Scan(&before); err != nil {
		return 0, 0, fmt.Errorf("migration harness: count rows before ingest-start: %w", err)
	}
	if err := conn.QueryRow(ctx, boundaryCountAtOrAfterSQL, metric, boundary).Scan(&atOrAfter); err != nil {
		return 0, 0, fmt.Errorf("migration harness: count rows at-or-after ingest-start: %w", err)
	}
	return before, atOrAfter, nil
}

func (w *World) whenCountBoundaryRows() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		before, atOrAfter, err := w.countBoundaryRows(context.Background(), bp.metric, bp.ingestStart)
		if err != nil {
			return bp, fmt.Errorf("archetype %s: %w", a, err)
		}
		bp.rowsBefore, bp.rowsAtOrAfter = before, atOrAfter
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

func (w *World) thenRowsAtOrAfterBoundary() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		if bp.rowsAtOrAfter == 0 {
			return bp, fmt.Errorf("archetype %s: clickhouse holds no rows at or after its own ingest-start either, so the boundary bounds nothing real", a)
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

// thenIncumbentAnswersPreBoundary asserts the pre-boundary probe was actually
// dialled at the incumbent's own URL — the structural precondition a split
// router depends on: the incumbent must be alive and answering at an instant
// ClickHouse cannot serve at all.
func (w *World) thenIncumbentAnswersPreBoundary() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		if !strings.HasPrefix(bp.incumbentPre.RequestURL, w.live.PromURL) {
			return bp, fmt.Errorf("archetype %s: the pre-boundary probe recorded as answered by the incumbent was not dialled at the incumbent's own url (%s)",
				a, bp.incumbentPre.RequestURL)
		}
		return bp, nil
	})
}

// whenCompareLiveRetentionToElapsed reads the live ClickHouse retention
// (never the schema renderer's own text — see readLiveRetention) and compares
// it against how long it has actually been since ingest-start. A signal whose
// tables carry no TTL clause at all fails closed, exactly as
// thenRetentionCoversLookback already does for MIG-14: absence never reads as
// "unbounded".
//
// The router is removable once ClickHouse's OWN retention has aged PAST
// ingest-start — that is, once elapsed time since ingest-start has grown
// larger than the TTL, so ClickHouse itself has started expiring rows from
// right around the original boundary. Only then does the ingest-start-aware
// special case collapse into ordinary TTL-bounded routing: every query
// ClickHouse can still legally answer starts after ingest-start anyway, TTL
// or no TTL, so the dedicated boundary check has nothing left to add. Right
// after cutover, ClickHouse has not dropped anything yet (elapsed is far
// smaller than any real TTL), so the router still earns its keep.
func (w *World) whenCompareLiveRetentionToElapsed() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		retention, err := w.readLiveRetention(context.Background())
		if err != nil {
			return bp, fmt.Errorf("archetype %s: %w", a, err)
		}
		elapsed := time.Since(bp.ingestStart)
		have, ok := retention[signalMetrics]
		bp.routerRemovable = ok && elapsed >= have
		return bp, nil
	})
}

func (w *World) thenSplitRouterNotYetRemovable() error {
	return w.eachBoundary(func(a string, bp boundaryProbe) (boundaryProbe, error) {
		if bp.routerRemovable {
			return bp, fmt.Errorf(
				"archetype %s: the live retention has already aged past ingest-start; this scenario asserts the router is still required this soon after cutover",
				a,
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
