package steps

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cucumber/godog"
	"github.com/prometheus/common/model"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// lookbackGolden is the committed retention runway each archetype's corpus is
// compared against.
const lookbackGolden = "lookback.json"

// The three signals cerberus stores, named as the retention an operator
// provisions is named.
const (
	signalMetrics = "metrics"
	signalLogs    = "logs"
	signalTraces  = "traces"
)

// ttlClauseRe captures the retention a rendered CREATE TABLE provisions:
// `TTL toDateTime(<column>) + toInterval<Unit>(<count>)`. Reading the runway
// out of the DDL measures what would ACTUALLY be provisioned, rather than
// re-deriving it from the same environment variable the renderer read.
var ttlClauseRe = regexp.MustCompile(`TTL\s+toDateTime\([^)]*\)\s*\+\s*toInterval([A-Za-z]+)\((\d+)\)`)

// ttlUnits maps the interval units the schema renderer emits to their duration.
// A unit outside this table is a renderer change the harness must be taught
// about rather than silently read as no retention at all.
var ttlUnits = map[string]time.Duration{
	"Second": time.Second,
	"Minute": time.Minute,
	"Hour":   time.Hour,
	"Day":    24 * time.Hour,
	"Week":   7 * 24 * time.Hour,
}

// signalRetention is the retention the rendered schema provisions per signal:
// the SHORTEST TTL across that signal's tables, because the first table to
// expire is what makes a panel go blank.
type signalRetention struct {
	read     bool
	bySignal map[string]time.Duration
}

// registerLookbackSteps binds the MIG-14 steps.
func (w *World) registerLookbackSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the ClickHouse retention cerberus is configured to provision$`, w.givenConfiguredRetention)
	ctx.Step(`^the operator computes the longest lookback the corpus requires$`, w.whenComputeLookback)
	ctx.Step(`^the computed longest lookback matches its committed golden$`, w.thenLookbackMatchesGolden)
	ctx.Step(`^every query's own lookback is attributed back to the rule or dashboard that needs it$`,
		w.thenLookbackIsAttributed)
	ctx.Step(`^every corpus entry outside the PromQL lookback walk is counted, never assumed to reach back no distance$`,
		w.thenLookbackAccountsForEveryEntry)
	ctx.Step(`^the configured retention for each signal covers the longest lookback that signal's queries require$`,
		w.thenRetentionCoversLookback)
}

// givenConfiguredRetention renders the schema under the case the scenario named
// and reads the per-signal retention out of the DDL cerberus would provision.
func (w *World) givenConfiguredRetention() error {
	if w.schema.caseName == "" {
		return fmt.Errorf("no schema case selected; the scenario must name the schema environment the retention comes from")
	}
	res, err := w.run(w.schema.env, "migrate", "schema")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("rendering the %s schema exited %d: %s",
			w.schema.caseName, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	byLine, err := retentionBySignal(schemaStatements(res.Stdout))
	if err != nil {
		return err
	}
	if len(byLine) == 0 {
		return fmt.Errorf("the %s schema provisions no retention at all, so it bounds nothing", w.schema.caseName)
	}
	w.retention = signalRetention{read: true, bySignal: byLine}
	return nil
}

// retentionBySignal reads each rendered statement's TTL and folds it into the
// signal its table belongs to, keeping the shortest per signal.
func retentionBySignal(stmts []string) (map[string]time.Duration, error) {
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
		ttl := ttlClauseRe.FindStringSubmatch(stmt)
		if ttl == nil {
			continue
		}
		unit, ok := ttlUnits[ttl[1]]
		if !ok {
			return nil, fmt.Errorf("the rendered schema uses the interval unit %q, which the harness cannot convert", ttl[1])
		}
		n, err := strconv.Atoi(ttl[2])
		if err != nil {
			return nil, fmt.Errorf("unreadable retention count %q: %w", ttl[2], err)
		}
		d := time.Duration(n) * unit
		if cur, seen := out[signal]; !seen || d < cur {
			out[signal] = d
		}
	}
	return out, nil
}

// signalOfTable maps one of cerberus's physical tables to the signal whose
// retention it carries.
func signalOfTable(table string) (string, bool) {
	metrics := schema.DefaultOTelMetrics()
	switch table {
	case metrics.GaugeTable, metrics.SumTable, metrics.HistogramTable, metrics.ExpHistogramTable, metrics.SummaryTable:
		return signalMetrics, true
	case schema.DefaultOTelLogs().LogsTable:
		return signalLogs, true
	case schema.DefaultOTelTraces().SpansTable:
		return signalTraces, true
	default:
		return "", false
	}
}

// signalOfLang maps a corpus query's language to the signal whose retention
// bounds how far back it can read.
func signalOfLang(lang string) (string, bool) {
	switch lang {
	case migrate.LangPromQL:
		return signalMetrics, true
	case migrate.LangLogQL:
		return signalLogs, true
	case migrate.LangTraceQL:
		return signalTraces, true
	default:
		return "", false
	}
}

// whenComputeLookback walks each tagged archetype's corpus for the retention
// runway it requires.
func (w *World) whenComputeLookback() error {
	if err := w.requireArchetypes("the lookback computation"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		c, ok := w.corpus[a]
		if !ok {
			return fmt.Errorf("archetype %s has no harvested corpus to walk", a)
		}
		w.lookback[a] = migrate.MaxLookback(c)
	}
	return nil
}

// thenLookbackMatchesGolden compares the computed runway against its committed
// golden and asserts the corpus actually requires one — a runway of zero would
// match a zero golden while proving the walk found nothing.
func (w *World) thenLookbackMatchesGolden() error {
	return w.eachLookback(func(a string, lb migrate.Lookback) error {
		if time.Duration(lb.Longest) <= 0 {
			return fmt.Errorf("archetype %s requires no retention runway at all (%s)", a, lb.Longest)
		}
		if lb.SchemaVersion != migrate.LookbackVersion {
			return fmt.Errorf("archetype %s computed lookback version %d, this build reads %d",
				a, lb.SchemaVersion, migrate.LookbackVersion)
		}
		data, err := lb.Marshal()
		if err != nil {
			return fmt.Errorf("archetype %s: %w", a, err)
		}
		return lib.AssertGolden(w.goldenPath(a, lookbackGolden), data)
	})
}

// thenLookbackIsAttributed asserts every walked query's own reach is traceable
// to the rule or dashboard that needs it, and that the longest figure is one of
// them rather than a number with no query behind it.
func (w *World) thenLookbackIsAttributed() error {
	return w.eachLookback(func(a string, lb migrate.Lookback) error {
		if len(lb.ByQuery) == 0 {
			return fmt.Errorf("archetype %s attributed no query's lookback", a)
		}
		sources := w.corpusSources(a)
		var longest model.Duration
		for _, q := range lb.ByQuery {
			if !hasProvenancePrefix(q.Source) {
				return fmt.Errorf("archetype %s: a lookback of %s names %q, which is neither a rule nor a dashboard",
					a, q.Reach, q.Source)
			}
			if _, ok := sources[q.Source]; !ok {
				return fmt.Errorf("archetype %s: a lookback names source %q, which is not in the corpus it walked", a, q.Source)
			}
			if q.Expr == "" || q.Kind == "" {
				return fmt.Errorf("archetype %s: the lookback from %s carries expr %q and kind %q", a, q.Source, q.Expr, q.Kind)
			}
			if q.Reach > longest {
				longest = q.Reach
			}
		}
		if longest != lb.Longest {
			return fmt.Errorf("archetype %s reports a longest lookback of %s, but the farthest-reaching attributed query needs %s",
				a, lb.Longest, longest)
		}
		return nil
	})
}

// thenLookbackAccountsForEveryEntry asserts the walk partitions the corpus:
// every entry is either attributed a reach or listed with the reason it is
// outside the walk. An entry that fell out silently would be read as reaching
// back no distance, quietly shrinking the runway the operator provisions.
func (w *World) thenLookbackAccountsForEveryEntry() error {
	return w.eachLookback(func(a string, lb migrate.Lookback) error {
		accounted := len(lb.ByQuery) + len(lb.Anchored) + len(lb.NonPromQL) + len(lb.Unparseable)
		if want := len(w.corpus[a].Queries); accounted != want {
			return fmt.Errorf("archetype %s accounted for %d of %d corpus queries (walked=%d anchored=%d non-promql=%d unparseable=%d)",
				a, accounted, want, len(lb.ByQuery), len(lb.Anchored), len(lb.NonPromQL), len(lb.Unparseable))
		}
		for name, list := range map[string][]migrate.SkippedEntry{
			"anchored": lb.Anchored, "non-promql": lb.NonPromQL, "unparseable": lb.Unparseable,
		} {
			if err := requireStatedReasons(a, "lookback "+name, list); err != nil {
				return err
			}
		}
		return nil
	})
}

// thenRetentionCoversLookback asserts the retention cerberus would provision
// reaches back at least as far as the corpus needs, per signal. Both sides are
// MEASURED — the runway from the corpus, the retention from the rendered DDL —
// so there is no threshold to tune.
//
// A signal whose queries need a runway but whose rendered schema provisions no
// TTL fails rather than passing as "unbounded": an unset retention makes this
// assertion true of every corpus, which is the vacuous branch, and the scenario
// names the retention environment it wants precisely so the comparison bites.
func (w *World) thenRetentionCoversLookback() error {
	if !w.retention.read {
		return fmt.Errorf("the configured retention has not been read; the scenario must establish it first")
	}
	needed := map[string]model.Duration{}
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
			if q.Reach > needed[signal] {
				needed[signal] = q.Reach
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if len(needed) == 0 {
		return fmt.Errorf("no signal has a computed runway, so the retention comparison would decide nothing")
	}
	for _, signal := range sortedSignals(needed) {
		want := time.Duration(needed[signal])
		have, ok := w.retention.bySignal[signal]
		if !ok || have == 0 {
			return fmt.Errorf(
				"the %s schema provisions no %s retention, so nothing bounds the %s runway the corpus needs",
				w.schema.caseName, signal, model.Duration(want),
			)
		}
		if have < want {
			return fmt.Errorf("the %s schema provisions %s of %s retention, short of the %s the corpus reads back",
				w.schema.caseName, model.Duration(have), signal, model.Duration(want))
		}
	}
	return nil
}

// sortedSignals lists the signals a runway was computed for, in a deterministic
// order so a failure always names the same one first.
func sortedSignals(in map[string]model.Duration) []string {
	out := make([]string, 0, len(in))
	for s := range in {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// eachLookback runs fn over every tagged archetype's computed runway, failing
// when a When never computed one.
func (w *World) eachLookback(fn func(archetype string, lb migrate.Lookback) error) error {
	if err := w.requireArchetypes("the lookback assertion"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		lb, ok := w.lookback[a]
		if !ok {
			return fmt.Errorf("archetype %s has no computed lookback; the scenario must compute one first", a)
		}
		if err := fn(a, lb); err != nil {
			return err
		}
	}
	return nil
}
