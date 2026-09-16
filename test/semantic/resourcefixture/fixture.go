// Package resourcefixture holds SIGNAL-RESOURCE-001's one deterministic
// cross-head resource fixture (cerberus issue #3457): a single logical OTel
// resource, seeded independently into each of the three heads' own metrics /
// logs / traces tables, that proves the SAME configured resource is
// queryable through each head's own documented projection — not that the
// three heads expose byte-identical wire labels for it, which the heads'
// genuinely different label-sanitization, Map-vs-scoped-attribute, and
// timestamp-precision strategies make a false contract (see PromQL's
// internal/promql/resource_attributes.go, LogQL's
// internal/schema/logs.go's MaterializedResourceColumns doc, and TraceQL's
// resource./span. scope prefixes).
//
// This package deliberately stays a "tiny fixture value object" plus three
// independent per-head test files — NOT a shared "TelemetryWorld" seed/
// query/compare abstraction. A general cross-signal world is an explicit
// NO-GO for this issue: coupling all three heads' random generators or
// inventing one uniform seed/query/assert vocabulary across PromQL, LogQL
// and TraceQL is a materially larger project than one correlated fixture
// needs, and would hide each head's real, different attribute strategy
// behind a false shared abstraction — precisely the failure mode this
// fixture exists to catch. Each per-head test file in this package builds
// its own DDL, its own seed SQL, its own query string, and its own
// assertions directly from the ResourceFixture value below; none of them
// call into one another or into a shared seed/query helper.
package resourcefixture

import "time"

// ResourceFixture is the one correlated logical resource every per-head
// adapter in this package seeds into its own table, under its own custom
// schema override, and queries through its own head's grammar.
type ResourceFixture struct {
	// Team is a simple, undotted resource key every head exposes
	// identically with NO normalization: PromQL and LogQL project it
	// verbatim as the wire label "team"; TraceQL addresses it as the
	// resource-scoped attribute "resource.team". It is deliberately not a
	// real OTel semantic-convention key (unlike "service.name") so it
	// never triggers a head's own dedicated-column special-casing (e.g.
	// PromQL's ServiceNameColumn arm, internal/promql/resource_attributes.go's
	// dedicatedResourceKeys) — this fixture means to exercise the
	// generic resource-attribute path every custom key takes, not a
	// dedicated-column fast path.
	Team string
	// Namespace is a dotted OTel-style resource key whose per-head
	// projection genuinely differs: PromQL and LogQL both sanitize it to
	// the underscored wire label "k8s_namespace_name"
	// (internal/api/format.OTelToPromLabel) and reverse that
	// normalization via each head's own dotted-fallback candidate chain;
	// TraceQL addresses the identical attribute dotted and
	// resource-scoped, "resource.k8s.namespace.name", never underscored.
	Namespace string
	// Timestamp is the single instant every per-head seed writes its one
	// fixture row at, carrying a sub-millisecond fractional component
	// (nanosecond precision) so each head's OWN native timestamp
	// contract gets asserted — PromQL's millisecond-resolution wire
	// value, LogQL's and TraceQL's full nanosecond value — rather than
	// rounding every signal into one universal millisecond model.
	Timestamp time.Time
}

// Fixture is the one deterministic resource this package's three
// independent per-head adapters seed and query. A single package-level
// value, not a per-test constructor, because nothing about it varies
// between the PromQL / LogQL / TraceQL legs — pinning it once is what
// keeps the fixture small and deterministic rather than a per-test
// snowflake.
var Fixture = ResourceFixture{
	Team:      "checkout-team",
	Namespace: "checkout-prod",
	Timestamp: time.Date(2026, 6, 1, 12, 0, 0, 123456789, time.UTC),
}

const (
	// TeamKey is Fixture.Team's OTel resource-attribute key.
	TeamKey = "team"
	// NamespaceKey is Fixture.Namespace's OTel-dotted resource-attribute
	// key.
	NamespaceKey = "k8s.namespace.name"
	// NamespaceWireLabel is NamespaceKey's PromQL/LogQL sanitized wire
	// form (dot -> underscore). Pinned as a literal, not computed via
	// format.OTelToPromLabel, so the untagged companion test in this
	// package (resource_config_test.go) can assert the production
	// normalizer actually PRODUCES this literal, rather than the fixture
	// and the assertion silently sharing one derivation that could drift
	// together.
	NamespaceWireLabel = "k8s_namespace_name"
)

// TSFormat is the ClickHouse toDateTime64(_, 9) literal layout every
// per-head seed formats Fixture.Timestamp with — nanosecond precision,
// matching the DateTime64(9) columns the OTel-CH exporter schema (and
// every seed DDL in this package) declares.
const TSFormat = "2006-01-02 15:04:05.000000000"

// Per-head custom schema overrides (acceptance criterion: "Custom names
// are exercised through configured schemas rather than default-name
// fixtures" — one override per head). Declared here, in this package's
// only untagged, always-compiled file, rather than inside each
// chdb-tagged test file: resource_config_test.go's assertions and each
// chdb test's actual seed/query both need the SAME literal, and a
// build-tag split between "the value a config test checks" and "the value
// the chdb test actually uses" is exactly the drift this single
// declaration point forecloses. Table NAMES only — not shared
// seed/query/comparator logic, so each per-head chdb test still builds
// its own DDL, seed SQL, query string, and assertions independently.
const (
	// promqlFixtureMetricsTable overrides schema.Metrics.SumTable.
	promqlFixtureMetricsTable = "fixture_signal_resource_metrics_sum"
	// logqlFixtureLogsTable overrides schema.Logs.LogsTable.
	logqlFixtureLogsTable = "fixture_signal_resource_logs"
	// traceqlFixtureTracesTable overrides schema.Traces.SpansTable.
	traceqlFixtureTracesTable = "fixture_signal_resource_traces"
)

// promqlFixtureResourceLabels is the explicit CERBERUS_PROM_RESOURCE_LABELS
// allow-list the PromQL chdb test configures — see
// TestResourceFixture_ExplicitPromResourceLabelsAllowlist for why this is
// a non-nil, explicitly-named list rather than the nil "promote every
// key" default.
var promqlFixtureResourceLabels = []string{TeamKey, NamespaceKey}
