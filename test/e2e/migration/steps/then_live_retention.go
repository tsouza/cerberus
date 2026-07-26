package steps

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
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

// liveTTLClauseRe captures the retention the COLLECTOR actually provisioned on
// a live table, read straight out of `SHOW CREATE TABLE`. It is deliberately a
// different pattern than then_lookback.go's ttlClauseRe: cerberus's own
// renderer wraps the TTL column in toDateTime(...), but the OTel-CH exporter's
// GenerateTTLExpr (internal/clickhouse.go in the vendored fork) emits the
// column bare — `TTL <column> + toIntervalDay(30)`, no wrapper — which is
// exactly the byte-for-byte difference MIG-10 exists to catch. This step reads
// what the collector actually applied, not what cerberus would render, so it
// must match the collector's own shape rather than reusing ttlClauseRe.
var liveTTLClauseRe = regexp.MustCompile(`TTL\s+\S+\s*\+\s*toInterval([A-Za-z]+)\((\d+)\)`)

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

	bySignal, err := liveRetentionBySignal(stmts)
	if err != nil {
		return err
	}
	if len(bySignal) == 0 {
		return fmt.Errorf("the live collector provisions no retention at all on any signal table, so it bounds nothing")
	}
	w.retention = signalRetention{read: true, bySignal: bySignal}
	return nil
}

// liveRetentionBySignal is then_lookback.go's retentionBySignal, reading the
// collector's bare-column TTL shape (liveTTLClauseRe) instead of cerberus's
// own toDateTime(...)-wrapped one. It shares signalOfTable/unqualify/ttlUnits
// with the Tier-0 path so both halves fold "shortest TTL wins per signal" the
// same way.
func liveRetentionBySignal(stmts []string) (map[string]time.Duration, error) {
	out := map[string]time.Duration{}
	for _, stmt := range stmts {
		m := createObjectRe.FindStringSubmatch(stmt)
		if m == nil {
			continue
		}
		signal, ok := signalOfTable(unqualify(m[1]))
		if !ok {
			continue
		}
		ttl := liveTTLClauseRe.FindStringSubmatch(stmt)
		if ttl == nil {
			continue
		}
		unit, ok := ttlUnits[ttl[1]]
		if !ok {
			return nil, fmt.Errorf("the live table for %s uses the interval unit %q, which the harness cannot convert", signal, ttl[1])
		}
		n, err := strconv.Atoi(ttl[2])
		if err != nil {
			return nil, fmt.Errorf("unreadable live retention count %q: %w", ttl[2], err)
		}
		d := time.Duration(n) * unit
		if cur, seen := out[signal]; !seen || d < cur {
			out[signal] = d
		}
	}
	return out, nil
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
