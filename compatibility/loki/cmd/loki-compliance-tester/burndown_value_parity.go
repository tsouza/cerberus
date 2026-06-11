package main

// Value-level differential pass for the LogQL wrong-rejection
// burndown (mirrors the detected_fields pass pattern in
// detected_fields.go).
//
// The vendored loki-bench corpus exercises none of the operator
// shapes cerberus historically 422'd — vector set ops, the `bool`
// modifier on arithmetic, ip() line/label filters, `|>` / `!>`
// pattern filters, first/last_over_time, absent_over_time,
// topk/bottomk, sort/sort_desc — so flipping those rejections into
// lowerings would otherwise land with status-class parity only (the
// rejection-parity driver) and zero VALUE coverage. This pass closes
// that gap: a fixed set of queries over the seeded dataset runs
// against BOTH backends through the same compareOne → diffTyped
// pipeline as the corpus cases, and any value drift is a real
// cerberus bug. No allow-list; the results join the same report +
// score.
//
// Query-shape provenance: every template below is a minimal variation
// of a corpus shape that already passes at 100% (`sum by
// (detected_level) (count_over_time(...))`, the `| logfmt | duration
// != "" | unwrap duration(duration)` extraction), with only the
// burned-down operator swapped in — so a diff implicates the new
// operator, not the carrier query.
//
// Determinism notes:
//
//   - Set-op / binop legs share the same row base (count vs bytes over
//     the SAME selector), so both legs are sample-dense across the
//     step grid and the per-step reference semantics agree with the
//     signature-based SQL shape on every anchor.
//   - topk/bottomk use K=10 ≥ the seeded detected_level cardinality:
//     value parity is asserted for the full set; the K-cut itself is
//     pinned by the chdb round-trip fixtures (test/spec/logql/topk*),
//     because reference Loki breaks value ties by heap insertion
//     order, which no SQL ORDER BY tie-break can reproduce.
//   - sort/sort_desc assert value parity; the comparator normalises
//     result order on both sides (normaliseTypedResult), so output
//     ORDERING is deliberately out of scope here.

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/grafana/loki/v3/pkg/logproto"

	bench "github.com/tsouza/cerberus/compatibility/loki/upstream/loki-bench"
)

// burndownSource is the report `testCase.source` for this pass.
const burndownSource = "cerberus/wrong-rejection-burndown"

// burndownWindow is the slice of the seeded 24h dataset the pass
// queries: [TimeRange.Start, TimeRange.Start + 1h] at 1m steps — 61
// anchors, every one of which has data for the per-minute seeded
// services.
const (
	burndownLength = time.Hour
	burndownStep   = time.Minute
)

// compareBurndownValueParity builds and runs the burndown cases.
// Selector resolution failures (dataset missing the required service /
// format / unwrappable field) surface as one UnexpectedFailure result
// so the report shows the broken precondition instead of silently
// shrinking the denominator.
func compareBurndownValueParity(c *http.Client, f flags, metadata *bench.DatasetMetadata) []Result {
	cases, err := burndownCases(metadata)
	if err != nil {
		return []Result{{
			TestCase:          TestCase{Source: burndownSource, Description: "burndown case construction"},
			UnexpectedFailure: err.Error(),
		}}
	}
	results := make([]Result, 0, len(cases))
	for _, tc := range cases {
		results = append(results, compareOne(c, f, tc, burndownSource, false /*isInstant*/))
	}
	return results
}

// burndownCases derives the fixed query list from the dataset
// metadata. Two selectors anchor the templates:
//
//   - selJSON — the web-server stream: JSON format, EVERY line carries
//     a `client_ip` IPv4 (see upstream faker.go's web-server
//     generator), lines start with `{` and end with `}`. Drives the
//     ip() and pattern-filter cases.
//   - selDur — the lexicographically-first selector carrying both
//     logfmt format and the `duration` unwrappable field. Drives the
//     first/last_over_time unwrap cases (the corpus' proven
//     `| logfmt | duration != "" | unwrap duration(duration)` shape).
func burndownCases(metadata *bench.DatasetMetadata) ([]bench.TestCase, error) {
	jsonSels := metadata.ByServiceName["web-server"]
	if len(jsonSels) == 0 {
		return nil, fmt.Errorf("burndown: dataset metadata has no web-server selectors (required for ip()/pattern cases)")
	}
	selJSON := jsonSels[0]

	durSels := intersectSorted(metadata.ByFormat[bench.LogFormatLogfmt], metadata.ByUnwrappableField["duration"])
	if len(durSels) == 0 {
		return nil, fmt.Errorf("burndown: dataset metadata has no logfmt selector with an unwrappable `duration` field (required for first/last_over_time cases)")
	}
	selDur := durSels[0]

	start := metadata.TimeRange.Start.UTC()
	end := start.Add(burndownLength)

	type q struct {
		desc  string
		query string
		kind  string // "metric" | "log"
	}
	queries := []q{
		// Vector set ops. Both legs aggregate the SAME selector's rows
		// (count vs bytes), so the legs are equally dense per anchor —
		// `and` keeps every LHS sample with its LHS value (a diff fires
		// if RHS values leak through); `or` / `unless` pit the
		// `by (detected_level)` signatures against the empty-label-set
		// `sum(...)` series so the anti-join arm carries real rows.
		{"set-op and: LHS values survive, signature-matched", fmt.Sprintf(`sum by (detected_level) (count_over_time(%s[5m])) and sum by (detected_level) (bytes_over_time(%s[5m]))`, selJSON, selJSON), "metric"},
		{"set-op or: union with anti-right on disjoint signatures", fmt.Sprintf(`sum by (detected_level) (count_over_time(%s[5m])) or sum (count_over_time(%s[5m]))`, selJSON, selJSON), "metric"},
		{"set-op unless: anti-join on disjoint signatures keeps LHS", fmt.Sprintf(`sum by (detected_level) (count_over_time(%s[5m])) unless sum (count_over_time(%s[5m]))`, selJSON, selJSON), "metric"},
		// `bool` on arithmetic: reference ignores the modifier
		// (MergeBinOp's arithmetic mergers never consult it), so the
		// result must equal the unmodified sum-of-legs.
		{"bool modifier ignored on arithmetic binop", fmt.Sprintf(`sum by (detected_level) (count_over_time(%s[5m])) + bool sum by (detected_level) (count_over_time(%s[5m]))`, selJSON, selJSON), "metric"},
		// first/last_over_time over the corpus' proven unwrap shape.
		{"first_over_time: time-earliest unwrapped value per window", fmt.Sprintf(`sum by (level) (first_over_time(%s | logfmt | duration != "" | unwrap duration(duration) [5m]))`, selDur), "metric"},
		{"last_over_time: time-latest unwrapped value per window", fmt.Sprintf(`sum by (level) (last_over_time(%s | logfmt | duration != "" | unwrap duration(duration) [5m]))`, selDur), "metric"},
		// absent_over_time: the impossible line filter guarantees zero
		// samples in every window, so both backends must synthesise the
		// matcher-derived series with value 1 at every anchor.
		{"absent_over_time: synthesised series on guaranteed absence", fmt.Sprintf(`absent_over_time(%s |= "cerberus-burndown-no-such-token" [5m])`, selJSON), "metric"},
		// topk/bottomk with K ≥ series count (value parity; see the
		// file comment for the tie-break rationale) + sort/sort_desc.
		{"topk: K covers the full series set, values intact", fmt.Sprintf(`topk(10, sum by (detected_level) (count_over_time(%s[5m])))`, selJSON), "metric"},
		{"bottomk: K covers the full series set, values intact", fmt.Sprintf(`bottomk(10, sum by (detected_level) (count_over_time(%s[5m])))`, selJSON), "metric"},
		{"sort: sample set unchanged", fmt.Sprintf(`sort(sum by (detected_level) (count_over_time(%s[5m])))`, selJSON), "metric"},
		{"sort_desc: sample set unchanged", fmt.Sprintf(`sort_desc(sum by (detected_level) (count_over_time(%s[5m])))`, selJSON), "metric"},
		// ip() line + label filters. web-server lines all carry an
		// IPv4, so the match-all CIDR keeps every line and the
		// negations carve real subsets.
		{"ip line filter: match-all CIDR", fmt.Sprintf(`%s |= ip("0.0.0.0/0")`, selJSON), "log"},
		{"ip line filter negated: single-IP", fmt.Sprintf(`%s != ip("255.255.255.255")`, selJSON), "log"},
		{"ip label filter: match-all CIDR over client_ip", fmt.Sprintf(`%s | json | client_ip = ip("0.0.0.0/0")`, selJSON), "log"},
		{"ip label filter negated: CIDR over client_ip", fmt.Sprintf(`%s | json | client_ip != ip("10.0.0.0/8")`, selJSON), "log"},
		// Pattern line filters. `{<_>}` anchors the closing brace at
		// end-of-line (JSON lines all match); `<_>error<_>` requires
		// "error" strictly inside the line.
		{"pattern line filter: anchored JSON envelope", fmt.Sprintf(`%s |> "{<_>}"`, selJSON), "log"},
		{"pattern line filter negated: embedded literal", fmt.Sprintf(`%s !> "<_>error<_>"`, selJSON), "log"},
	}

	cases := make([]bench.TestCase, 0, len(queries))
	for _, def := range queries {
		tc := bench.TestCase{
			Query:     def.query,
			Start:     start,
			End:       end,
			Direction: logproto.FORWARD,
			Source:    burndownSource,
			QueryDesc: def.desc,
			Tags:      []string{"wrong-rejection-burndown"},
		}
		if def.kind == "metric" {
			tc.Step = burndownStep
		} else {
			tc.Direction = logproto.BACKWARD
		}
		cases = append(cases, tc)
	}
	return cases, nil
}

// intersectSorted returns the sorted intersection of two selector
// lists. Sorted output keeps the selector pick deterministic across
// runs (the metadata maps carry slices whose order is generator-
// dependent).
func intersectSorted(a, b []string) []string {
	inB := make(map[string]struct{}, len(b))
	for _, s := range b {
		inB[s] = struct{}{}
	}
	var out []string
	for _, s := range a {
		if _, ok := inB[s]; ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
