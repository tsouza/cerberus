package schema

import (
	"slices"
	"strings"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/sqltemplates"
)

// The bare-column sorting-key prefixes pin the OTel-CH ORDER BY tuples the
// optimize_aggregation_in_order eligibility check relies on. If an upstream
// exporter bump reorders a sort key, this test fails loudly rather than the
// eligibility check silently stamping (or missing) the setting.
//
// That is what the comment always promised, and until #3182 it was not what
// the test did: it compared SortingKeyPrefix() — a hand-written literal over
// this package's own column-name fields — against a second hand-written
// literal in the test file. Two Go constants agreeing with each other. An
// upstream reorder of `ORDER BY (MetricName, Attributes, ServiceName, …)` to
// any other order changed nothing either side could observe, so the test
// passed while the eligibility check began stamping the setting on a GROUP BY
// that is no longer a sort-key prefix.
//
// The tuples are now PARSED out of the pinned exporter's own DDL templates —
// the same `sqltemplates` internal/schema/ddl renders the real tables from —
// and the bare-column prefix is re-derived from them by the rule sortingkey.go
// documents. A reorder in a `go.mod` bump fails here.

// orderByTuple extracts the top-level elements of a template's `ORDER BY (…)`
// tuple, splitting on commas at paren depth zero so a function-wrapped element
// stays one element.
func orderByTuple(t *testing.T, ddl string) []string {
	t.Helper()
	idx := strings.Index(ddl, "ORDER BY (")
	if idx < 0 {
		t.Fatalf("no `ORDER BY (` in the template — the parser is broken, not the DDL:\n%s", ddl)
	}
	rest := ddl[idx+len("ORDER BY ("):]

	depth := 0
	var elems []string
	var cur strings.Builder
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c == '(':
			depth++
			cur.WriteByte(c)
		case c == ')' && depth > 0:
			depth--
			cur.WriteByte(c)
		case c == ')' && depth == 0:
			elems = append(elems, strings.TrimSpace(cur.String()))
			if len(elems) == 0 {
				t.Fatalf("empty ORDER BY tuple parsed from:\n%s", ddl)
			}
			return elems
		case c == ',' && depth == 0:
			elems = append(elems, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	t.Fatalf("unterminated ORDER BY tuple in:\n%s", ddl)
	return nil
}

// bareColumnPrefix applies sortingkey.go's documented rule: the LEADING run of
// plain column references. A function-wrapped element terminates the prefix,
// because optimize_aggregation_in_order can only exploit a GROUP BY expressed
// over the same bare columns.
func bareColumnPrefix(elems []string) []string {
	out := []string{}
	for _, e := range elems {
		if !isBareIdentifier(e) {
			return out
		}
		out = append(out, e)
	}
	return out
}

func isBareIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		// A digit is legal in an identifier but never leads one.
		digit := c >= '0' && c <= '9' && i > 0
		if alpha || digit {
			continue
		}
		return false
	}
	return true
}

// metricsTemplates is every metrics table's DDL. sortingkey.go states that all
// five share one ORDER BY and returns a single prefix for them; nothing checked
// that, so the claim is asserted here rather than assumed.
func metricsTemplates() map[string]string {
	return map[string]string{
		"gauge":         sqltemplates.MetricsGaugeCreateTable,
		"sum":           sqltemplates.MetricsSumCreateTable,
		"histogram":     sqltemplates.MetricsHistogramCreateTable,
		"exp_histogram": sqltemplates.MetricsExpHistogramCreateTable,
		"summary":       sqltemplates.MetricsSummaryCreateTable,
	}
}

func TestEveryMetricsTableSharesOneOrderBy(t *testing.T) {
	var first []string
	var firstName string
	for name, ddl := range metricsTemplates() {
		got := orderByTuple(t, ddl)
		if first == nil {
			first, firstName = got, name
			continue
		}
		if !slices.Equal(got, first) {
			t.Errorf("metrics ORDER BY differs: %s=%v vs %s=%v — SortingKeyPrefix returns ONE prefix "+
				"for all five metrics tables, which is only sound while they agree", name, got, firstName, first)
		}
	}
	if len(first) == 0 {
		t.Fatal("parsed no metrics ORDER BY elements — the parser is broken, not the DDL")
	}
}

func TestMetricsSortingKeyPrefix_MatchesUpstreamDDL(t *testing.T) {
	want := bareColumnPrefix(orderByTuple(t, sqltemplates.MetricsGaugeCreateTable))
	if len(want) == 0 {
		t.Fatal("derived an empty metrics prefix from the DDL — the deriver is broken")
	}
	if got := DefaultOTelMetrics().SortingKeyPrefix(); !slices.Equal(got, want) {
		t.Errorf("metrics SortingKeyPrefix = %v; the pinned exporter's ORDER BY gives %v", got, want)
	}
}

func TestTracesSortingKeyPrefix_MatchesUpstreamDDL(t *testing.T) {
	want := bareColumnPrefix(orderByTuple(t, sqltemplates.TracesCreateTable))
	if len(want) == 0 {
		t.Fatal("derived an empty traces prefix from the DDL — the deriver is broken")
	}
	if got := DefaultOTelTraces().SortingKeyPrefix(); !slices.Equal(got, want) {
		t.Errorf("traces SortingKeyPrefix = %v; the pinned exporter's ORDER BY gives %v", got, want)
	}
}

// Logs lead with a function-wrapped key element, so there is no bare-column
// prefix: optimize_aggregation_in_order is never eligible for logs. Asserted
// against the DDL, so it holds because the FIRST element is
// toStartOfFiveMinutes(Timestamp) and not because a Go function returns nil.
func TestLogsSortingKeyPrefix_EmptyBecauseTheDDLLeadsWithAFunction(t *testing.T) {
	elems := orderByTuple(t, sqltemplates.LogsCreateTable)
	if len(elems) == 0 {
		t.Fatal("parsed no logs ORDER BY elements — the parser is broken, not the DDL")
	}
	if isBareIdentifier(elems[0]) {
		t.Fatalf("the logs ORDER BY now LEADS with a bare column (%q); SortingKeyPrefix returns "+
			"nil unconditionally, so optimize_aggregation_in_order would stay ineligible for logs "+
			"that have become eligible", elems[0])
	}
	if got := DefaultOTelLogs().SortingKeyPrefix(); len(got) != 0 {
		t.Errorf("logs SortingKeyPrefix = %v; want empty", got)
	}
}

// SortingKeyPrefix honours overridden column names so a custom schema's sort
// key is reported with the operator's column names, not the OTel defaults.
func TestMetricsSortingKeyPrefix_HonoursColumnOverrides(t *testing.T) {
	m := DefaultOTelMetrics()
	m.MetricNameColumn = "Name"
	m.AttributesColumn = "Labels"
	m.ServiceNameColumn = "Svc"
	got := m.SortingKeyPrefix()
	want := []string{"Name", "Labels", "Svc"}
	if !slices.Equal(got, want) {
		t.Errorf("overridden metrics SortingKeyPrefix = %v; want %v", got, want)
	}
}
