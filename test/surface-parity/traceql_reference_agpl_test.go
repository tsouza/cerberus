//go:build agpl_oracle

// TestTraceQLReferenceVerdictsAreCurrent re-probes every
// intrinsic/metrics-op/aggregate probe through the ACTUAL reference —
// upstream Tempo's AGPL tempotraceql.Parse + tempotraceql.Validate, the
// same in-process, no-compat-container gate the wire path runs — and
// diffs the result against the checked-in
// traceql-reference-verdicts.json artifact that traceql.go's
// probeTraceQL (default build, no build tag) reads instead of calling
// the AGPL parser directly. This is the ONLY file in this package that
// imports the AGPL grafana/tempo/pkg/traceql package, gated behind the
// `agpl_oracle` build tag so neither the production build nor the
// default `go test ./...` run links it — see #1520.
//
// Set CERBERUS_UPDATE_TRACEQL_REFERENCE_VERDICTS=1 to rewrite the
// artifact (the same update-via-env convention as
// TestInventoryIsRegenerable). Run explicitly with:
//
//	go test -tags agpl_oracle -run TestTraceQLReferenceVerdictsAreCurrent ./test/surface-parity/
package surfaceparity

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	tempotraceql "github.com/grafana/tempo/pkg/traceql"
)

// liveReferenceVerdictTraceQL models reference Tempo acceptance: the
// wire path parses then validates. tempotraceql.Parse runs the
// optimizing parse (the same the tempo handler uses) and
// tempotraceql.Validate runs the reference's unsupported-feature gate.
func liveReferenceVerdictTraceQL(query string) Verdict {
	expr, err := tempotraceql.Parse(query)
	if err != nil {
		return VerdictReject
	}
	if err := tempotraceql.Validate(expr); err != nil {
		return VerdictReject
	}
	return VerdictAccept
}

func TestTraceQLReferenceVerdictsAreCurrent(t *testing.T) {
	t.Parallel()

	all := make([]tqlProbe, 0, len(tqlIntrinsicProbes)+len(tqlMetricsProbes)+len(tqlAggregateProbes))
	all = append(all, tqlIntrinsicProbes...)
	all = append(all, tqlMetricsProbes...)
	all = append(all, tqlAggregateProbes...)

	live := make(map[string]string, len(all))
	for _, p := range all {
		live[p.symbol] = string(liveReferenceVerdictTraceQL(p.probe))
	}
	want, err := json.MarshalIndent(traceqlReferenceVerdicts{Verdicts: live}, "", "  ")
	if err != nil {
		t.Fatalf("marshal live verdicts: %v", err)
	}
	want = append(want, '\n')

	if os.Getenv("CERBERUS_UPDATE_TRACEQL_REFERENCE_VERDICTS") != "" {
		if err := os.WriteFile(traceqlReferenceVerdictsPath, want, 0o644); err != nil {
			t.Fatalf("write %s: %v", traceqlReferenceVerdictsPath, err)
		}
		t.Logf("rewrote %s (%d symbols)", traceqlReferenceVerdictsPath, len(live))
		return
	}

	got, err := os.ReadFile(traceqlReferenceVerdictsPath) //nolint:gosec // repo-relative artifact path
	if err != nil {
		t.Fatalf("read %s (rerun with CERBERUS_UPDATE_TRACEQL_REFERENCE_VERDICTS=1 to generate): %v", traceqlReferenceVerdictsPath, err)
	}
	var pinned traceqlReferenceVerdicts
	if err := json.Unmarshal(got, &pinned); err != nil {
		t.Fatalf("parse %s: %v", traceqlReferenceVerdictsPath, err)
	}

	var drifted []string
	for symbol, liveVerdict := range live {
		if pinnedVerdict, ok := pinned.Verdicts[symbol]; !ok || pinnedVerdict != liveVerdict {
			drifted = append(drifted, symbol)
		}
	}
	for symbol := range pinned.Verdicts {
		if _, ok := live[symbol]; !ok {
			drifted = append(drifted, symbol)
		}
	}
	if len(drifted) > 0 {
		sort.Strings(drifted)
		t.Errorf("%s is stale relative to the live reference Tempo parser for %d symbol(s): %v — "+
			"rerun with CERBERUS_UPDATE_TRACEQL_REFERENCE_VERDICTS=1, review the diff, and commit it.",
			traceqlReferenceVerdictsPath, len(drifted), drifted)
	}
}
