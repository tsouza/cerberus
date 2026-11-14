# tempopb

Tempo's protobuf wire types: the HTTP and gRPC request and response messages
cerberus's Tempo head speaks, and the `StreamingQuerier` gRPC service. The
`*.pb.go` and `*.proto` files and `LICENSE_APACHE2` are copied from `pkg/tempopb`
of `github.com/grafana/tempo` at the commit in
[`compatibility/tempo/UPSTREAM_VERSION`](../../../compatibility/tempo/UPSTREAM_VERSION);
[`SOURCE`](SOURCE) names the exact module version. This file,
`preallocbytes*.go` and `wire_*_test.go` are cerberus code.

That subtree is licensed Apache-2.0 by Grafana Labs
([`LICENSE_APACHE2`](LICENSE_APACHE2)); the rest of the Tempo module, which
cerberus does not copy or link, is AGPL-3.0. The repository `NOTICE` lists it.

The copy is regenerated, never edited by hand:

```sh
just tempopb-sync
```

`.github/scripts/tempopb-sync.mjs` copies the files, formats the Go files with
the repository's formatters (`gofumpt -extra`, `goimports -local`), and makes
three rewrites:
the import path becomes `github.com/tsouza/cerberus/internal/thirdparty/tempopb`;
the names registered in gogo/protobuf's global registry (and the `enum=`
struct tags that look them up) move from `tempopb.` to `cerberus.tempopb.`, so
a test binary that also links Tempo's own copy does not panic on a duplicate
enum; and each Go file carries a notice that it was modified. The wire encoding
and the gRPC service names are unchanged.

Two checks hold the copy to its source. `just tempopb-sync-check` (run by the
`agpl-clean` CI job) re-runs the sync into a scratch directory and fails on any
file that differs from the committed package. `wire_tempo_agpl_test.go`
(build tag `agpl_oracle`, run by the `agpl-oracle` lane) cross-encodes every
message the Tempo head uses between this package and Tempo's own `pkg/tempopb`
in both directions, comparing the two by field name.

The hand-written files beside the generated code upstream are not copied.
`pool.go` and `prealloc.go` back `PreallocBytes` with Tempo's ingester
byte-slice pool, which registers a Prometheus counter at init and reads
`PREALLOC_*` tuning variables from the environment; `preallocbytes.go` defines
the same type over a plain copy. `utils.go` and `trace_utils.go` hold helpers
cerberus does not call.
