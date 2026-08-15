//go:build chdb

package traceql

type exoticCase struct {
	name    string
	traceql string
}

// ExoticMatrix covers every query family the independent TraceQL oracle
// evaluates, with explicit empty and boundary cases alongside positive ones.
var ExoticMatrix = []exoticCase{
	// CAT1: resource and span attribute selectors.
	{name: "cat1/service", traceql: `{ resource.service.name = "api" }`},
	{name: "cat1/cluster", traceql: `{ resource.cluster = "east" }`},
	{name: "cat1/span_method", traceql: `{ span.http.method = "POST" }`},
	{name: "cat1/empty", traceql: `{ resource.service.name = "missing" }`},

	// CAT2: matcher variants and conjunction.
	{name: "cat2/regex", traceql: `{ resource.service.name =~ "a.*" }`},
	{name: "cat2/not_regex", traceql: `{ resource.service.name !~ "web" }`},
	{name: "cat2/not_equal", traceql: `{ resource.cluster != "west" }`},
	{name: "cat2/conjunction", traceql: `{ resource.service.name = "api" && status = error }`},

	// CAT3: intrinsic filters and exact boundaries.
	{name: "cat3/duration_gt", traceql: `{ duration > 50ms }`},
	{name: "cat3/duration_equal", traceql: `{ duration = 120ms }`},
	{name: "cat3/status", traceql: `{ status = error }`},
	{name: "cat3/status_not", traceql: `{ status != unset }`},
	{name: "cat3/name", traceql: `{ name = "GET /checkout/0" }`},

	// CAT4: immediate-child versus any-depth descendant semantics.
	{name: "cat4/child", traceql: `{ resource.service.name = "api" } > { resource.service.name = "web" }`},
	{name: "cat4/descendant", traceql: `{ resource.service.name = "api" } >> { resource.service.name = "batch" }`},
	{name: "cat4/structural_empty", traceql: `{ resource.service.name = "batch" } > { resource.service.name = "api" }`},

	// CAT5: trace-scoped count filters, including equality and ceiling.
	{name: "cat5/count_gt", traceql: `{ resource.service.name = "api" } | count() > 1`},
	{name: "cat5/count_equal", traceql: `{ resource.service.name = "web" } | count() = 2`},
	{name: "cat5/count_empty", traceql: `{ resource.service.name = "api" } | count() > 3`},

	// CAT6: duration reducers across the matching spans of each trace.
	{name: "cat6/avg", traceql: `{ resource.service.name = "api" } | avg(duration) > 100ms`},
	{name: "cat6/min", traceql: `{ resource.service.name = "web" } | min(duration) = 10ms`},
	{name: "cat6/max", traceql: `{ resource.service.name = "web" } | max(duration) >= 120ms`},
	{name: "cat6/sum", traceql: `{ resource.service.name = "batch" } | sum(duration) >= 300ms`},

	// CAT7: projection preserves the selected spanset cardinality.
	{name: "cat7/select", traceql: `{ resource.service.name = "api" } | select(span.http.method)`},
}
