# exhaustive/

Empty for now. This directory exists because
`bench.QueryRegistry.Load` (`compatibility/loki/upstream/loki-bench/query_registry.go`)
requires every suite subdirectory it's asked to load
(`fast/`, `regression/`, `exhaustive/`) to exist, even when a suite
currently carries zero cerberus-owned YAML files. See
`../README.md` for the overlay's purpose and layout.
