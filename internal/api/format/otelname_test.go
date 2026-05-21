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
