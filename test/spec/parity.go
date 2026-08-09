// Package spec — parity extension.
//
// This file defines the optional `parity:` section, which opts a TXTAR
// fixture into evaluation against a REAL reference query engine.
//
// # Why this exists
//
// `expected_rows:` is not an oracle. [RunRoundTrip] executes cerberus's
// own emitted SQL against chDB and, under GOLDEN_UPDATE=1, writes the
// result straight back into the fixture (runner.go's Match does not even
// compare in that mode). So a fixture's "expected" answer is whatever
// cerberus most recently produced. If a lowering emits semantically wrong
// SQL, `just update-golden` re-pins the wrong answer and the fixture
// passes forever — the regression is absorbed rather than caught. Two real
// semantic bugs have reached main that way, both eventually caught by the
// live compatibility harness rather than by this corpus.
//
// The `parity:` section is the fix. A fixture carrying one is additionally
// evaluated by the reference engine itself — the actual upstream
// Prometheus evaluator, in-process — over the same seeded data, and the
// two answers are compared.
//
// # Why the section carries a CONTRACT and not an ANSWER
//
// The obvious design is to store the reference engine's answer in a
// section beside `expected_rows:`. That does not work here, and the reason
// is mechanical rather than aesthetic: `just update-golden` grants each
// shard ownership of a whole DIRECTORY (see the `goldens` field in
// .github/scripts/lib/golden-shards.mjs), and its generators rewrite
// whatever sections they please inside it. A stored reference answer would
// sit inside the `promql` shard's blast radius, regenerable by the very
// cerberus-driven generator whose output it exists to check. Protecting it
// would take a reserved-key guard, an input fingerprint, a drift check and
// an engine-version manifest — machinery whose whole purpose is to make a
// stored answer behave like a computed one.
//
// So the answer is computed live, at test time, and nothing is stored.
// There is no artefact for regeneration to overwrite: absorption is not
// detected, it is impossible. GOLDEN_UPDATE=1 has no effect on the parity
// check because that code path has no update branch at all.
//
// What the fixture stores is the CONTRACT — which oracle answers for it,
// through which endpoint, and how much of the answer is in honest scope.
// That is hand-authored, reviewed like any other source line, and is what
// eventually lets the compatibility driver enumerate fixtures instead of
// maintaining a parallel query corpus.
//
// # Section contract
//
//	-- parity --
//	oracle: prometheus
//	endpoint: /api/v1/query
//	scope: full
//
// or, for a TraceQL fixture:
//
//	-- parity --
//	oracle: tempo
//	endpoint: /api/search
//	scope: full
//
// Every key is required and every value comes from a CLOSED vocabulary.
// An unknown key or value is an error, never a silent skip: a typo must
// fail loudly rather than quietly un-enrol a fixture from its oracle.
//
// `scope` exists for one narrow, real case and is deliberately hard to
// abuse — see [ScopeExceptZeroBucket]. It is NOT a tolerance knob and
// carries no numeric field; a fixture is either compared in full or has
// one named, machine-checked axis excluded.
package spec

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// SectionParity is the TXTAR section name a fixture carries to opt into
// reference-engine evaluation.
const SectionParity = "parity"

// Parity vocabularies. Each is closed; [LoadParity] rejects anything else.
const (
	// OraclePrometheus evaluates the fixture with the upstream Prometheus
	// engine (Apache-2.0, resolved from the pinned prometheus fork).
	OraclePrometheus = "prometheus"

	// OracleLoki evaluates the fixture with the upstream Loki engine.
	// AGPLv3, so it runs only under the agpl_oracle build tag.
	OracleLoki = "loki"

	// OracleTempo evaluates the fixture with the upstream Tempo TraceQL
	// engine, including its own nested-set implementation of the
	// structural operators. AGPLv3, so it runs only under the
	// agpl_oracle build tag.
	OracleTempo = "tempo"
)

const (
	// EndpointInstant is Prometheus's instant-query endpoint. The fixture
	// is evaluated at a single instant.
	EndpointInstant = "/api/v1/query"

	// EndpointRange is Prometheus's range-query endpoint. The fixture must
	// also carry a `range_step:` section.
	EndpointRange = "/api/v1/query_range"

	// EndpointSearch is Tempo's trace-search endpoint. The fixture's
	// query is a spanset pipeline and the compared answer is the set of
	// matched spans, not a time series, so no evaluation instant or step
	// participates.
	EndpointSearch = "/api/search"
)

const (
	// ScopeFull compares every value the reference engine returns. This is
	// the default posture and what almost every fixture should carry.
	ScopeFull = "full"

	// ScopeExceptZeroBucket excludes the zero-bucket boundary of an
	// exponential-histogram quantile from the comparison.
	//
	// This is not a tolerance. It records a fact about the data model:
	// internal/schema/otel.go sets ZeroThresholdColumn to the empty string
	// because upstream OTel-CH persists no zero-threshold column at all. A
	// reference histogram reconstructed from ClickHouse rows must
	// therefore INVENT a zero threshold, and the only honest value to
	// invent is 0 — which is exactly what cerberus's own native-quantile
	// emitter assumes. Comparing that axis would compare cerberus's
	// assumption against itself, which is the hollow green this whole
	// mechanism exists to prevent.
	//
	// Fixtures carrying this scope are excluded from every parity coverage
	// figure, so the limitation cannot disappear into a headline number.
	ScopeExceptZeroBucket = "except-zero-bucket"
)

// ParityEval is the evaluation window a TIME-SERIES oracle needs: the
// instant (or range) at which the reference engine is asked for an answer.
//
// It is the zero value for the trace-search oracles. A TraceQL spanset
// query has no evaluation instant — it selects spans, and the fixture's
// own `search_window:` section is what bounds the scan — so passing three
// zero positional arguments there would be a lie the signature told. The
// struct lets a caller say "no window applies" by saying nothing.
type ParityEval struct {
	// Start is the instant for an instant query, or the range start.
	Start time.Time

	// End equals Start for an instant query.
	End time.Time

	// Step is zero for an instant query, and the range step otherwise.
	Step time.Duration
}

// IsRange reports whether the evaluation is a range query, which is what
// decides whether output timestamps participate in the comparison.
func (e ParityEval) IsRange() bool { return e.Step > 0 }

// Parity is the parsed `parity:` section.
type Parity struct {
	// Oracle names the reference engine that answers for this fixture.
	Oracle string

	// Endpoint is the query endpoint whose semantics the comparison uses.
	Endpoint string

	// Scope is how much of the reference answer is in honest scope.
	Scope string
}

// ComparesInFull reports whether every returned value participates in the
// comparison. Coverage accounting counts only these fixtures.
func (p *Parity) ComparesInFull() bool { return p.Scope == ScopeFull }

// parityVocabularies is the single source of truth for every accepted key
// and its accepted values. [LoadParity] and the contract test both read
// it, so a new key cannot be added to one without the other.
var parityVocabularies = map[string][]string{
	"oracle":   {OraclePrometheus, OracleLoki, OracleTempo},
	"endpoint": {EndpointInstant, EndpointRange, EndpointSearch},
	"scope":    {ScopeFull, ScopeExceptZeroBucket},
}

// ParityKeys returns the required keys, sorted. Exported for the contract
// test, which asserts the fixture corpus and this vocabulary agree.
func ParityKeys() []string {
	keys := make([]string, 0, len(parityVocabularies))
	for k := range parityVocabularies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ParityValues returns the accepted values for one key, sorted.
func ParityValues(key string) []string {
	vals := append([]string(nil), parityVocabularies[key]...)
	sort.Strings(vals)
	return vals
}

// LoadParity parses a fixture's `parity:` section.
//
// The bool reports whether the fixture opted in at all. A fixture without
// the section is not an error — enrolment is incremental by design, and
// the corpus-wide floor is enforced separately by the contract test rather
// than by failing every unenrolled fixture here.
//
// A fixture WITH the section but with a malformed body IS an error. A
// silent skip on a typo'd key would un-enrol a fixture from its oracle
// while still looking enrolled in the diff, which is the failure mode this
// whole file exists to prevent.
func LoadParity(c *Case) (*Parity, bool, error) {
	body, ok := c.Section(SectionParity)
	if !ok {
		return nil, false, nil
	}

	got := map[string]string{}
	for i, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			return nil, false, fmt.Errorf(
				"%s section line %d: %q is not `key: value`", SectionParity, i+1, line,
			)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)

		accepted, known := parityVocabularies[key]
		if !known {
			return nil, false, fmt.Errorf(
				"%s section: unknown key %q (accepted: %s)",
				SectionParity, key, strings.Join(ParityKeys(), ", "),
			)
		}
		if _, dup := got[key]; dup {
			return nil, false, fmt.Errorf("%s section: key %q appears twice", SectionParity, key)
		}
		if !slices.Contains(accepted, value) {
			return nil, false, fmt.Errorf(
				"%s section: key %q has value %q (accepted: %s)",
				SectionParity, key, value, strings.Join(ParityValues(key), ", "),
			)
		}
		got[key] = value
	}

	for _, key := range ParityKeys() {
		if _, present := got[key]; !present {
			return nil, false, fmt.Errorf("%s section: missing required key %q", SectionParity, key)
		}
	}

	return &Parity{
		Oracle:   got["oracle"],
		Endpoint: got["endpoint"],
		Scope:    got["scope"],
	}, true, nil
}
