package steps

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// liveRetentionBudget bounds the live ClickHouse round-trips this Given makes
// (one SHOW CREATE TABLE per signal table). The stack is already up and
// answering by the time a Tier-1 scenario reaches this step, so this is a
// generous ceiling against a stalled query, not a cold-start allowance.
const liveRetentionBudget = 30 * time.Second

// registerLiveRetentionSteps binds MIG-14's Tier-1 half: the collector-applied
// retention read off the live stack, compared against the same corpus-derived
// lookback the Tier-0 half computes.
func (w *World) registerLiveRetentionSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the ClickHouse retention the live collector actually provisioned$`, w.givenLiveRetention)
	ctx.Step(`^the collector-provisioned retention for each signal covers the longest lookback that signal's queries require$`,
		w.thenLiveRetentionCoversLookback)
}

// givenLiveRetention reads every signal table's TTL clause directly off the
// live ClickHouse the Tier-1 collector provisioned, rather than off a
// `cerberus migrate schema` render that was never applied. This is the
// distinction MIG-10 exists to prove structurally; here it is load-bearing: a
// drift between the two would make this step's answer wrong for the wrong
// reason if it reused the rendered-schema path instead.
func (w *World) givenLiveRetention() error {
	if !w.liveSet {
		return fmt.Errorf("the tier-1 stack has not been established; scenario must establish it first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveRetentionBudget)
	defer cancel()

	conn, err := seed.DialCH(ctx, seed.CHConfig{
		Addr:     w.live.CHAddr,
		Database: w.live.CHDatabase,
		Username: w.live.CHUsername,
		Password: w.live.CHPassword,
	})
	if err != nil {
		return fmt.Errorf("migration harness: dial the live clickhouse for the retention read: %w", err)
	}
	defer func() { _ = conn.Close() }()

	stmts := make([]string, 0, len(signalTables()))
	for _, table := range signalTables() {
		var stmt string
		row := conn.QueryRow(ctx, fmt.Sprintf("SHOW CREATE TABLE %s.%s", w.live.CHDatabase, table))
		if err := row.Scan(&stmt); err != nil {
			return fmt.Errorf("migration harness: SHOW CREATE TABLE %s: %w", table, err)
		}
		stmts = append(stmts, stmt)
	}

	// The fold is then_lookback.go's, unchanged: ttlClauseRe reads the
	// collector's bare-column TTL as readily as cerberus's own
	// toDateTime(...)-wrapped one, and signalOfTable/ttlUnits are shared, so
	// both halves of MIG-14 fold "shortest TTL wins per signal" identically.
	// It refuses an empty result, so a collector whose DDL this harness can no
	// longer read arrives as a failure here rather than as a retention of no
	// signals the Then below would loop over without comparing anything.
	bySignal, err := retentionBySignal("the live collector-provisioned tables", stmts)
	if err != nil {
		return err
	}
	w.retention = signalRetention{read: true, bySignal: bySignal}
	return nil
}

// thenLiveRetentionCoversLookback is the Tier-1 twin of then_lookback.go's
// thenRetentionCoversLookback: same per-signal "both sides measured, no
// threshold to tune" comparison, sourced from the live collector's actual
// retention instead of a rendered-but-unapplied schema. A signal whose
// corpus needs a runway but whose live table carries no TTL at all fails
// rather than being read as unbounded — the collector-configured retention is
// what an operator provisions, and an operator who never set one has not yet
// made the decision this story exists to force.
func (w *World) thenLiveRetentionCoversLookback() error {
	if !w.retention.read {
		return fmt.Errorf("the live collector-provisioned retention has not been read; the scenario must establish it first")
	}
	needed := map[string]time.Duration{}
	if err := w.eachLookback(func(a string, lb migrate.Lookback) error {
		byLang := map[string]string{}
		for _, q := range w.corpus[a].Queries {
			byLang[q.Source+"\x00"+q.Expr] = q.Lang
		}
		for _, q := range lb.ByQuery {
			lang := byLang[q.Source+"\x00"+q.Expr]
			signal, ok := signalOfLang(lang)
			if !ok {
				return fmt.Errorf("archetype %s: the lookback from %s carries language %q, which maps to no signal", a, q.Source, lang)
			}
			if time.Duration(q.Reach) > needed[signal] {
				needed[signal] = time.Duration(q.Reach)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if len(needed) == 0 {
		return fmt.Errorf("no signal has a computed runway, so the live retention comparison would decide nothing")
	}
	// A corpus whose every query reads back no distance would make the
	// comparison below true for any retention at all, including one this
	// harness misread as zero. The runway has to be a real, positive number
	// before the retention it is held against means anything.
	var longest time.Duration
	for _, want := range needed {
		if want > longest {
			longest = want
		}
	}
	if longest <= 0 {
		return fmt.Errorf("the corpus requires no runway at all across %d signal(s), so no live retention could fail this comparison", len(needed))
	}
	for _, signal := range sortedLiveSignals(needed) {
		want := needed[signal]
		have, ok := w.retention.bySignal[signal]
		if !ok || have == 0 {
			return fmt.Errorf(
				"the live collector provisions no %s retention, so nothing bounds the %s runway the corpus needs",
				signal, want,
			)
		}
		if have < want {
			return fmt.Errorf("the live collector provisions %s of %s retention, short of the %s the corpus reads back",
				have, signal, want)
		}
	}
	return nil
}

// sortedLiveSignals lists the signals a live runway comparison covers, in a
// deterministic order so a failure always names the same one first.
func sortedLiveSignals(in map[string]time.Duration) []string {
	out := make([]string, 0, len(in))
	for s := range in {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
