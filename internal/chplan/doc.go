// Package chplan defines the shared ClickHouse-leaning relational plan IR
// that all three heads (PromQL, LogQL, TraceQL) lower into.
//
// The IR is a tree of operators (Scan, Filter, Project, Aggregate,
// RangeWindow, Limit, Join, …) over which the optimizer rewrites and the
// chsql emitter walks. Lands in seed PR4.
package chplan
