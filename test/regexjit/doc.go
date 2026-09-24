// Package regexjit holds the real-ClickHouse evidence for the native-code
// regular-expression compilation ClickHouse 26.7 added to `match`,
// `extract`, `extractAll`, `replaceRegexpOne` and `replaceRegexpAll`
// (`compile_regular_expressions`, on by default).
//
// The tests boot pinned clickhouse/clickhouse-server builds under container
// CPU and memory limits, seed cerberus's own OTel schema with adversarial
// label values and log lines, and drive PromQL and LogQL queries through the
// production handlers, so every regular expression the server evaluates is
// one cerberus emitted. For each query they require:
//
//   - the response with compilation forced on, with it forced off, and under
//     the server's default compile threshold across repeated requests, to be
//     byte-identical on valid UTF-8 input;
//   - the series or lines selected, and the labels label_replace writes, to
//     equal what the reference engines compute with Go's regexp package —
//     Prometheus's own matcher for selectors, Prometheus's label_replace
//     expansion, and Loki's unanchored line-filter match;
//   - the server to have compiled exactly the emitted shapes the corpus
//     marks as compilable, so the equality above is never vacuously RE2
//     against RE2.
//
// BenchmarkRegexJIT measures the same emitted shapes over large seeded
// tables: cold and warm requests, a changing pattern per request, and
// concurrent clients, with compilation on and off. It runs only under
// `-bench`; `just regex-jit-bench` is its entry point.
//
// The tests carry the `integration` build tag (Docker required) and run in
// the regex-jit job of strict-scan.yml through `just regex-jit-integration`.
package regexjit
