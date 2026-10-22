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
	_, after, ok := strings.Cut(ddl, "ORDER BY (")
	if !ok {
		t.Fatalf("no `ORDER BY (` in the template — the parser is broken, not the DDL:\n%s", ddl)
	}
	rest := after

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

// bareColumnPrefix applies sortingkey.go's documented rule to a parsed
// ORDER BY tuple through SortingKeyPrefixOf, the reduction the boot probe
// applies to a deployed table's sorting_key.
func bareColumnPrefix(elems []string) []string {
	return SortingKeyPrefixOf(strings.Join(elems, ", "))
}

// allMetricsTimestampPrecisions is every metrics timestamp precision schema
// provisioning can render.
var allMetricsTimestampPrecisions = []MetricsTimestampPrecision{MetricsTimestampNanos, MetricsTimestampSeconds}

// metricsTemplates is every metrics table's DDL for one timestamp precision,
// selected through the same SQLTemplatePrecision mapping provisioning uses.
// sortingkey.go states that all five share one ORDER BY and returns a single
// prefix for them; nothing checked that, so the claim is asserted here rather
// than assumed.
func metricsTemplates(p MetricsTimestampPrecision) map[string]string {
	set := sqltemplates.MetricsCreateTables(p.SQLTemplatePrecision())
	return map[string]string{
		"gauge":         set.Gauge,
		"sum":           set.Sum,
		"histogram":     set.Histogram,
		"exp_histogram": set.ExpHistogram,
		"summary":       set.Summary,
	}
}

func TestEveryMetricsTableSharesOneOrderBy(t *testing.T) {
	for _, precision := range allMetricsTimestampPrecisions {
		var first []string
		var firstName string
		for name, ddl := range metricsTemplates(precision) {
			got := orderByTuple(t, ddl)
			if first == nil {
				first, firstName = got, name
				continue
			}
			if !slices.Equal(got, first) {
				t.Errorf("%s: metrics ORDER BY differs: %s=%v vs %s=%v — SortingKeyPrefix returns ONE prefix "+
					"for all five metrics tables, which is only sound while they agree", precision, name, got, firstName, first)
			}
		}
		if len(first) == 0 {
			t.Fatalf("%s: parsed no metrics ORDER BY elements — the parser is broken, not the DDL", precision)
		}
	}
}

// TestMetricsSortingKeyPrefix_MatchesUpstreamDDL derives the bare-column
// prefix from the metrics DDL of every precision: SortingKeyPrefix answers
// for whichever of them a deployment was provisioned with, so both have to
// agree with it.
func TestMetricsSortingKeyPrefix_MatchesUpstreamDDL(t *testing.T) {
	for _, precision := range allMetricsTimestampPrecisions {
		want := bareColumnPrefix(orderByTuple(t, metricsTemplates(precision)["gauge"]))
		if len(want) == 0 {
			t.Fatalf("%s: derived an empty metrics prefix from the DDL — the deriver is broken", precision)
		}
		if got := DefaultOTelMetrics().SortingKeyPrefix(); !slices.Equal(got, want) {
			t.Errorf("metrics SortingKeyPrefix = %v; the pinned exporter's %s ORDER BY gives %v", got, precision, want)
		}
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
	if len(bareColumnPrefix(elems[:1])) != 0 {
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

// TestSortingKeyPrefixOf pins the reduction of a system.tables sorting_key
// to its leading bare-column run, over the keys cerberus provisions and the
// stock exporter keys a deployment may carry instead.
func TestSortingKeyPrefixOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, key string
		want      []string
	}{
		{
			"cerberus metrics", "MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix)",
			[]string{"MetricName", "Attributes", "ServiceName"},
		},
		{
			"stock metrics before v0.157", "ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix)",
			[]string{"ServiceName", "MetricName", "Attributes"},
		},
		{
			"stock metrics from v0.157", "ServiceName, MetricName, toStartOfHour(TimeUnix), cityHash64(Attributes), TimeUnix",
			[]string{"ServiceName", "MetricName"},
		},
		{"function-led key", "toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp", []string{}},
		{"comma inside a call does not split", "MetricName, tuple(a, b), ServiceName", []string{"MetricName"}},
		{"back-quoted identifier", "`service.name`, MetricName", []string{"service.name", "MetricName"}},
		{"expression is not bare", "MetricName, ServiceName || 'x'", []string{"MetricName"}},
		{"leading digit is not an identifier", "1, MetricName", []string{}},
		{"an expression ending in a call ends the run", "MetricName, x + f(TimeUnix), ServiceName", []string{"MetricName"}},
		{"a malformed back-quoted name ends the run", "MetricName, `a`b`", []string{"MetricName"}},
		{"an empty element ends the run", "MetricName, , ServiceName", []string{"MetricName"}},
		{"no sorting key", "", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SortingKeyPrefixOf(tc.key)
			if got == nil || !slices.Equal(got, tc.want) {
				t.Errorf("SortingKeyPrefixOf(%q) = %#v, want %#v", tc.key, got, tc.want)
			}
		})
	}
}

// TestSortingKeyColumnsOf pins the reduction of a system.tables sorting_key
// to the columns it sorts by: each element that reads one column through
// single-argument calls contributes it once, and the run ends at the first
// element that reads none or several.
func TestSortingKeyColumnsOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, key string
		want      []string
	}{
		{
			"cerberus metrics", "MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix)",
			[]string{"MetricName", "Attributes", "ServiceName", "TimeUnix"},
		},
		{
			"stock metrics before v0.157", "ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix)",
			[]string{"ServiceName", "MetricName", "Attributes", "TimeUnix"},
		},
		{
			"stock metrics from v0.157", "ServiceName, MetricName, toStartOfHour(TimeUnix), cityHash64(Attributes), TimeUnix",
			[]string{"ServiceName", "MetricName", "TimeUnix", "Attributes"},
		},
		{"function-led key", "toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp", []string{"Timestamp", "ServiceName"}},
		{"nested single-argument calls", "toDate(toDateTime(Timestamp)), ServiceName", []string{"Timestamp", "ServiceName"}},
		{"back-quoted identifier", "`service.name`, toDateTime(`ts`)", []string{"service.name", "ts"}},
		{"a two-argument call ends the run", "MetricName, toStartOfInterval(TimeUnix, toIntervalHour(1)), ServiceName", []string{"MetricName"}},
		{"a multi-column tuple ends the run", "MetricName, tuple(a, b), ServiceName", []string{"MetricName"}},
		{"an expression ends the run", "MetricName, ServiceName || 'x', TimeUnix", []string{"MetricName"}},
		{"a literal argument ends the run", "MetricName, toDate('2026-01-01'), TimeUnix", []string{"MetricName"}},
		{"an expression ending in a call ends the run", "MetricName, x + f(TimeUnix), ServiceName", []string{"MetricName"}},
		{"a malformed back-quoted name ends the run", "MetricName, `a`b`", []string{"MetricName"}},
		{"an empty element ends the run", "MetricName, , ServiceName", []string{"MetricName"}},
		{"no sorting key", "", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SortingKeyColumnsOf(tc.key)
			if got == nil || !slices.Equal(got, tc.want) {
				t.Errorf("SortingKeyColumnsOf(%q) = %#v, want %#v", tc.key, got, tc.want)
			}
		})
	}
}
