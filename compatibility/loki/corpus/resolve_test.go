package corpus

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	selA = `{app="a", env="prod"}`
	selB = `{app="b", env="prod"}`
	selC = `{app="c", env="dev"}`
)

var testSpanStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// testMetadata is a three-stream dataset over one day: a and b are
// logfmt with a `duration` field, c is json; only a has an instant
// range bound.
func testMetadata() *Metadata {
	return &Metadata{
		AllSelectors:       []string{selA, selB, selC},
		ByFormat:           map[LogFormat][]string{LogFormatLogfmt: {selA, selB}, LogFormatJSON: {selC}},
		ByUnwrappableField: map[string][]string{"duration": {selA, selB}},
		ByLabelKey:         map[string][]string{"app": {selA, selB, selC}, "env": {selA, selB, selC}},
		ByKeyword:          map[string][]string{"error": {selC}},
		RangesBySelector: map[string]*StreamRanges{
			selA: {MinRange: 5 * time.Minute, MinInstantRange: 10 * time.Minute},
			selB: {MinRange: 90 * time.Second},
			selC: {MinRange: time.Hour},
		},
		TimeRange: Window{Start: testSpanStart, End: testSpanStart.Add(24 * time.Hour)},
		Version:   MetadataVersion,
	}
}

func TestExpand_Fanout(t *testing.T) {
	t.Parallel()
	r := NewResolver(testMetadata(), 1)
	for _, tc := range []struct {
		def  Definition
		want []Direction
		step time.Duration
	}{
		{Definition{Query: "${SELECTOR}", Kind: KindLog, Directions: DirectionsBoth, TimeRange: TimeRange{Length: "1h", Step: "1m"}}, []Direction{Forward, Backward}, 0},
		{Definition{Query: "${SELECTOR}", Kind: KindLog, Directions: DirectionsBackward, TimeRange: TimeRange{Length: "1h"}}, []Direction{Backward}, 0},
		{Definition{Query: "${SELECTOR}", Kind: KindLog, Directions: DirectionsForward, TimeRange: TimeRange{Length: "1h"}}, []Direction{Forward}, 0},
		{Definition{Query: "count_over_time(${SELECTOR}[1m])", Kind: KindMetric, TimeRange: TimeRange{Length: "1h", Step: "30s"}}, []Direction{Forward}, 30 * time.Second},
		{Definition{Query: "${SELECTOR}", Kind: KindLog, TimeRange: TimeRange{Length: "1h"}}, nil, 0},
	} {
		cases, err := r.Expand(tc.def, false)
		if err != nil {
			t.Fatalf("Expand(%+v): %v", tc.def, err)
		}
		var got []Direction
		for _, c := range cases {
			got = append(got, c.Direction)
			if c.Step != tc.step {
				t.Fatalf("Expand(%+v) step = %v, want %v", tc.def, c.Step, tc.step)
			}
			if c.End.Sub(c.Start) != time.Hour {
				t.Fatalf("window %v..%v is not the 1h length", c.Start, c.End)
			}
		}
		if !slices.Equal(got, tc.want) {
			t.Fatalf("Expand(%+v) directions = %v, want %v", tc.def, got, tc.want)
		}
	}
}

func TestExpand_CarriesDefinitionIdentity(t *testing.T) {
	t.Parallel()
	def := Definition{Description: "d", Source: "fast/a.yaml:3", Tags: []string{"t"}, Query: "${SELECTOR}", Kind: KindLog, Directions: DirectionsForward, TimeRange: TimeRange{Length: "1h"}}
	cases, err := NewResolver(testMetadata(), 1).Expand(def, false)
	if err != nil {
		t.Fatal(err)
	}
	c := cases[0]
	if c.Description != "d" || c.Source != "fast/a.yaml:3" || !slices.Equal(c.Tags, []string{"t"}) {
		t.Fatalf("case lost the definition identity: %+v", c)
	}
}

func TestExpand_RequirementsNarrowTheSelector(t *testing.T) {
	t.Parallel()
	for seed := range int64(20) {
		r := NewResolver(testMetadata(), seed)
		got, err := r.resolveQuery("${SELECTOR}", Requirements{LogFormat: "json"}, false)
		if err != nil || got != selC {
			t.Fatalf("seed %d: json stream = %q, %v; want %s", seed, got, err, selC)
		}
		got, err = r.resolveQuery("${SELECTOR}", Requirements{UnwrappableFields: []string{"duration"}}, false)
		if err != nil || (got != selA && got != selB) {
			t.Fatalf("seed %d: duration stream = %q, %v; want a or b", seed, got, err)
		}
		// Only a has an instant range bound, so an instant ${RANGE}
		// query can resolve only to a.
		got, err = r.resolveQuery("${SELECTOR}[${RANGE}]", Requirements{}, true)
		if err != nil || got != selA+"[10m]" {
			t.Fatalf("seed %d: instant range query = %q, %v; want %s[10m]", seed, got, err, selA)
		}
	}
}

func TestExpand_DrawsEveryEligibleStream(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for seed := range int64(64) {
		got, err := NewResolver(testMetadata(), seed).resolveQuery("${SELECTOR}", Requirements{}, false)
		if err != nil {
			t.Fatal(err)
		}
		seen[got] = true
	}
	if len(seen) != 3 {
		t.Fatalf("64 seeds drew %d distinct streams, want all 3: %v", len(seen), seen)
	}
}

func TestExpand_LabelPlaceholders(t *testing.T) {
	t.Parallel()
	for seed := range int64(20) {
		r := NewResolver(testMetadata(), seed)
		got, err := r.resolveQuery(`sum by (${LABEL_NAME}) (count_over_time(${SELECTOR} | ${LABEL_NAME}="${LABEL_VALUE}" [1m]))`, Requirements{Labels: []string{"env"}, LogFormat: "json"}, false)
		if err != nil {
			t.Fatal(err)
		}
		want := `sum by (env) (count_over_time(` + selC + ` | env="dev" [1m]))`
		if got != want {
			t.Fatalf("seed %d: %s, want the required label env preferred: %s", seed, got, want)
		}
	}
	// With no required label present, any equality matcher may be drawn.
	got, err := NewResolver(testMetadata(), 3).resolveQuery(`${SELECTOR} | ${LABEL_NAME}="${LABEL_VALUE}"`, Requirements{LogFormat: "json"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != selC+` | app="c"` && got != selC+` | env="dev"` {
		t.Fatalf("label draw = %s", got)
	}
}

func TestExpand_RangeRendering(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		5 * time.Minute:                 "5m",
		90 * time.Second:                "1m30s",
		time.Hour:                       "1h0m",
		30 * time.Second:                "30s",
		time.Hour + 30*time.Minute:      "1h30m",
		2*time.Hour + 15*time.Second:    "2h0m15s",
		1500 * time.Millisecond:         "1.5s",
		10*time.Minute + 5*time.Second:  "10m5s",
		20*time.Minute + 50*time.Second: "20m50s",
	} {
		if got := logqlDuration(d); got != want {
			t.Fatalf("logqlDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestExpand_Errors(t *testing.T) {
	t.Parallel()
	md := testMetadata()
	md.RangesBySelector[selC] = &StreamRanges{}
	for name, tc := range map[string]struct {
		query string
		req   Requirements
		want  string
	}{
		"unknown format":       {"${SELECTOR}", Requirements{LogFormat: "unstructured"}, "log format"},
		"unknown field":        {"${SELECTOR}", Requirements{UnwrappableFields: []string{"bytes"}}, "unwrappable field"},
		"unknown label":        {"${SELECTOR}", Requirements{Labels: []string{"zone"}}, "label"},
		"unknown keyword":      {"${SELECTOR}", Requirements{Keywords: []string{"panic"}}, "keyword"},
		"label without stream": {"${LABEL_NAME}", Requirements{}, "${SELECTOR}"},
		"range without stream": {"x[${RANGE}]", Requirements{}, "${SELECTOR}"},
		"no ranged stream":     {"${SELECTOR}[${RANGE}]", Requirements{Keywords: []string{"error"}}, "range bound"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := NewResolver(md, 1).resolveQuery(tc.query, tc.req, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resolveQuery(%q) error = %v, want one mentioning %q", tc.query, err, tc.want)
			}
		})
	}
	if _, err := NewResolver(md, 1).Expand(Definition{Query: "${SELECTOR}", Kind: KindLog, Directions: DirectionsBoth, TimeRange: TimeRange{Length: "48h"}}, false); err == nil {
		t.Fatal("a window longer than the dataset span expanded")
	}
}

func TestWindow_StaysInsideTheSpan(t *testing.T) {
	t.Parallel()
	md := testMetadata()
	r := NewResolver(md, 7)
	starts := map[time.Time]bool{}
	for range 50 {
		start, end, err := r.window(time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if start.Before(md.TimeRange.Start) || end.After(md.TimeRange.End) || end.Sub(start) != time.Hour {
			t.Fatalf("window %v..%v escapes the span", start, end)
		}
		starts[start] = true
	}
	if len(starts) < 2 {
		t.Fatal("50 draws produced one window start; the offset is not drawn")
	}
	start, end, err := r.window(24 * time.Hour)
	if err != nil || !start.Equal(md.TimeRange.Start) || !end.Equal(md.TimeRange.End) {
		t.Fatalf("a full-span window = %v..%v, %v; want the span itself", start, end, err)
	}
}

func TestExpand_SameSeedSameCases(t *testing.T) {
	t.Parallel()
	defs := []Definition{
		{Query: "${SELECTOR}", Kind: KindLog, Directions: DirectionsBoth, TimeRange: TimeRange{Length: "1h"}},
		{Query: "count_over_time(${SELECTOR}[${RANGE}])", Kind: KindMetric, TimeRange: TimeRange{Length: "2h", Step: "1m"}},
	}
	run := func(seed int64) []Case {
		r := NewResolver(testMetadata(), seed)
		var out []Case
		for _, d := range defs {
			cs, err := r.Expand(d, false)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, cs...)
		}
		return out
	}
	a, b := run(42), run(42)
	for i := range a {
		if a[i].Query != b[i].Query || !a[i].Start.Equal(b[i].Start) {
			t.Fatalf("case %d differs between two runs with one seed: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestIntersectSorted(t *testing.T) {
	t.Parallel()
	got := intersectSorted([]string{"a", "c", "d", "f"}, []string{"b", "c", "f", "g"})
	if !slices.Equal(got, []string{"c", "f"}) {
		t.Fatalf("intersectSorted = %v", got)
	}
	if got := intersectSorted(nil, []string{"a"}); len(got) != 0 {
		t.Fatalf("intersectSorted(nil, …) = %v", got)
	}
}

func TestQueryKindAndInferKind(t *testing.T) {
	t.Parallel()
	for q, want := range map[string]string{
		`{a="b"}`:                          KindLog,
		`{a="b"} |= "x" | json`:            KindLog,
		`count_over_time({a="b"}[1m])`:     KindMetric,
		`sum by (a) (rate({a="b"}[1m]))`:   KindMetric,
		`1 + 1`:                            KindMetric,
		`not logql`:                        KindInvalid,
		`count_over_time({a="b"}[1m]) by`:  KindInvalid,
		`sum by (a) (rate({a="b"}[1m])) >`: KindInvalid,
	} {
		if got := QueryKind(q); got != want {
			t.Fatalf("QueryKind(%q) = %q, want %q", q, got, want)
		}
	}
	for q, want := range map[string]string{
		`sum by (${LABEL_NAME}) (count_over_time(${SELECTOR}[${RANGE}]))`: KindMetric,
		`${SELECTOR} | ${LABEL_NAME}="${LABEL_VALUE}"`:                    KindLog,
		`garbage ${SELECTOR}`: KindLog,
	} {
		if got := InferKind(q); got != want {
			t.Fatalf("InferKind(%q) = %q, want %q", q, got, want)
		}
	}
}

func TestDirectionString(t *testing.T) {
	t.Parallel()
	if Forward.String() != "FORWARD" || Backward.String() != "BACKWARD" {
		t.Fatalf("direction names = %s / %s", Forward, Backward)
	}
}
