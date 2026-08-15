//go:build chdb_agpl_oracle

package logql

type exoticCase struct {
	name  string
	logql string
}

// ExoticMatrix covers every stream-query family the independent LogQL
// oracle evaluates. Metric queries and parser stages absent from that oracle
// are deliberately not represented as both-erroring pseudo-coverage.
var ExoticMatrix = []exoticCase{
	// CAT1: stream label selection, including missing-label semantics.
	{name: "cat1/selector_equal", logql: `{job="api"}`},
	{name: "cat1/selector_not_equal", logql: `{service_name=~".+",job!="api"}`},
	{name: "cat1/selector_regex", logql: `{service_name=~"auth|billing"}`},
	{name: "cat1/selector_not_regex", logql: `{job=~".+",service_name!~"checkout"}`},
	{name: "cat1/selector_conjunction", logql: `{job="api",service_name="auth"}`},
	{name: "cat1/selector_empty", logql: `{job="missing"}`},

	// CAT2: positive, negative, regex, and chained line filters.
	{name: "cat2/contains", logql: `{job="api"} |= "retry"`},
	{name: "cat2/not_contains", logql: `{job="api"} != "retry"`},
	{name: "cat2/regex", logql: `{job="api"} |~ "(timeout|error)"`},
	{name: "cat2/not_regex", logql: `{job="api"} !~ "timeout"`},
	{name: "cat2/chained", logql: `{job="api"} |= "retry" != "billing"`},

	// CAT3: post-pipeline stream identity mutation.
	{name: "cat3/label_format_job", logql: `{job="api"} | label_format workload=job`},
	{name: "cat3/label_format_service", logql: `{service_name="auth"} | label_format service=service_name`},

	// CAT4: IP filter single address, CIDR, range, and negation.
	{name: "cat4/ip_single", logql: `{job="api"} |= ip("10.1.2.3")`},
	{name: "cat4/ip_cidr", logql: `{job="api"} |= ip("10.0.0.0/8")`},
	{name: "cat4/ip_range", logql: `{job="api"} |= ip("192.168.0.1-192.168.1.255")`},
	{name: "cat4/ip_not", logql: `{job="api"} != ip("10.0.0.0/8")`},

	// CAT5: pattern filters with leading/trailing captures and negation.
	{name: "cat5/pattern_surrounded", logql: `{job="api"} |> "<_>timeout<_>"`},
	{name: "cat5/pattern_prefix", logql: `{job="api"} |> "auth<_>"`},
	{name: "cat5/pattern_suffix", logql: `{job="api"} |> "<_>retry"`},
	{name: "cat5/pattern_not", logql: `{job="api"} !> "<_>timeout<_>"`},
}
