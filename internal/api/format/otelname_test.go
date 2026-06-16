package format_test

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/tsouza/cerberus/internal/api/format"
)

var (
	promLabelRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	promMetricRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
)

func TestOTelToPromLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"already_ok", "already_ok"},
		{"__name__", "__name__"},
		{"service.name", "service_name"},
		{"http.request.method", "http_request_method"},
		{"network.protocol.version", "network_protocol_version"},
		{"http.response.status_code", "http_response_status_code"},
		{"with-dash", "with_dash"},
		{"with space", "with_space"},
		{"1invalid", "_1invalid"},
		{"9", "_9"},
		// `:` is allowed in metric names but NOT in label names.
		{"a:b", "a_b"},
		// multibyte: each byte becomes an underscore.
		{"服务", "______"},
		// trailing / leading dots collapse to underscores deterministically.
		{".lead", "_lead"},
		{"trail.", "trail_"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := format.OTelToPromLabel(c.in)
			if got != c.want {
				t.Fatalf("OTelToPromLabel(%q) = %q, want %q", c.in, got, c.want)
			}
			// Every non-empty output must satisfy Prom's label grammar.
			if got != "" && !promLabelRE.MatchString(got) {
				t.Fatalf("OTelToPromLabel(%q) = %q does not match Prom label grammar", c.in, got)
			}
		})
	}
}

func TestOTelToPromMetric(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"http_requests_total", "http_requests_total"},
		{"my:metric", "my:metric"},
		{"http.requests.total", "http_requests_total"},
		{"1up", "_1up"},
		{"with-dash", "with_dash"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := format.OTelToPromMetric(c.in)
			if got != c.want {
				t.Fatalf("OTelToPromMetric(%q) = %q, want %q", c.in, got, c.want)
			}
			if got != "" && !promMetricRE.MatchString(got) {
				t.Fatalf("OTelToPromMetric(%q) = %q does not match Prom metric grammar", c.in, got)
			}
		})
	}
}

func TestNormalizeLabelMap(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{"nil", nil, nil},
		{"empty", map[string]string{}, map[string]string{}},
		{
			"passthrough",
			map[string]string{"job": "api", "instance": "x"},
			map[string]string{"job": "api", "instance": "x"},
		},
		{
			"rewrite-dot",
			map[string]string{"service.name": "tempo"},
			map[string]string{"service_name": "tempo"},
		},
		{
			"collision-underscore-wins",
			// Both forms present — only the underscored form survives,
			// carrying its own value (NOT the dotted one).
			map[string]string{"service.name": "tempo", "service_name": "loki"},
			map[string]string{"service_name": "loki"},
		},
		{
			"mixed-shape",
			map[string]string{
				"service.name":         "tempo",
				"http.request.method":  "GET",
				"already_ok":           "1",
				"http.response.status": "200",
			},
			map[string]string{
				"service_name":         "tempo",
				"http_request_method":  "GET",
				"already_ok":           "1",
				"http_response_status": "200",
			},
		},
		{
			"leading-digit",
			map[string]string{"1abc": "v"},
			map[string]string{"_1abc": "v"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := format.NormalizeLabelMap(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("NormalizeLabelMap(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for k := range got {
				if !promLabelRE.MatchString(k) {
					t.Fatalf("output key %q does not match Prom label grammar", k)
				}
			}
		})
	}
}

func TestNormalizeLabelMapDoesNotMutateInput(t *testing.T) {
	in := map[string]string{"service.name": "x", "ok": "y"}
	_ = format.NormalizeLabelMap(in)
	if _, ok := in["service.name"]; !ok {
		t.Fatalf("input was mutated: %v", in)
	}
}

func TestNormalizeLabelNames(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{
			"basic",
			[]string{"service.name", "http.request.method", "job"},
			[]string{"http_request_method", "job", "service_name"},
		},
		{
			"collision-natural-wins",
			[]string{"service.name", "service_name", "job"},
			[]string{"job", "service_name"},
		},
		{
			"dedup",
			[]string{"service.name", "service.name"},
			[]string{"service_name"},
		},
		{
			"empty-dropped",
			[]string{"", "service.name"},
			[]string{"service_name"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := format.NormalizeLabelNames(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("NormalizeLabelNames(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for _, k := range got {
				if !promLabelRE.MatchString(k) {
					t.Fatalf("output entry %q does not match Prom label grammar", k)
				}
			}
		})
	}
}

// TestPromLabelToOTelCandidates pins the dotted-expansion heuristic
// used by the PromQL / LogQL matcher lowering paths. Cerberus emits a
// coalesce / if-chain over the returned candidates so a `cerberus_ql`
// matcher hits both the underscored row (if written that way) and the
// dotted `cerberus.ql` row (the OTel-canonical form). Without this
// expansion, every "by language" dashboard panel against a freshly-
// ingested OTel stream collapses to a single anonymous "Value" series.
func TestPromLabelToOTelCandidates(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", []string{""}},
		{"no-underscore", "job", []string{"job"}},
		{"name-label", "__name__", []string{"__name__"}},
		// A k-underscore name expands to the dot powerset (2^k) UNIONed
		// with the bounded dots→slashes→underscores zone variants, input
		// first then the rest sorted. ASCII order is `.` < `/` < `_`.
		{"single-internal", "cerberus_ql", []string{"cerberus_ql", "cerberus.ql", "cerberus/ql"}},
		{
			"two-internal",
			"service_name_full",
			[]string{
				"service_name_full",
				"service.name.full",
				"service.name/full",
				"service.name_full",
				"service/name/full",
				"service/name_full",
				"service_name.full",
			},
		},
		// Leading-underscore is preserved (it's a Prom grammar marker
		// from a leading-digit OTel source like `1xx` → `_1xx`).
		{
			"leading-underscore",
			"_1invalid",
			[]string{"_1invalid"},
		},
		// `__name__` flows through unchanged because the only
		// underscores are leading / trailing markers, not internal.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := format.PromLabelToOTelCandidates(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("PromLabelToOTelCandidates(%q) =\n  got:  %v\n  want: %v", tc.in, got, tc.want)
			}
			if len(got) == 0 || got[0] != tc.in {
				t.Fatalf("first candidate must be the input verbatim; got %v", got)
			}
		})
	}
}

// TestPromLabelToOTelCandidatesInvariants pins the structural invariants
// for a deeper name without hand-enumerating every entry: input first, no
// duplicates, every candidate differs from the input only at the internal
// underscore positions using only `.` / `/`, the dot powerset is a subset,
// and the uniform all-dots / all-slashes endpoints are present.
func TestPromLabelToOTelCandidatesInvariants(t *testing.T) {
	in := "http_request_method_v2" // 3 internal underscores
	got := format.PromLabelToOTelCandidates(in)
	if got[0] != in {
		t.Fatalf("first candidate must be input verbatim: got %q", got[0])
	}
	seen := map[string]bool{}
	for _, c := range got {
		if seen[c] {
			t.Fatalf("duplicate candidate %q", c)
		}
		seen[c] = true
		if len(c) != len(in) {
			t.Fatalf("candidate %q changed length vs %q", c, in)
		}
		for i := range c {
			if c[i] == in[i] {
				continue
			}
			if in[i] != '_' {
				t.Fatalf("candidate %q rewrote a non-underscore byte at %d", c, i)
			}
			if c[i] != '.' && c[i] != '/' {
				t.Fatalf("candidate %q used separator %q (want . or /)", c, c[i])
			}
		}
	}
	for _, must := range []string{
		"http.request.method.v2", // all-dots (dot powerset endpoint)
		"http/request/method/v2", // all-slashes (zone endpoint)
		"http.request_method.v2", // dot powerset member
		"http.request/method/v2", // mixed dot→slash zone member
	} {
		if !seen[must] {
			t.Fatalf("expected candidate %q missing from %v", must, got)
		}
	}
}

// TestPromLabelToOTelCandidatesGCP pins the dotted-AND-slashed GCP Cloud
// Monitoring shape (rc.8 Issue A): the queried Prometheus name must
// reverse-map to the literal raw OTel name the clickhouseexporter wrote,
// e.g. `cloudsql.googleapis.com/database/up`. A `.`-only powerset never
// reconstructs the slash segments — this is the regression guard. The
// zone model (dots→slashes→underscores) covers the GCP
// `domain.parts/path/parts/leaf_name` structure.
func TestPromLabelToOTelCandidatesGCP(t *testing.T) {
	for _, tc := range []struct{ in, raw string }{
		{"cloudsql_googleapis_com_database_up", "cloudsql.googleapis.com/database/up"},
		{"loadbalancing_googleapis_com_https_request_count", "loadbalancing.googleapis.com/https/request_count"},
		{"loadbalancing_googleapis_com_https_total_latencies_bucket", "loadbalancing.googleapis.com/https/total_latencies_bucket"},
	} {
		got := format.PromLabelToOTelCandidates(tc.in)
		found := false
		for _, c := range got {
			if c == tc.raw {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("PromLabelToOTelCandidates(%q): raw OTel name %q not reconstructed (got %d candidates)",
				tc.in, tc.raw, len(got))
		}
	}
}

// TestPromLabelToOTelCandidatesCap pins the powerset cap — beyond 6
// rewritable underscores the function falls back to the uniform endpoints
// (verbatim + all-dots + all-slashes) so the candidate set stays bounded.
func TestPromLabelToOTelCandidatesCap(t *testing.T) {
	// 8 internal underscores → 2^8 + zone entries without the cap; 3 with it.
	in := "a_b_c_d_e_f_g_h_i"
	got := format.PromLabelToOTelCandidates(in)
	if len(got) != 3 {
		t.Fatalf("powerset cap not applied: got %d candidates, want 3", len(got))
	}
	if got[0] != in {
		t.Fatalf("first candidate must be input verbatim: got %q", got[0])
	}
	if got[1] != "a.b.c.d.e.f.g.h.i" {
		t.Fatalf("fallback candidate 1 must be all-dots: got %q", got[1])
	}
	if got[2] != "a/b/c/d/e/f/g/h/i" {
		t.Fatalf("fallback candidate 2 must be all-slashes: got %q", got[2])
	}
}

// TestPromLabelToOTelCandidatesCached pins the memoisation contract:
// repeated calls with the same input return the same slice (identity,
// not just deep-equal) so callers can rely on the cache amortising the
// powerset walk. Unknown-on-first-call labels still resolve correctly
// through the cold path — the cache is a fast-path overlay, never a
// gate. The "identity" check uses a tiny indirection trick: take the
// slice header of each call and compare the underlying data pointer.
func TestPromLabelToOTelCandidatesCached(t *testing.T) {
	// A label the cache has likely never seen — uniqueness rules out
	// test-order coupling with the powerset / fallback tests above.
	const novel = "cerberus_cache_probe_label"
	a := format.PromLabelToOTelCandidates(novel)
	b := format.PromLabelToOTelCandidates(novel)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("repeated calls returned different content: %v vs %v", a, b)
	}
	// Identity check: cached slices must share the same backing array.
	// `&a[0] == &b[0]` is the canonical "is this the same slice header
	// payload" probe.
	if len(a) > 0 && len(b) > 0 && &a[0] != &b[0] {
		t.Fatalf("cache returned a freshly-allocated slice on repeat: pointers differ")
	}
}

// TestPromLabelNeedsDottedFallback pins the gate function that
// lowering paths use to short-circuit single-key emission when no
// rewrite is possible. The "fast path" stays a plain MapAccess for
// `job`, `__name__`, etc.
func TestPromLabelNeedsDottedFallback(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"job", false},
		{"__name__", false},
		{"_1invalid", false},
		{"cerberus_ql", true},
		{"service_name", true},
		{"_lead_internal", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := format.PromLabelNeedsDottedFallback(tc.in); got != tc.want {
				t.Fatalf("PromLabelNeedsDottedFallback(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeLabelNamesNoStrayBytes is a property-shape test pinning
// the conformance contract: every output of NormalizeLabelNames matches
// `^[a-zA-Z_][a-zA-Z0-9_]*$`. Catches future regressions where a new
// edge case sneaks through (e.g. someone allowing `:` in label names).
func TestNormalizeLabelNamesNoStrayBytes(t *testing.T) {
	in := []string{
		"__name__",
		"cerberus.ql",
		"cerberus.route",
		"http.request.method",
		"http.response.status_code",
		"http.route",
		"http_status",
		"job",
		"network.protocol.name",
		"network.protocol.version",
		"result",
		"server.address",
		"server.port",
		"stage",
		"url.scheme",
	}
	got := format.NormalizeLabelNames(in)
	for _, k := range got {
		if !promLabelRE.MatchString(k) {
			t.Fatalf("output %q has stray bytes", k)
		}
	}
	// Verify the specific keys from the bug repro come out clean.
	expected := map[string]bool{
		"__name__":                  true,
		"cerberus_ql":               true,
		"cerberus_route":            true,
		"http_request_method":       true,
		"http_response_status_code": true,
		"http_route":                true,
		"http_status":               true,
		"job":                       true,
		"network_protocol_name":     true,
		"network_protocol_version":  true,
		"result":                    true,
		"server_address":            true,
		"server_port":               true,
		"stage":                     true,
		"url_scheme":                true,
	}
	for _, k := range got {
		if !expected[k] {
			t.Fatalf("unexpected output key %q", k)
		}
		delete(expected, k)
	}
	if len(expected) > 0 {
		t.Fatalf("missing expected keys: %v", expected)
	}
}
