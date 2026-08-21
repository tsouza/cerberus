// Package profile is the corpus-wide perf profiler (Component B of the
// perf-assessment framework).
//
// # Why this exists
//
// The Phase-1 perf audit (wf_a7e317b9) found that every perf bug
// cerberus shipped was COMPUTE FAN-OUT: the table scan read a normal
// number of rows, but an intermediate pipeline stage (a CROSS JOIN, an
// ARRAY JOIN, a range-window cross product, a recursive-CTE closure)
// exploded the row count between the scan and the final result, blowing
// up peak memory. The existing reactive guards under test/perf/ each pin
// ONE construct a human already found broken (range_lwr_scaling,
// histogram_range_scaling, setop_chain_scaling, …). They share a style
// but they only cover constructs someone remembered to register — a
// fan-out in an unregistered construct sails through.
//
// This profiler closes that gap with corpus-wide BREADTH. It walks every
// committed TXTAR fixture that is executable (declares `seed:` +
// `expected_rows:` + `sql:`), and for each one measures the fan-out
// signal directly in chDB:
//
//   - EXPLAIN PLAN actions=1 — detect the fan-out OPERATORS (CROSS JOIN,
//     ARRAY JOIN, recursive CTE) structurally, before they run.
//   - a per-subquery-level count() decomposition — peak INTERMEDIATE
//     cardinality vs the leaf SCAN row count. fan_factor =
//     peak_intermediate / scan_rows is the headline fan-out number.
//   - the chDB native per-query stats (bytes_read as the peak-memory
//     proxy; chDB's embedded engine does not expose query_log /
//     peak_memory_usage, so bytes_read is the closest available signal).
//
// It emits one [Record] per fixture. The nightly perf-profile.yml lane
// runs the whole corpus, uploads the JSON, and surfaces the highest
// fan_factor fixtures in the step summary — so a newly-introduced fan-out
// shows up as an outlier even in a construct no per-construct guard
// covers.
//
// # chDB seam reuse
//
// Seeding + SQL rewriting go through the SAME pipeline the round-trip
// assertion uses (test/spec.PrepareRoundTrip), so the SQL profiled here
// is byte-identical to the SQL CI executes. The profiler does not invent
// its own seed or its own now64/Map rewrites.
//
// # Build-tag split
//
// [Record], [LevelCount] and [SortByFanFactor] (record.go) carry NO
// `//go:build chdb` tag: they are plain data plus a comparison, with no
// chDB dependency, so anything that only needs to read/merge/rank
// profiles someone else already collected — cmd/perf-profile's `-merge`
// mode, in particular — can import this package without pulling in
// libchdb.so. Everything that actually QUERIES chDB (profile.go,
// corpus.go) stays tagged.
package profile
