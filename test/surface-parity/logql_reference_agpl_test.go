//go:build agpl_oracle

// TestLogQLReferenceVerdictsAreCurrent re-probes every logProbes entry
// through the ACTUAL reference — upstream Loki's AGPL
// lokisyntax.ParseExpr (parse + validate), the same in-process, no-
// compat-container gate the wire path runs — and diffs the result
// against the checked-in logql-reference-verdicts.json artifact that
// logql.go's probeLogQL (default build, no build tag) reads instead of
// calling the AGPL parser directly. This is the ONLY file in this
// package that imports the AGPL grafana/loki syntax package, gated
// behind the `agpl_oracle` build tag so neither the production build
// nor the default `go test ./...` run links it — see #1520.
//
// Set CERBERUS_UPDATE_LOGQL_REFERENCE_VERDICTS=1 to rewrite the
// artifact (the same update-via-env convention as
// TestInventoryIsRegenerable). Run explicitly with:
//
//	go test -tags agpl_oracle -run TestLogQLReferenceVerdictsAreCurrent ./test/surface-parity/
package surfaceparity

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	lokisyntax "github.com/grafana/loki/v3/pkg/logql/syntax"
)

// liveReferenceVerdictLogQL models reference Loki acceptance: the wire
// path accepts exactly what lokisyntax.ParseExpr (parse + validate)
// accepts.
func liveReferenceVerdictLogQL(query string) Verdict {
	if _, err := lokisyntax.ParseExpr(query); err != nil {
		return VerdictReject
	}
	return VerdictAccept
}

func TestLogQLReferenceVerdictsAreCurrent(t *testing.T) {
	t.Parallel()

	live := make(map[string]string, len(logProbes))
	for _, p := range logProbes {
		live[p.symbol] = string(liveReferenceVerdictLogQL(p.probe))
	}
	want, err := json.MarshalIndent(logqlReferenceVerdicts{Verdicts: live}, "", "  ")
	if err != nil {
		t.Fatalf("marshal live verdicts: %v", err)
	}
	want = append(want, '\n')

	if os.Getenv("CERBERUS_UPDATE_LOGQL_REFERENCE_VERDICTS") != "" {
		if err := os.WriteFile(logqlReferenceVerdictsPath, want, 0o644); err != nil {
			t.Fatalf("write %s: %v", logqlReferenceVerdictsPath, err)
		}
		t.Logf("rewrote %s (%d symbols)", logqlReferenceVerdictsPath, len(live))
		return
	}

	got, err := os.ReadFile(logqlReferenceVerdictsPath) //nolint:gosec // repo-relative artifact path
	if err != nil {
		t.Fatalf("read %s (rerun with CERBERUS_UPDATE_LOGQL_REFERENCE_VERDICTS=1 to generate): %v", logqlReferenceVerdictsPath, err)
	}
	var pinned logqlReferenceVerdicts
	if err := json.Unmarshal(got, &pinned); err != nil {
		t.Fatalf("parse %s: %v", logqlReferenceVerdictsPath, err)
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
		t.Errorf("%s is stale relative to the live reference Loki parser for %d symbol(s): %v — "+
			"rerun with CERBERUS_UPDATE_LOGQL_REFERENCE_VERDICTS=1, review the diff, and commit it.",
			logqlReferenceVerdictsPath, len(drifted), drifted)
	}
}
