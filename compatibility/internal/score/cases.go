package score

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The per-case sidecar: WHICH cases the harness ran, and which of them
// agreed with the reference backend.
//
// score.Score answers "how many agreed"; a count alone cannot tell a
// swap from a steady state — one case regressing while a different one
// starts passing leaves passed/total untouched. The parity ratchet
// (.github/scripts/compat-ratchet.mjs) therefore gates on the identity
// of every case, and this file is the contract all three harnesses emit
// it through, so the ratchet reads one shape instead of three
// harness-specific report formats.
//
// The file is written next to compat-score.json as compat-cases.json
// and is the input to `heads.<head>.cases` in
// compatibility/parity-baseline.json.

// caseIDOccurrenceSuffix disambiguates two cases that reduce to the same
// base ID (a corpus can legitimately carry the same query twice). The
// suffix is assigned in corpus order, so it is stable across runs and
// unaffected by edits elsewhere in the corpus — inserting a case never
// renumbers an unrelated one.
const caseIDOccurrenceSuffix = " #%d"

// Case is one corpus case reduced to the two facts the parity ratchet
// gates on.
type Case struct {
	// ID identifies the case across runs. Each harness builds it from
	// the case's static corpus identity (query text, endpoint, suite,
	// direction) and never from wall-clock-derived values, so the same
	// corpus yields the same IDs on every run.
	ID string `json:"id"`

	// Passed reports whether the case fully agreed with the reference
	// backend, using that harness's own success predicate — the same
	// one feeding Score.Passed.
	Passed bool `json:"passed"`
}

// CaseSet is the compat-cases.json envelope. Head names the harness so
// the ratchet can reject a score/cases pair that was crossed over
// between heads.
type CaseSet struct {
	Head  string `json:"head"`
	Cases []Case `json:"cases"`
}

// WriteCases writes the per-case sidecar to path.
//
// Cases are emitted sorted by ID so the committed baseline diff shows
// exactly which cases moved rather than reshuffling on every corpus
// edit. IDs that repeat are suffixed in the order given, which keeps
// every case in the file (dropping a duplicate would silently shrink
// the corpus the ratchet gates on).
func WriteCases(path, head string, cases []Case) error {
	if head == "" {
		return fmt.Errorf("write cases %s: head is required", path)
	}
	out, err := normaliseCases(cases)
	if err != nil {
		return fmt.Errorf("write cases %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir cases dir: %w", err)
	}
	data, err := json.MarshalIndent(CaseSet{Head: head, Cases: out}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cases: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write cases: %w", err)
	}
	return nil
}

// normaliseCases disambiguates repeated IDs and sorts the result.
func normaliseCases(cases []Case) ([]Case, error) {
	out := make([]Case, 0, len(cases))
	seen := make(map[string]int, len(cases))
	for _, c := range cases {
		id := strings.TrimSpace(c.ID)
		if id == "" {
			return nil, fmt.Errorf("case %d has an empty ID", len(out))
		}
		seen[id]++
		if n := seen[id]; n > 1 {
			id += fmt.Sprintf(caseIDOccurrenceSuffix, n)
		}
		out = append(out, Case{ID: id, Passed: c.Passed})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	for i := 1; i < len(out); i++ {
		// Reachable only if a case's own ID already looks like another
		// case's occurrence suffix. Erroring keeps the file a true set;
		// silently collapsing the pair would drop a case from the
		// roster the ratchet gates on.
		if out[i].ID == out[i-1].ID {
			return nil, fmt.Errorf("duplicate case ID %q after disambiguation", out[i].ID)
		}
	}
	return out, nil
}
