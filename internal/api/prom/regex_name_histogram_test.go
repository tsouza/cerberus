package prom

import (
	"context"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestUnpinnedMetricNameMatchers pins which selectors need synthetic-name
// resolution. The classification is the whole fix in miniature: an equality
// `__name__` is already routed by the lowering, everything else speaks about
// names the stored column does not hold.
func TestUnpinnedMetricNameMatchers(t *testing.T) {
	t.Parallel()

	parser := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name     string
		matcher  string
		wantOK   bool
		wantName int
		wantRest int
	}{
		{name: "regex_name", matcher: `{__name__=~".*lat.*"}`, wantOK: true, wantName: 1},
		{
			name:     "regex_name_with_attribute",
			matcher:  `{__name__=~".*lat.*",route="/a"}`,
			wantOK:   true,
			wantName: 1,
			wantRest: 1,
		},
		// A negated matcher alone is empty-matching, which PromQL rejects,
		// so the negated shape always arrives with a companion predicate.
		{
			name:     "negated_regex_name",
			matcher:  `{__name__!~".*lat.*",route="/a"}`,
			wantOK:   true,
			wantName: 1,
			wantRest: 1,
		},
		{
			name:     "two_regex_name_matchers",
			matcher:  `{__name__=~"synth.*",__name__!~".*total"}`,
			wantOK:   true,
			wantName: 2,
		},
		// An equality pin — the lowering strips the companion suffix and
		// reads the histogram row directly, so re-deriving it here would
		// only duplicate arms.
		{name: "equality_name", matcher: `{__name__="synth_latency_seconds_sum"}`},
		{name: "bare_name", matcher: `synth_latency_seconds_sum`},
		// Mixed: the equality already pins the name; the regex narrows it.
		{name: "equality_and_regex", matcher: `{__name__="synth_up",__name__=~"synth.*"}`},
		// No `__name__` matcher at all: selects by attribute, no name set
		// to resolve.
		{name: "attributes_only", matcher: `{route="/a"}`},
		{name: "parse_error", matcher: `((not_a_selector`},
		{name: "not_a_single_selector", matcher: `sum(up)`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			nameMatchers, other, ok := unpinnedMetricNameMatchers(parser, tc.matcher)
			if ok != tc.wantOK {
				t.Fatalf("unpinnedMetricNameMatchers(%q) ok=%v, want %v", tc.matcher, ok, tc.wantOK)
			}
			if len(nameMatchers) != tc.wantName {
				t.Errorf("name matchers: got %d, want %d", len(nameMatchers), tc.wantName)
			}
			if len(other) != tc.wantRest {
				t.Errorf("other matchers: got %d, want %d", len(other), tc.wantRest)
			}
		})
	}
}

// TestHistogramSyntheticVariants pins the rewrite: every emitted variant pins
// `__name__` to an EQUALITY (the shape the lowering can route, and the shape
// that prunes on the metric tables' primary-key prefix), carries the
// non-name matchers through unchanged, and names only synthetic series the
// matcher set actually accepts.
func TestHistogramSyntheticVariants(t *testing.T) {
	t.Parallel()

	parser := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	metrics := schema.DefaultOTelMetrics()
	bases := []string{"synth_latency_seconds", "synth_size_bytes"}

	cases := []struct {
		name    string
		matcher string
		want    []string
	}{
		{
			name:    "regex_selects_one_family",
			matcher: `{__name__=~".*latency.*"}`,
			want: []string{
				`{__name__="synth_latency_seconds_bucket"}`,
				`{__name__="synth_latency_seconds_count"}`,
				`{__name__="synth_latency_seconds_sum"}`,
			},
		},
		{
			// A regex naming a single companion selects that companion
			// only — the fan-out never widens the user's intent.
			name:    "regex_selects_one_companion",
			matcher: `{__name__=~"synth_latency_seconds_su."}`,
			want:    []string{`{__name__="synth_latency_seconds_sum"}`},
		},
		{
			// Prometheus matchers are fully anchored: the base name is not
			// a series name, and a pattern that only matches the base
			// selects nothing.
			name:    "anchored_base_name_selects_nothing",
			matcher: `{__name__=~"synth_latency_seconds"}`,
			want:    nil,
		},
		{
			name:    "attribute_matcher_preserved",
			matcher: `{__name__=~".*latency.*",route="/a"}`,
			want: []string{
				`{__name__="synth_latency_seconds_bucket",route="/a"}`,
				`{__name__="synth_latency_seconds_count",route="/a"}`,
				`{__name__="synth_latency_seconds_sum",route="/a"}`,
			},
		},
		{
			// `le` is the synthetic label the bucket fan-out produces; it
			// must survive into the pinned variant or the bucket arm
			// silently widens.
			name:    "le_matcher_preserved",
			matcher: `{__name__=~".*latency.*_bucket",le="0.5"}`,
			want:    []string{`{__name__="synth_latency_seconds_bucket",le="0.5"}`},
		},
		{
			name:    "negated_regex_selects_the_complement",
			matcher: `{__name__!~".*latency.*",route="/a"}`,
			want: []string{
				`{__name__="synth_size_bytes_bucket",route="/a"}`,
				`{__name__="synth_size_bytes_count",route="/a"}`,
				`{__name__="synth_size_bytes_sum",route="/a"}`,
			},
		},
		{
			name:    "conjunction_of_name_matchers",
			matcher: `{__name__=~"synth.*",__name__!~".*_bucket"}`,
			want: []string{
				`{__name__="synth_latency_seconds_count"}`,
				`{__name__="synth_latency_seconds_sum"}`,
				`{__name__="synth_size_bytes_count"}`,
				`{__name__="synth_size_bytes_sum"}`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			nameMatchers, other, ok := unpinnedMetricNameMatchers(parser, tc.matcher)
			if !ok {
				t.Fatalf("unpinnedMetricNameMatchers(%q) rejected the selector", tc.matcher)
			}
			got := histogramSyntheticVariants(nameMatchers, other, bases, metrics)
			if !stringSlicesEqual(got, tc.want) {
				t.Fatalf("histogramSyntheticVariants(%q)\n got:  %v\n want: %v", tc.matcher, got, tc.want)
			}
			for _, variant := range got {
				if strings.Contains(variant, "=~") {
					t.Errorf("variant %q kept a regex __name__ — it would be re-applied to the stored base name", variant)
				}
				if _, err := parser.ParseExpr(variant); err != nil {
					t.Errorf("variant %q does not parse: %v", variant, err)
				}
			}
		})
	}
}

// TestHistogramSyntheticVariants_NoHistogramTable pins the configuration
// short-circuit: with no classic-histogram table there are no synthetic names
// to resolve against, so the fan-out must stay empty rather than emit variants
// naming a table that does not exist.
func TestHistogramSyntheticVariants_NoHistogramTable(t *testing.T) {
	t.Parallel()

	parser := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	metrics := schema.DefaultOTelMetrics()
	metrics.HistogramTable = ""

	nameMatchers, other, ok := unpinnedMetricNameMatchers(parser, `{__name__=~".+"}`)
	if !ok {
		t.Fatalf("selector rejected")
	}
	if got := histogramSyntheticVariants(nameMatchers, other, []string{"synth_latency_seconds"}, metrics); len(got) != 0 {
		t.Fatalf("no histogram table configured, got variants %v", got)
	}
}

// recordingQuerier answers QueryStrings with a fixed list and records every
// statement it is asked to run, so a test can assert both the shape and the
// COUNT of the queries an endpoint issues.
type recordingQuerier struct {
	strings []string
	sql     []string
}

func (q *recordingQuerier) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return nil, nil
}

func (q *recordingQuerier) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	return nil, nil
}

func (q *recordingQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	q.sql = append(q.sql, sql)
	return q.strings, nil
}

func (q *recordingQuerier) QueryLabelSets(_ context.Context, sql string, _ ...any) ([]map[string]string, error) {
	q.sql = append(q.sql, sql)
	return nil, nil
}

func (q *recordingQuerier) QueryMetricMeta(
	_ context.Context, sql, _ string, _ ...any,
) ([]chclient.MetricMetaRow, error) {
	q.sql = append(q.sql, sql)
	return nil, nil
}

func (q *recordingQuerier) QueryExemplars(context.Context, string, ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

// TestExpandMetadataMatchers_PinnedNameIssuesNoEnumeration pins the cost of the
// common case: a selector whose `__name__` is already pinned to an equality
// needs no name set, so the metadata path must not issue the base-name
// enumeration at all.
func TestExpandMetadataMatchers_PinnedNameIssuesNoEnumeration(t *testing.T) {
	t.Parallel()

	q := &recordingQuerier{strings: []string{"synth_latency_seconds"}}
	h := New(q, schema.DefaultOTelMetrics(), nil)
	start, end := metadataTestWindow()

	if _, err := h.expandMetadataMatchers(context.Background(), []string{`{__name__="up"}`}, start, end, false); err != nil {
		t.Fatalf("expandMetadataMatchers: %v", err)
	}
	if len(q.sql) != 0 {
		t.Fatalf("pinned-name selector issued %d enumeration queries: %v", len(q.sql), q.sql)
	}
}

// TestExpandMetadataMatchers_EnumerationIsWindowBounded pins the scan bound on
// the query the resolution adds. The base-name enumeration reads the histogram
// table, and an unbounded read there would trade a wrong answer for an
// expensive one: it must carry the endpoint's own bounded window (a TimeUnix
// literal), which is what prunes the scan to the requested partitions.
func TestExpandMetadataMatchers_EnumerationIsWindowBounded(t *testing.T) {
	t.Parallel()

	q := &recordingQuerier{strings: []string{"synth_latency_seconds"}}
	h := New(q, schema.DefaultOTelMetrics(), nil)
	start, end := metadataTestWindow()

	variants, err := h.expandMetadataMatchers(
		context.Background(), []string{`{__name__=~".*latency.*"}`}, start, end, false,
	)
	if err != nil {
		t.Fatalf("expandMetadataMatchers: %v", err)
	}
	if len(q.sql) != 1 {
		t.Fatalf("expected exactly one base-name enumeration, got %d: %v", len(q.sql), q.sql)
	}
	enumeration := q.sql[0]
	if !strings.Contains(enumeration, h.Schema.HistogramTable) {
		t.Errorf("enumeration does not read the histogram table: %s", enumeration)
	}
	if !strings.Contains(enumeration, "toDateTime64(") {
		t.Errorf("enumeration carries no TimeUnix bound — unbounded scan: %s", enumeration)
	}
	want := []string{
		`{__name__=~".*latency.*"}`,
		`{__name__="synth_latency_seconds_bucket"}`,
		`{__name__="synth_latency_seconds_count"}`,
		`{__name__="synth_latency_seconds_sum"}`,
	}
	if !stringSlicesEqual(variants, want) {
		t.Fatalf("variants:\n got:  %v\n want: %v", variants, want)
	}
}

// TestExpandMetadataMatchers_FanOutStaysChunked pins the resource bound on the
// resolution itself: the synthetic variants are bounded by the number of
// histogram FAMILIES in the window (times the companion suffixes), and the
// arms they produce are folded into size-capped combined queries rather than
// one query per name. Without the cap a broad regex would either render one
// oversized statement or one round trip per series name.
func TestExpandMetadataMatchers_FanOutStaysChunked(t *testing.T) {
	t.Parallel()

	const families = 300
	bases := make([]string, 0, families)
	for i := range families {
		bases = append(bases, "synth_family_"+string(rune('a'+i%26))+"_"+itoa(i)+"_seconds")
	}
	q := &recordingQuerier{strings: bases}
	h := New(q, schema.DefaultOTelMetrics(), nil)
	start, end := metadataTestWindow()
	ctx := context.Background()

	variants, err := h.expandMetadataMatchers(ctx, []string{`{__name__=~".+"}`}, start, end, false)
	if err != nil {
		t.Fatalf("expandMetadataMatchers: %v", err)
	}
	// One original arm plus one per synthetic name: the fan-out scales with
	// the histogram catalog, not with the rows in the window.
	wantVariants := 1 + families*len(promql.HistogramSyntheticNames(bases[0], h.Schema))
	if len(variants) != wantVariants {
		t.Fatalf("variants: got %d, want %d", len(variants), wantVariants)
	}

	q.sql = nil
	if _, err := h.labelValuesForMatchers(ctx, "route", variants, start, end); err != nil {
		t.Fatalf("labelValuesForMatchers: %v", err)
	}
	if len(q.sql) == 0 {
		t.Fatalf("no statements issued")
	}
	// Chunked, not per-arm: far fewer statements than variants.
	if len(q.sql) >= len(variants) {
		t.Fatalf("fan-out issued %d statements for %d variants — not chunked", len(q.sql), len(variants))
	}
	for i, sql := range q.sql {
		if len(sql) > maxRenderedQueryBytes {
			t.Fatalf("statement %d rendered %d bytes, over the %d-byte cap", i, len(sql), maxRenderedQueryBytes)
		}
		if !strings.Contains(sql, "toDateTime64(") {
			t.Errorf("statement %d carries no TimeUnix bound — unbounded scan: %s", i, sql)
		}
	}
}

// metadataTestWindow returns a fixed bounded metadata window.
func metadataTestWindow() (start, end time.Time) {
	end = time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	return end.Add(-time.Hour), end
}

// itoa renders a non-negative int without pulling strconv into the test's
// import set for a single call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
