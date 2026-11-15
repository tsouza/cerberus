# fast/

Empty for now. This directory exists because
`corpus.Load` (`compatibility/loki/corpus/definition.go`)
requires every suite subdirectory it's asked to load
(`fast/`, `regression/`, `exhaustive/`) to exist, even when a suite
currently carries zero cerberus-owned YAML files. See
`../README.md` for the overlay's purpose and layout.
