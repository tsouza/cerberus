// Isolated nested module for the histogram benchmark harness.
//
// It deliberately does NOT belong to the parent `github.com/tsouza/cerberus`
// module: its own go.mod means `go build ./...` / `golangci-lint run` in the
// repo root skip this tree entirely, so the benchmark tooling can carry heavy
// deps (the ClickHouse driver) without touching cerberus's dependency graph.
//
// Because it is a separate module it cannot import cerberus's `internal/…`
// packages (Go forbids cross-module internal imports). The histogram table DDL
// and column layout are therefore inlined in cmd/gen — kept byte-compatible
// with what cerberus's own auto-create path (internal/schema/ddl) produces.
module cerberus-bench/histogram

go 1.26

require (
	github.com/ClickHouse/clickhouse-go/v2 v2.47.0
	github.com/golang/snappy v1.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/ClickHouse/ch-go v0.73.0 // indirect
	github.com/andybalholm/brotli v1.2.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-faster/city v1.0.1 // indirect
	github.com/go-faster/errors v0.7.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/paulmach/orb v0.13.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.27 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/sys v0.46.0 // indirect
)
