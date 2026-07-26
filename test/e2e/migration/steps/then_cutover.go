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
// probes: one full sample step, which the fixture guarantees carries no
// written row (see test/e2e/migration/seed/fixture.go's SeedStart/SampleStep
// geometry — the very first sample lands AT SeedStart).
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
