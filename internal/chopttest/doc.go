// Package chopttest wires the ClickHouse-native timeSeries*ToGrid lowering
// table against a REAL, already-connected chclient.Client, for real-CH
// integration tests that want a production prom.Handler configured the way a
// real deployment configures it — not prom.New's zero-value Lowerers, which
// promql.RangeLowerers.withDefaults normalizes to fan-out-only regardless of
// the connected server's version.
//
// # Why this package exists
//
// cmd/cerberus/main.go's own nativeRangeLowerers builds the boot-wired
// promql.RangeLowerers table from a probed chopt.EnabledSet, but it is
// unexported in `package main` and so cannot be imported. Before this
// package, every real-CH integration test that builds a prom.Handler
// constructed it as prom.New(client, schema, nil) and left .Lowerers at its
// zero value — confirmed for test/perf/smoke and
// internal/api/prom/handler_histogram_integration_test.go. That left six of
// the seven ts_grid_* native families (rate excepted only by
// test/perf/nightly's own narrowly-scoped nightlyClassicHistogramLowerer,
// which wires just the ONE field its histogram sentinel needs) permanently
// unreachable by any real-ClickHouse test in the repo outside a full binary
// boot (docker-compose / e2e / compose-smoke, none of which assert a
// specific native family actually activated) — issue #2487.
//
// BuildRangeLowerers is a deliberate, reviewed duplicate of
// nativeRangeLowerers's dispatch-table formula, not an import (it cannot be
// one). Keeping the two in lockstep is a reviewer-discipline concern, the
// same way nightlyClassicHistogramLowerer's own doc comment already flags
// for the one field it duplicates.
//
// AssertNativeFunctionFired is the activation-proof half: an HTTP 200 alone
// is indistinguishable from a silent fall-back to the fan-out path, so it
// reads the emitted SQL text back out of the real server's own
// system.query_log and asserts the expected native ts_grid_* function name
// (e.g. "timeSeriesRateToGrid") actually appears in it — the same
// "hollow green" precedent this repo's own project memory warns about: a
// passing test proves nothing about activation unless it looks at what was
// actually sent to ClickHouse.
package chopttest
