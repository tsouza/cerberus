//go:build chdb

// The differential that makes /index/volume's two normalisers one rule.
//
// [format.NormalizeLabelMap] decides the label set the response CARRIES;
// [normalizedLabelsFrag] decides the label set ClickHouse GROUPS by. If
// they disagree on a single entry, the endpoint is back to serving two
// vector samples under one label set (#3246) — the exact bug the SQL twin
// exists to close — so the two are pinned against each other over a corpus
// rather than against a transcript of what either one currently answers.
//
// A stub querier can never show this: only an engine that actually
// evaluates the emitted expression can say whether it agrees with the Go
// function.

package loki_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclienttest"
)

// normalizerCorpus is the input side of the differential: every stored
// label-set shape whose rewrite is decided by a distinct clause of
// [format.NormalizeLabelMap] / [format.OTelToPromLabel].
//
// Each entry is a stored map. The expectation is not written down —
// TestNormalizedLabelsFrag_ChDB_MatchesGoNormalizer computes it by
// calling the Go function, so the corpus cannot drift into asserting
// whatever the SQL happens to do.
var normalizerCorpus = []map[string]string{
	// Already Prometheus-shaped: the identity case, and the one the
	// fast path in OTelToPromLabel takes.
	{"job": "api", "pod": "a"},
	// The grammar rewrite: dot, dash, slash and space all become `_`.
	{"a.b": "1"},
	{"k8s.pod.name": "p", "http.request.method": "GET"},
	{"a-b": "1", "a/b": "2", "a b": "3"},
	// Leading digit: `_` prefix ON TOP of the rewrite.
	{"9lives": "1", "0.5": "2"},
	// Non-ASCII. Go rewrites BYTE-wise, so a two-byte rune becomes
	// TWO underscores — the case a rune-wise regex gets wrong, and the
	// reason the SQL twin walks bytes rather than calling
	// replaceRegexpAll.
	{"aé": "1"},
	{"aé": "1", "a__": "2"},
	{"日": "1"},
	// The collision policy, both branches. Natural form present: the
	// rewrite is DROPPED and the natural value survives.
	{"a.b": "dotted", "a_b": "natural"},
	{"service.name": "otel", "service_name": "prom", "job": "api"},
	// Two rewrites onto one name and NO natural form: the lexically
	// first stored key wins — `-` is 0x2D and `.` is 0x2E, so `a-b`
	// sorts ahead of `a.b` and `dash` is the surviving value.
	{"a-b": "dash", "a.b": "dot"},
	{"a.b": "dot", "a b": "space"},
	// The empty stored key normalises to the empty name, which is
	// dropped outright rather than served as a nameless label.
	{"": "orphan", "job": "api"},
	{"": "orphan"},
	// Empty map: no entries, no keys, nothing to sort.
	{},
	// Values are never touched — only identifiers go through the
	// grammar — including values that look like keys needing a rewrite.
	{"a.b": "v.a.l", "c": ""},
	// Double underscore is legal Prometheus, so `__name__`-shaped keys
	// pass through untouched.
	{"__name__": "up", "_x": "1"},
}

// TestNormalizedLabelsFrag_ChDB_MatchesGoNormalizer runs the emitted
// served-label-set expression against chDB over every corpus entry and
// requires it to answer exactly what [format.NormalizeLabelMap] answers
// for the same input.
//
// It discriminates, and the sharpest witness is the non-ASCII pair.
// Written the obvious way — `replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_')` —
// the SQL answers `{a_:1, a__:2}` for `{aé:1, a__:2}` where Go answers
// `{a__:2}`: ClickHouse compiles the pattern with RE2's default UTF-8
// encoding, so `é` is one match, not two bytes. That is a two-entry map
// against a one-entry map, which is precisely #3246's duplicate label set
// reappearing one layer down. Every other corpus entry is likewise a
// clause of the Go function that a plausible SQL shortcut gets wrong:
// dropping the (dirty, key) sort answers `dotted` where Go answers
// `natural`, and dropping the first-occurrence filter leaves BOTH
// spellings in a duplicate-keyed CH Map, whose surviving entry is
// whichever the array order ends on rather than the one the policy picks.
// Both were measured by ablation, not assumed.
func TestNormalizedLabelsFrag_ChDB_MatchesGoNormalizer(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    ServiceName String,
    ResourceAttributes Map(String, String)
) ENGINE = Memory;`)

	servedSQL, args := loki.NormalizedLabelsSQL()
	if len(args) != 0 {
		t.Fatalf("the served-label-set expression must be shape-only, got %d bound args", len(args))
	}

	// Every entry is run under BOTH stored key orders. A CH Map is an
	// ordered pair of arrays, and OTLP delivers whatever order the
	// producer wrote, so an expression that answered correctly only when
	// the natural spelling happened to come first would be a coin flip in
	// production — and would pass a one-order corpus.
	for _, stored := range normalizerCorpus {
		for _, ascending := range []bool{true, false} {
			runNormalizerCase(t, c, servedSQL, stored, ascending)
		}
	}
}

// runNormalizerCase evaluates the emitted expression over one stored map
// laid out in one key order and compares it with the Go normaliser.
func runNormalizerCase(t *testing.T, c *chclienttest.Client, servedSQL string, stored map[string]string, ascending bool) {
	t.Helper()
	query := fmt.Sprintf(
		"SELECT toJSONString(%s) FROM (SELECT %s AS `%s`)",
		servedSQL, chMapLiteral(stored, ascending), loki.StoredLabelsAlias,
	)
	rows, err := c.QueryStrings(context.Background(), query)
	if err != nil {
		t.Fatalf("stored=%v: %v\n--- sql ---\n%s", stored, err, query)
	}
	if len(rows) != 1 {
		t.Fatalf("stored=%v: expected one row, got %d", stored, len(rows))
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(rows[0]), &got); err != nil {
		t.Fatalf("stored=%v: decode %q: %v", stored, rows[0], err)
	}
	want := format.NormalizeLabelMap(stored)
	if !sameLabelMap(got, want) {
		t.Errorf("stored=%v (ascending=%v):\n  SQL twin  = %v\n  Go        = %v\n"+
			"the two normalisers must agree entry for entry — /index/volume groups by the "+
			"first and serves the second, so any disagreement is a duplicate label set",
			stored, ascending, got, want)
	}
}

// chMapLiteral renders a stored label set as a CH `map(...)` literal with
// its keys laid out in the requested order.
func chMapLiteral(m map[string]string, ascending bool) string {
	if len(m) == 0 {
		return "CAST([] AS Map(String, String))"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !ascending {
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	}
	parts := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		parts = append(parts, chStringLiteral(k), chStringLiteral(m[k]))
	}
	return "map(" + strings.Join(parts, ", ") + ")"
}

// chStringLiteral quotes one string for a CH literal, escaping the two
// bytes CH's single-quoted form treats specially.
func chStringLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('\'')
	return b.String()
}

// sameLabelMap compares two label sets entry for entry, treating a nil
// map and an empty one as the same thing — NormalizeLabelMap returns its
// nil input unchanged, while the JSON decode of `{}` yields an empty map.
func sameLabelMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
