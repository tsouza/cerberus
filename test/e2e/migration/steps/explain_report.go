package steps

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The explain report is the operator-facing artifact `cerberus migrate explain`
// writes: a header, one block per query, and a trailing skip list. Parsing it
// back into typed values is what lets the scenario assert on FIELDS (this query
// yielded SQL, that one named a rejecting symbol, this one reads these tables)
// rather than on the presence of a substring in a wall of text.
const (
	explainBlockMarker = "== "
	explainSkipMarker  = "== skipped ("
)

// The field prefixes one query block carries. A query either yields SQL — with
// its tables and its risk lines — or names the symbol that rejected it.
const (
	explainExprPrefix   = "expr:"
	explainSQLPrefix    = "sql:"
	explainTablesPrefix = "tables:"
	explainRiskPrefix   = "risk:"
	explainUnsupPrefix  = "UNSUPPORTED:"
)

// explainHeaderRe captures the header's own tally, so a report whose blocks and
// whose headline disagree fails rather than being read from whichever half the
// assertion happened to look at.
var explainHeaderRe = regexp.MustCompile(`^# (\d+) queries, (\d+) skipped$`)

// explainBlockRe captures a query block's opening line: the kind of corpus
// entry it came from and its provenance.
var explainBlockRe = regexp.MustCompile(`^== \[([^\]]+)\] (.+)$`)

// explainQuery is one query's section of the report, read back as values.
// Exactly one of SQL and Unsupported is ever set: a query the engine emitted
// SQL for, or one it rejected by name.
type explainQuery struct {
	Kind        string
	Source      string
	Expr        string
	SQL         string
	Tables      []string
	Risks       []string
	Unsupported string
}

// explainSkip is one corpus entry the harvest dropped, carried into the report.
type explainSkip struct {
	Source string
	Reason string
}

// explainReport is a whole parsed report: the header's tally, one entry per
// query block, and the skip list.
type explainReport struct {
	HeaderQueries int
	HeaderSkipped int
	Queries       []explainQuery
	Skipped       []explainSkip
}

// parseExplainReport reads the report text back into typed values. Anything it
// does not recognise is an error: silently ignoring an unknown line would let a
// report shape change out from under every assertion built on it.
func parseExplainReport(data []byte) (explainReport, error) {
	var (
		rep     explainReport
		cur     *explainQuery
		inSkips bool
		seen    bool
	)
	flush := func() {
		if cur != nil {
			rep.Queries = append(rep.Queries, *cur)
			cur = nil
		}
	}
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		switch {
		case strings.TrimSpace(line) == "":
			continue

		case strings.HasPrefix(line, explainSkipMarker):
			flush()
			inSkips = true

		case strings.HasPrefix(line, explainBlockMarker):
			flush()
			inSkips = false
			m := explainBlockRe.FindStringSubmatch(line)
			if m == nil {
				return explainReport{}, fmt.Errorf("explain report line %d is an unreadable block header: %q", i+1, line)
			}
			cur = &explainQuery{Kind: m[1], Source: m[2]}

		case strings.HasPrefix(line, "#"):
			if m := explainHeaderRe.FindStringSubmatch(line); m != nil {
				q, err := strconv.Atoi(m[1])
				if err != nil {
					return explainReport{}, fmt.Errorf("explain report line %d: %w", i+1, err)
				}
				s, err := strconv.Atoi(m[2])
				if err != nil {
					return explainReport{}, fmt.Errorf("explain report line %d: %w", i+1, err)
				}
				rep.HeaderQueries, rep.HeaderSkipped, seen = q, s, true
			}

		case inSkips:
			source, reason, ok := strings.Cut(strings.TrimSpace(line), ": ")
			if !ok {
				return explainReport{}, fmt.Errorf("explain report line %d is an unreadable skip entry: %q", i+1, line)
			}
			rep.Skipped = append(rep.Skipped, explainSkip{Source: source, Reason: reason})

		case cur != nil:
			if err := readExplainField(cur, strings.TrimSpace(line)); err != nil {
				return explainReport{}, fmt.Errorf("explain report line %d: %w", i+1, err)
			}

		default:
			return explainReport{}, fmt.Errorf("explain report line %d sits outside every block: %q", i+1, line)
		}
	}
	flush()
	if !seen {
		return explainReport{}, fmt.Errorf("explain report carries no `# N queries, M skipped` header")
	}
	return rep, nil
}

// readExplainField assigns one field line to the block being read.
func readExplainField(q *explainQuery, line string) error {
	switch {
	case strings.HasPrefix(line, explainExprPrefix):
		q.Expr = strings.TrimSpace(strings.TrimPrefix(line, explainExprPrefix))
	case strings.HasPrefix(line, explainSQLPrefix):
		q.SQL = strings.TrimSpace(strings.TrimPrefix(line, explainSQLPrefix))
	case strings.HasPrefix(line, explainTablesPrefix):
		for _, t := range strings.Split(strings.TrimPrefix(line, explainTablesPrefix), ",") {
			if t = strings.TrimSpace(t); t != "" {
				q.Tables = append(q.Tables, t)
			}
		}
	case strings.HasPrefix(line, explainRiskPrefix):
		q.Risks = append(q.Risks, strings.TrimSpace(strings.TrimPrefix(line, explainRiskPrefix)))
	case strings.HasPrefix(line, explainUnsupPrefix):
		q.Unsupported = strings.TrimSpace(strings.TrimPrefix(line, explainUnsupPrefix))
	default:
		return fmt.Errorf("unrecognised field line %q", line)
	}
	return nil
}
