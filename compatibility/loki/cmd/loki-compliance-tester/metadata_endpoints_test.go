package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	bench "github.com/tsouza/cerberus/compatibility/loki/upstream/loki-bench"
)

// metadataStub is an in-memory Loki-shaped metadata backend: a handful
// of streams, served on every route the metadata pass grades, in the
// exact wire shapes upstream uses (enveloped lists for /labels,
// /label/{name}/values and /series; a Prometheus vector for
// /index/volume; bare proto JSON for /index/stats and /detected_labels).
// Two stubs built from the same streams agree on every case; a stub
// with one label value changed disagrees on exactly the cases that
// observe that value.
type metadataStub struct {
	streams []map[string]string
	// shuffle reverses every list the stub serves, so a comparator
	// that depended on wire order would see two agreeing stubs as
	// diverging.
	shuffle bool
	// status, when non-zero, is returned on every route with body
	// `stub error` instead of data.
	status int
}

func (s *metadataStub) matches(stream map[string]string, selector string) bool {
	// The stub understands the two selector shapes the pass sends: the
	// all-streams regex and an exact single-label equality.
	if selector == "" || selector == metadataAllStreamsSelector {
		return true
	}
	inner := strings.Trim(selector, "{}")
	name, value, ok := strings.Cut(inner, "=")
	if !ok {
		return false
	}
	return stream[name] == strings.Trim(value, `"`)
}

func (s *metadataStub) selected(selector string) []map[string]string {
	var out []map[string]string
	for _, st := range s.streams {
		if s.matches(st, selector) {
			out = append(out, st)
		}
	}
	return out
}

func (s *metadataStub) order(list []string) []string {
	sort.Strings(list)
	if s.shuffle {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	return list
}

func (s *metadataStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.status != 0 {
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte("stub error"))
		return
	}
	q := r.URL.Query()
	w.Header().Set("Content-Type", "application/json")
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	envelope := func(data any) { write(map[string]any{"status": "success", "data": data}) }

	switch {
	case r.URL.Path == "/loki/api/v1/labels":
		set := map[string]struct{}{}
		for _, st := range s.selected(q.Get("query")) {
			for k := range st {
				set[k] = struct{}{}
			}
		}
		envelope(s.order(keysOf(set)))
	case strings.HasPrefix(r.URL.Path, "/loki/api/v1/label/") && strings.HasSuffix(r.URL.Path, "/values"):
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/loki/api/v1/label/"), "/values")
		set := map[string]struct{}{}
		for _, st := range s.selected(q.Get("query")) {
			if v, ok := st[name]; ok {
				set[v] = struct{}{}
			}
		}
		envelope(s.order(keysOf(set)))
	case r.URL.Path == "/loki/api/v1/series":
		sel := s.selected(q.Get("match[]"))
		if s.shuffle {
			for i, j := 0, len(sel)-1; i < j; i, j = i+1, j-1 {
				sel[i], sel[j] = sel[j], sel[i]
			}
		}
		envelope(sel)
	case r.URL.Path == "/loki/api/v1/index/stats":
		sel := s.selected(q.Get("query"))
		// chunks and bytes are deliberately derived from the shuffle flag
		// so two agreeing stubs report different chunk-storage numbers,
		// as a chunk store and a row store would.
		chunks, bytes := uint64(len(sel)), uint64(len(sel))*100
		if s.shuffle {
			chunks, bytes = 0, uint64(len(sel))*97
		}
		write(indexStatsWire{Streams: uint64(len(sel)), Chunks: chunks, Entries: uint64(len(sel)) * 10, Bytes: bytes})
	case r.URL.Path == "/loki/api/v1/index/volume":
		sel := s.selected(q.Get("query"))
		target := map[string]struct{}{}
		if tl := q.Get("targetLabels"); tl != "" {
			for _, k := range strings.Split(tl, ",") {
				target[k] = struct{}{}
			}
		}
		volumes := map[string]map[string]string{}
		for _, st := range sel {
			key := map[string]string{}
			for k, v := range st {
				if _, ok := target[k]; ok || len(target) == 0 {
					key[k] = v
				}
			}
			if q.Get("aggregateBy") == volumeAggregateByLabels {
				for k := range key {
					volumes[k] = map[string]string{k: ""}
				}
				continue
			}
			volumes[renderLabelSets([]map[string]string{key})[0]] = key
		}
		names := s.order(keysOf(volumes))
		if limit := q.Get("limit"); limit == fmt.Sprint(volumeTieBreakLimit) {
			// Every row ties (see the file comment in metadata_endpoints.go),
			// so upstream's cut is by name ascending.
			sort.Strings(names)
			names = names[:volumeTieBreakLimit]
		}
		result := make([]map[string]any, 0, len(names))
		for _, n := range names {
			result = append(result, map[string]any{"metric": volumes[n], "value": []any{1.7e9, "12345"}})
		}
		envelope(map[string]any{"resultType": "vector", "result": result})
	case r.URL.Path == "/loki/api/v1/detected_labels":
		values := map[string]map[string]struct{}{}
		for _, st := range s.selected(q.Get("query")) {
			for k, v := range st {
				if values[k] == nil {
					values[k] = map[string]struct{}{}
				}
				values[k][v] = struct{}{}
			}
		}
		var out []map[string]any
		for _, k := range s.order(keysOf(values)) {
			out = append(out, map[string]any{"label": k, "cardinality": len(values[k])})
		}
		write(map[string]any{"detectedLabels": out})
	default:
		http.NotFound(w, r)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func metadataStubStreams() []map[string]string {
	return []map[string]string{
		{"cluster": "cluster-0", "namespace": "ns-0", "service_name": "web", "env": "production"},
		{"cluster": "cluster-0", "namespace": "ns-1", "service_name": "db", "env": "production"},
		{"cluster": "cluster-1", "namespace": "ns-2", "service_name": "cache", "env": "production"},
	}
}

func newMetadataStub(t *testing.T, stub *metadataStub) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	return srv
}

func metadataTestWindow() *bench.DatasetMetadata {
	return &bench.DatasetMetadata{
		TimeRange: bench.TimeRange{
			Start: time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC),
		},
	}
}

// TestCompareMetadataEndpointsAll_AgreementIsOrderInsensitive — two
// backends serving the same streams agree on every case even when one
// of them reverses every list it serves and reports different
// chunk-storage numbers on /index/stats. The roster is the fixed table
// plus one label-values case per label the reference advertises, and
// every identity is distinct.
func TestCompareMetadataEndpointsAll_AgreementIsOrderInsensitive(t *testing.T) {
	t.Parallel()
	ref := newMetadataStub(t, &metadataStub{streams: metadataStubStreams()})
	test := newMetadataStub(t, &metadataStub{streams: metadataStubStreams(), shuffle: true})

	results := compareMetadataEndpointsAll(&http.Client{Timeout: 5 * time.Second}, flags{addr1: ref.URL, addr2: test.URL}, metadataTestWindow())

	labelCount := len(metadataStubStreams()[0])
	if want := len(metadataFixedCases(time.Time{}, time.Time{})) + labelCount; len(results) != want {
		t.Fatalf("results=%d, want %d (fixed cases + one label-values case per advertised label)", len(results), want)
	}
	ids := map[string]struct{}{}
	kinds := map[string]int{}
	for _, r := range results {
		if !r.success() {
			t.Errorf("agreeing backends produced a non-passing row: kind=%s desc=%q diff=%q failure=%q", r.TestCase.Kind, r.TestCase.Description, r.Diff, r.UnexpectedFailure)
		}
		if r.TestCase.Source != metadataEndpointsSource {
			t.Errorf("source=%q, want %q", r.TestCase.Source, metadataEndpointsSource)
		}
		id := r.TestCase.id()
		if _, dup := ids[id]; dup {
			t.Errorf("duplicate roster identity %q", id)
		}
		ids[id] = struct{}{}
		kinds[r.TestCase.Kind]++
	}
	for _, kind := range []string{
		metadataKindLabels, metadataKindLabelValues, metadataKindSeries,
		metadataKindIndexStats, metadataKindIndexVolume, metadataKindDetectedLabels,
	} {
		if kinds[kind] == 0 {
			t.Errorf("no case of kind %q reached the report", kind)
		}
	}
}

// TestCompareMetadataEndpointsAll_OneLabelValueDiffersFails — the
// class test. The test backend serves the same streams as the
// reference except that one stream's `namespace` value differs. Every
// case that observes that value fails and names it; every case that
// cannot observe it (label names, stats counts, the cluster-only
// volume projections) still passes.
func TestCompareMetadataEndpointsAll_OneLabelValueDiffersFails(t *testing.T) {
	t.Parallel()
	changed := metadataStubStreams()
	changed[1]["namespace"] = "ns-1-renamed"
	ref := newMetadataStub(t, &metadataStub{streams: metadataStubStreams()})
	test := newMetadataStub(t, &metadataStub{streams: changed, shuffle: true})

	results := compareMetadataEndpointsAll(&http.Client{Timeout: 5 * time.Second}, flags{addr1: ref.URL, addr2: test.URL}, metadataTestWindow())

	var failed, passed []string
	for _, r := range results {
		if r.UnexpectedFailure != "" {
			t.Errorf("unexpected harness failure on %q: %s", r.TestCase.Description, r.UnexpectedFailure)
		}
		if r.Diff != "" {
			failed = append(failed, r.TestCase.Description+" => "+r.Diff)
		} else {
			passed = append(passed, r.TestCase.Description)
		}
	}
	if len(failed) == 0 {
		t.Fatal("a differing label value produced no failing case")
	}
	for _, want := range []string{
		`label values parity: label=namespace selector=<none> => value "ns-1" missing from test endpoint; value "ns-1-renamed" unexpected on test endpoint`,
		`series parity: selector={service_name=~".+"} => label set {cluster="cluster-0", env="production", namespace="ns-1", service_name="db"} missing from test endpoint; label set {cluster="cluster-0", env="production", namespace="ns-1-renamed", service_name="db"} unexpected on test endpoint`,
		`series parity: selector={cluster="cluster-0"} =>`,
		`index volume parity (label sets): aggregateBy=series limit=1000 selector={service_name=~".+"} =>`,
		`index volume parity (label sets): aggregateBy=series limit=1000 targetLabels=cluster,namespace selector={service_name=~".+"} =>`,
	} {
		if !containsPrefix(failed, want) {
			t.Errorf("expected a failing case starting with %q; failing cases:\n  %s", want, strings.Join(failed, "\n  "))
		}
	}
	for _, want := range []string{
		"labels parity: selector=<none>",
		"index stats parity (streams, entries): selector={service_name=~\".+\"}",
		"index volume parity (label sets): aggregateBy=series limit=1000 targetLabels=cluster selector={service_name=~\".+\"}",
		"index volume parity (label sets): aggregateBy=labels limit=3 selector={service_name=~\".+\"}",
		"detected labels parity (label, cardinality): selector=<none>",
	} {
		if !containsPrefix(passed, want) {
			t.Errorf("expected %q to pass (it cannot observe the changed value); passing cases:\n  %s", want, strings.Join(passed, "\n  "))
		}
	}
}

func containsPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// TestCompareMetadataOne_StatusArms — the reference failing is a
// harness problem; the test endpoint answering a different status is a
// parity diff naming both statuses; an empty side follows the corpus
// emptiness convention.
func TestCompareMetadataOne_StatusArms(t *testing.T) {
	t.Parallel()
	window := metadataTestWindow()
	start, end := window.TimeRange.Start, window.TimeRange.End
	mc := metadataLabelsCase("", start, end)

	cases := []struct {
		name        string
		ref, test   *metadataStub
		wantDiff    string
		wantFailure string
	}{
		{
			name:        "reference non-200",
			ref:         &metadataStub{status: http.StatusInternalServerError},
			test:        &metadataStub{streams: metadataStubStreams()},
			wantFailure: "reference (-addr-1) returned status=500",
		},
		{
			name:     "test endpoint non-200",
			ref:      &metadataStub{streams: metadataStubStreams()},
			test:     &metadataStub{status: http.StatusBadRequest},
			wantDiff: "status differs: reference=200 test endpoint=400 (test body=stub error)",
		},
		{
			name:        "reference empty",
			ref:         &metadataStub{},
			test:        &metadataStub{streams: metadataStubStreams()},
			wantFailure: "baseline returned empty",
		},
		{
			name:        "test endpoint empty",
			ref:         &metadataStub{streams: metadataStubStreams()},
			test:        &metadataStub{},
			wantFailure: "test endpoint returned empty",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := flags{addr1: newMetadataStub(t, tc.ref).URL, addr2: newMetadataStub(t, tc.test).URL}
			got := compareMetadataOne(&http.Client{Timeout: 5 * time.Second}, f, mc, start, end)
			if tc.wantDiff != "" && got.Diff != tc.wantDiff {
				t.Fatalf("Diff=%q, want %q (failure=%q)", got.Diff, tc.wantDiff, got.UnexpectedFailure)
			}
			if tc.wantFailure != "" && !strings.Contains(got.UnexpectedFailure, tc.wantFailure) {
				t.Fatalf("UnexpectedFailure=%q, want it to contain %q (diff=%q)", got.UnexpectedFailure, tc.wantFailure, got.Diff)
			}
		})
	}
}

// TestDiffMetadataBody_SetKinds — for every set-valued kind, identical
// sets in a different order pass, one differing element fails and is
// named, and a duplicated element counts.
func TestDiffMetadataBody_SetKinds(t *testing.T) {
	t.Parallel()
	a := map[string]string{"cluster": "c0", "service_name": "web"}
	b := map[string]string{"cluster": "c1", "service_name": "db"}
	bChanged := map[string]string{"cluster": "c1", "service_name": "db2"}

	cases := []struct {
		name                string
		same, reordered     metadataBody
		changed, duplicated metadataBody
		wantChanged         string
	}{
		{
			name:        "labels",
			same:        metadataBody{kind: metadataKindLabels, names: []string{"a", "b"}},
			reordered:   metadataBody{kind: metadataKindLabels, names: []string{"b", "a"}},
			changed:     metadataBody{kind: metadataKindLabels, names: []string{"a", "c"}},
			duplicated:  metadataBody{kind: metadataKindLabels, names: []string{"a", "b", "b"}},
			wantChanged: `value "b" missing from test endpoint; value "c" unexpected on test endpoint`,
		},
		{
			name:        "label values",
			same:        metadataBody{kind: metadataKindLabelValues, names: []string{"web", "db"}},
			reordered:   metadataBody{kind: metadataKindLabelValues, names: []string{"db", "web"}},
			changed:     metadataBody{kind: metadataKindLabelValues, names: []string{"web", "db2"}},
			duplicated:  metadataBody{kind: metadataKindLabelValues, names: []string{"web", "web", "db"}},
			wantChanged: `value "db" missing from test endpoint; value "db2" unexpected on test endpoint`,
		},
		{
			name:        "series",
			same:        metadataBody{kind: metadataKindSeries, labelSets: []map[string]string{a, b}},
			reordered:   metadataBody{kind: metadataKindSeries, labelSets: []map[string]string{b, a}},
			changed:     metadataBody{kind: metadataKindSeries, labelSets: []map[string]string{a, bChanged}},
			duplicated:  metadataBody{kind: metadataKindSeries, labelSets: []map[string]string{a, a, b}},
			wantChanged: `label set {cluster="c1", service_name="db"} missing from test endpoint; label set {cluster="c1", service_name="db2"} unexpected on test endpoint`,
		},
		{
			name:        "index volume",
			same:        metadataBody{kind: metadataKindIndexVolume, labelSets: []map[string]string{a, b}},
			reordered:   metadataBody{kind: metadataKindIndexVolume, labelSets: []map[string]string{b, a}},
			changed:     metadataBody{kind: metadataKindIndexVolume, labelSets: []map[string]string{a, bChanged}},
			duplicated:  metadataBody{kind: metadataKindIndexVolume, labelSets: []map[string]string{a, b, b}},
			wantChanged: `label set {cluster="c1", service_name="db"} missing from test endpoint; label set {cluster="c1", service_name="db2"} unexpected on test endpoint`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := diffMetadataBody(tc.same, tc.reordered); diff != "" {
				t.Fatalf("identical set in a different order diffed: %s", diff)
			}
			if diff := diffMetadataBody(tc.same, tc.changed); diff != tc.wantChanged {
				t.Fatalf("one differing element: diff=%q, want %q", diff, tc.wantChanged)
			}
			if diff := diffMetadataBody(tc.same, tc.duplicated); diff == "" {
				t.Fatal("a duplicated element on the test endpoint passed")
			}
		})
	}
}

// TestDiffMetadataBody_DetectedLabels — the cardinality rides with the
// name: a label that is present with a different count fails, and so
// does a missing or unexpected label; order does not matter.
func TestDiffMetadataBody_DetectedLabels(t *testing.T) {
	t.Parallel()
	same := metadataBody{kind: metadataKindDetectedLabels, cardinality: map[string]uint64{"cluster": 2, "pod": 15}}
	if diff := diffMetadataBody(same, metadataBody{kind: metadataKindDetectedLabels, cardinality: map[string]uint64{"pod": 15, "cluster": 2}}); diff != "" {
		t.Fatalf("same map diffed: %s", diff)
	}
	changed := metadataBody{kind: metadataKindDetectedLabels, cardinality: map[string]uint64{"cluster": 3, "region": 1}}
	want := `label "cluster" cardinality: expected=2 actual=3; label "pod" missing from test endpoint; label "region" unexpected on test endpoint`
	if diff := diffMetadataBody(same, changed); diff != want {
		t.Fatalf("diff=%q, want %q", diff, want)
	}
}

// TestDiffMetadataBody_IndexStats — streams and entries are graded;
// chunks and bytes, which a row store cannot report the way a chunk
// store does, are not.
func TestDiffMetadataBody_IndexStats(t *testing.T) {
	t.Parallel()
	ref := metadataBody{kind: metadataKindIndexStats, stats: indexStatsWire{Streams: 15, Chunks: 360, Entries: 21600, Bytes: 4_000_000}}
	rowStore := metadataBody{kind: metadataKindIndexStats, stats: indexStatsWire{Streams: 15, Chunks: 0, Entries: 21600, Bytes: 3_700_000}}
	if diff := diffMetadataBody(ref, rowStore); diff != "" {
		t.Fatalf("chunks/bytes divergence graded: %s", diff)
	}
	wrong := metadataBody{kind: metadataKindIndexStats, stats: indexStatsWire{Streams: 14, Entries: 21599}}
	if diff := diffMetadataBody(ref, wrong); diff != "streams: expected=15 actual=14; entries: expected=21600 actual=21599" {
		t.Fatalf("diff=%q", diff)
	}
}

// TestDecodeMetadataBody_Shapes — enveloped routes require
// status=success; bare routes reject an envelope; /index/volume decodes
// through the vector decoder and rejects any other resultType.
func TestDecodeMetadataBody_Shapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		kind    string
		body    string
		wantErr string
		check   func(metadataBody) bool
	}{
		{
			name:  "labels envelope",
			kind:  metadataKindLabels,
			body:  `{"status":"success","data":["a","b"]}`,
			check: func(b metadataBody) bool { return len(b.names) == 2 },
		},
		{
			name:    "labels error status",
			kind:    metadataKindLabels,
			body:    `{"status":"error","data":["a"]}`,
			wantErr: `envelope status="error"`,
		},
		{
			name:  "series envelope",
			kind:  metadataKindSeries,
			body:  `{"status":"success","data":[{"a":"1"},{"a":"2"}]}`,
			check: func(b metadataBody) bool { return len(b.labelSets) == 2 && b.labelSets[1]["a"] == "2" },
		},
		{
			name:  "volume vector",
			kind:  metadataKindIndexVolume,
			body:  `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"cluster":""},"value":[1700000000,"42"]}]}}`,
			check: func(b metadataBody) bool { return len(b.labelSets) == 1 && b.labelSets[0]["cluster"] == "" },
		},
		{
			name:    "volume not a vector",
			kind:    metadataKindIndexVolume,
			body:    `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
			wantErr: "want vector",
		},
		{
			name:  "stats bare",
			kind:  metadataKindIndexStats,
			body:  `{"streams":15,"chunks":0,"entries":21600,"bytes":1}`,
			check: func(b metadataBody) bool { return b.stats.Streams == 15 && b.stats.Entries == 21600 },
		},
		{
			name:    "stats enveloped",
			kind:    metadataKindIndexStats,
			body:    `{"status":"success","data":{"streams":15,"chunks":0,"entries":21600,"bytes":1}}`,
			wantErr: "envelope",
		},
		{
			name:  "detected labels bare",
			kind:  metadataKindDetectedLabels,
			body:  `{"detectedLabels":[{"label":"cluster","cardinality":2,"sketch":"AAEC"}]}`,
			check: func(b metadataBody) bool { return b.cardinality["cluster"] == 2 },
		},
		{
			name:    "detected labels enveloped",
			kind:    metadataKindDetectedLabels,
			body:    `{"status":"success","data":{"detectedLabels":[]}}`,
			wantErr: "envelope",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeMetadataBody(tc.kind, []byte(tc.body))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !tc.check(got) {
				t.Fatalf("decoded body=%+v", got)
			}
		})
	}
}

// TestMetadataFixedCases_TieBreakLimitCutsTiedRows — the tie-break
// case is only a ranking grade while its limit is below the number of
// labels every seeded stream carries and the no-truncation limit is
// above any seeded row count; pin both against the real fixture
// description so a seeder change cannot silently turn the ranking case
// into a no-op.
func TestMetadataFixedCases_TieBreakLimitCutsTiedRows(t *testing.T) {
	t.Parallel()
	metadata, err := bench.LoadMetadata(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if len(metadata.AllSelectors) == 0 {
		t.Fatal("dataset_metadata.json advertises no selectors")
	}
	// Every seeded selector is the stream's FULL label set (one
	// `name="value"` pair per label), so counting the pairs of any one
	// of them is the per-stream label count the tie-break relies on.
	seededLabelsPerStream := strings.Count(metadata.AllSelectors[0], `="`)
	if volumeTieBreakLimit >= seededLabelsPerStream {
		t.Fatalf("volumeTieBreakLimit=%d must be below the %d labels every seeded stream carries, or the labels-mode response is never cut", volumeTieBreakLimit, seededLabelsPerStream)
	}
	if volumeNoTruncationLimit <= len(metadata.AllSelectors) || volumeNoTruncationLimit <= seededLabelsPerStream {
		t.Fatalf("volumeNoTruncationLimit=%d must exceed every seeded row count (%d streams, %d labels)", volumeNoTruncationLimit, len(metadata.AllSelectors), seededLabelsPerStream)
	}
	var tieBreak, noTruncation int
	for _, mc := range metadataFixedCases(time.Time{}, time.Time{}) {
		if mc.kind != metadataKindIndexVolume {
			continue
		}
		switch mc.params.Get("limit") {
		case fmt.Sprint(volumeTieBreakLimit):
			tieBreak++
			if mc.params.Get("aggregateBy") != volumeAggregateByLabels {
				t.Fatalf("tie-break case must run in labels mode (the only mode where every row ties): %+v", mc)
			}
		case fmt.Sprint(volumeNoTruncationLimit):
			noTruncation++
		default:
			t.Fatalf("volume case with an unexplained limit: %+v", mc)
		}
	}
	if tieBreak == 0 || noTruncation == 0 {
		t.Fatalf("tie-break cases=%d no-truncation cases=%d, want both present", tieBreak, noTruncation)
	}
}
