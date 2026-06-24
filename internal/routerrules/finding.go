package routerrules

import (
	"fmt"
	"sort"
	"strings"
)

// Severity ranks a finding for ordering and operator triage. Higher is more
// urgent. The string forms match the catalog's `severity:` tokens.
type Severity uint8

const (
	SeverityLow Severity = iota
	SeverityMedium
	SeverityHigh
	SeverityCritical
)

func parseSeverity(s string) (Severity, bool) {
	switch s {
	case "low":
		return SeverityLow, true
	case "medium":
		return SeverityMedium, true
	case "high":
		return SeverityHigh, true
	case "critical":
		return SeverityCritical, true
	default:
		return 0, false
	}
}

func (s Severity) String() string {
	switch s {
	case SeverityCritical:
		return "critical"
	case SeverityHigh:
		return "high"
	case SeverityMedium:
		return "medium"
	default:
		return "low"
	}
}

// evidenceExpr is one validated evidence aggregate to compute over matched rows.
// "count" is implicit (always reported as Support) and is not represented here;
// every other entry is one of the closed aggregate-over-column forms.
type evidenceExpr struct {
	raw    string  // the original token, for labeling the report column
	fn     AggFunc // max | avg | min | stddevPop
	column string
}

// parseEvidenceExpr parses an evidence token of the form "fn(column)" against
// the closed aggregate vocabulary and the corpus column allow-list. The bare
// token "count" is handled by the caller (it maps to Support) and must not
// reach here.
func parseEvidenceExpr(tok string) (evidenceExpr, error) {
	open := strings.IndexByte(tok, '(')
	if open <= 0 || !strings.HasSuffix(tok, ")") {
		return evidenceExpr{}, fmt.Errorf("evidence expression %q must be of the form fn(column) or the bare token count", tok)
	}
	fn := tok[:open]
	col := tok[open+1 : len(tok)-1]
	switch AggFunc(fn) {
	case AggMax, AggAvg, AggMin, AggStdDev:
	default:
		return evidenceExpr{}, fmt.Errorf("evidence function %q is not in the allowed set (max, avg, min, stddevPop)", fn)
	}
	if !knownColumn(col) {
		return evidenceExpr{}, fmt.Errorf("evidence column %q is not a corpus column", col)
	}
	if columnKinds[col] != ColumnNumeric {
		return evidenceExpr{}, fmt.Errorf("evidence column %q is not numeric", col)
	}
	return evidenceExpr{raw: tok, fn: AggFunc(fn), column: col}, nil
}

// Finding is one report row: a rule that fired for one shape class, with the
// support count, the resolved threshold actually used, the evidence
// aggregates, and the recommended action. The Message has every {param} and
// {column} placeholder substituted with the runtime-resolved value, so the
// operator sees concrete numbers even though the catalog carried only names.
type Finding struct {
	RuleID    string            `json:"rule_id"`
	Severity  string            `json:"severity"`
	GroupKey  map[string]string `json:"group_key"`
	Support   int64             `json:"support"`
	Evidence  map[string]float64 `json:"evidence,omitempty"`
	Action    string            `json:"action,omitempty"`
	Message   string            `json:"message"`

	severity Severity // for ordering
	groupKeyOrdered []string // group_by-ordered values, for deterministic tie-break
}

// Report is the ordered set of findings from one evaluation.
type Report struct {
	Findings []Finding `json:"findings"`
}

// sortFindings orders findings deterministically: severity descending, then
// rule id, then the group key in group_by order.
func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.severity != b.severity {
			return a.severity > b.severity
		}
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		ak := strings.Join(a.groupKeyOrdered, "\x00")
		bk := strings.Join(b.groupKeyOrdered, "\x00")
		return ak < bk
	})
}
