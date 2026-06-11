package surfaceparity

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Verdict is a single backend's posture on a probe expression.
type Verdict string

const (
	// VerdictAccept — the backend lowers/parses+validates the probe
	// without error.
	VerdictAccept Verdict = "accept"
	// VerdictReject — the backend errors on the probe.
	VerdictReject Verdict = "reject"
)

// Classification is the four-way parity grid cell for a symbol.
type Classification string

const (
	// ClassParityAccept — both cerberus and the reference accept.
	ClassParityAccept Classification = "parity-accept"
	// ClassParityReject — both reject.
	ClassParityReject Classification = "parity-reject"
	// ClassWrongReject — cerberus rejects, reference accepts. The
	// coverage gap this layer exists to surface.
	ClassWrongReject Classification = "wrong-reject"
	// ClassWrongAccept — cerberus accepts, reference rejects. A
	// correctness risk.
	ClassWrongAccept Classification = "wrong-accept"
)

// classify derives the parity cell from the two verdicts.
func classify(cerberus, reference Verdict) Classification {
	switch {
	case cerberus == VerdictAccept && reference == VerdictAccept:
		return ClassParityAccept
	case cerberus == VerdictReject && reference == VerdictReject:
		return ClassParityReject
	case cerberus == VerdictReject && reference == VerdictAccept:
		return ClassWrongReject
	default:
		return ClassWrongAccept
	}
}

// Entry is one grammar symbol probed through both backends.
type Entry struct {
	// Head is "promql" / "logql" / "traceql".
	Head string `json:"head"`
	// Symbol is the stable grammar-symbol key: the upstream parser's
	// own identifier for the function / aggregator / op / intrinsic
	// (e.g. "fn:acos", "agg:topk", "op:atan2", "range:rate",
	// "intrinsic:span:duration", "metric:quantile_over_time"). Greppable
	// straight back to the parser symbol table.
	Symbol string `json:"symbol"`
	// Kind groups symbols for reporting: "function", "aggregator",
	// "binary-op", "range-agg", "vector-agg", "parser-stage",
	// "conv-fn", "label-fn", "intrinsic", "metrics-op", "modifier".
	Kind string `json:"kind"`
	// Probe is the synthesized canonical, domain-aware query that
	// exercises the symbol against real seed metrics/labels/attributes.
	Probe string `json:"probe"`
	// Cerberus is cerberus's verdict on Probe (parse → lower → optimize
	// → emit).
	Cerberus Verdict `json:"cerberus"`
	// Reference is the reference backend's modelled verdict on Probe.
	Reference Verdict `json:"reference"`
	// Class is the four-way classification derived from the two
	// verdicts.
	Class Classification `json:"class"`
	// CerberusError is the cerberus rejection error (empty when
	// cerberus accepts). Recorded so a wrong-reject entry carries the
	// concrete failure a burndown fixes.
	CerberusError string `json:"cerberus_error,omitempty"`
	// ReferenceUnresolved flags a symbol whose reference verdict could
	// not be determined in-process (needs a compat-container diff). The
	// ratchet excludes these from the wrong-reject / wrong-accept sets
	// so an undeterminable verdict can't masquerade as a clean parity.
	ReferenceUnresolved bool `json:"reference_unresolved,omitempty"`
	// Note carries any extra context (e.g. why reference is
	// unresolved, or a domain caveat on the probe).
	Note string `json:"note,omitempty"`
}

// Inventory is the checked-in JSON artifact
// (test/surface-parity/inventory.json).
type Inventory struct {
	Source  string  `json:"source"`
	Entries []Entry `json:"entries"`
}

const inventorySource = "in-process probe of the three upstream parser symbol tables " +
	"(prometheus parser.Functions/aggregators/ops, loki syntax.Op* consts, tempo " +
	"intrinsic + metrics-op enums); cerberus verdict = parse→fold→lower→optimize→emit, " +
	"reference verdict = parser experimental flags (promql) / ParseExpr+validate (logql) " +
	"/ Parse+Validate (traceql); generated + pinned by test/surface-parity"

// Generate probes every symbol of every head and assembles the
// inventory. The cerberus and reference verdicts are recomputed from
// scratch each run, so the artifact is a pure function of the parser
// symbol tables + the cerberus lowering — no curated fields survive
// across regenerations (unlike the rejection-parity catalogue, every
// field here is mechanically derived).
func Generate() (*Inventory, error) {
	inv := &Inventory{Source: inventorySource}
	for _, head := range []string{"promql", "logql", "traceql"} {
		entries, err := probeHead(head)
		if err != nil {
			return nil, fmt.Errorf("probe %s: %w", head, err)
		}
		inv.Entries = append(inv.Entries, entries...)
	}
	sort.SliceStable(inv.Entries, func(i, j int) bool {
		a, b := inv.Entries[i], inv.Entries[j]
		if a.Head != b.Head {
			return headOrder(a.Head) < headOrder(b.Head)
		}
		return a.Symbol < b.Symbol
	})
	return inv, nil
}

func headOrder(head string) int {
	switch head {
	case "promql":
		return 0
	case "logql":
		return 1
	case "traceql":
		return 2
	}
	return 3
}

// probeHead dispatches to the per-head prober.
func probeHead(head string) ([]Entry, error) {
	switch head {
	case "promql":
		return probePromQL()
	case "logql":
		return probeLogQL()
	case "traceql":
		return probeTraceQL()
	}
	return nil, fmt.Errorf("unknown head %q", head)
}

// LoadInventory reads + parses the checked-in artifact.
func LoadInventory(path string) (*Inventory, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // repo-relative artifact path
	if err != nil {
		return nil, err
	}
	var inv Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &inv, nil
}

// MarshalInventory renders the canonical on-disk JSON form (2-space
// indent + trailing newline) so the regenerate-and-diff test compares
// byte-for-byte. Mirrors test/rejection-parity.MarshalCatalogue.
func MarshalInventory(inv *Inventory) ([]byte, error) {
	b, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// WrongRejections returns the wrong-reject entries for a head, in
// inventory order, excluding reference-unresolved symbols.
func (inv *Inventory) WrongRejections(head string) []Entry {
	return inv.byClass(head, ClassWrongReject)
}

// WrongAccepts returns the wrong-accept entries for a head.
func (inv *Inventory) WrongAccepts(head string) []Entry {
	return inv.byClass(head, ClassWrongAccept)
}

func (inv *Inventory) byClass(head string, class Classification) []Entry {
	var out []Entry
	for _, e := range inv.Entries {
		if e.Head == head && e.Class == class && !e.ReferenceUnresolved {
			out = append(out, e)
		}
	}
	return out
}

// SymbolKeys returns the sorted symbol keys of the given entries.
func SymbolKeys(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Symbol
	}
	sort.Strings(out)
	return out
}
