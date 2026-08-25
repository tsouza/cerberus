//go:build chdb

package promql_test

// This file exists only on the release/1.15.x backport line. On main these
// two constants are defined in histogram_native_float_vector_scaling_binop_swap_chdb_test.go
// and histogram_native_mixed_or_vector_arithmetic_chdb_test.go respectively —
// but the commits that add those two files' own features (the scaling-join
// swap-side fix, #2541; the vector-vector mixed-or arithmetic family, #2449)
// were not selected for this backport batch, while several LATER mixed-or
// commits that WERE backported reuse these two constants as shared test
// fixtures. Copied verbatim from main rather than backporting either whole
// feature, since both are pure test data with no behavioral dependency on
// the code those two commits actually change.

// swapGaugeSeedDDL declares otel_metrics_gauge AND the empty otel_metrics_sum
// sibling the read path's merge(...) fan-out scans regardless of which one a
// query actually seeds.
const swapGaugeSeedDDL = "" +
	"CREATE OR REPLACE TABLE otel_metrics_gauge (`MetricName` String, `Attributes` Map(String, String), `ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"CREATE OR REPLACE TABLE otel_metrics_sum (`MetricName` String, `Attributes` Map(String, String), `ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"

// mvvQuantileBaseline is the histogram_quantile(0.5, ...) answer for the
// [1,2,3,4]/Count=10/Sum=10.0 bucket layout the mixed-or vector-arithmetic
// fixtures seed for every series whose float arm participates.
const mvvQuantileBaseline = 6.3496042078727974
