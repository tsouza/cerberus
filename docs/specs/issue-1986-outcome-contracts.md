# Spec: #1986 endpoint and error outcome contracts

Status: implemented and target-verified.

## Problem

The PromQL reference parity translator accepts `Sample` rows only. Metadata
catalog projections, query-exemplar fixtures, and duplicate-labelset failures
are therefore structurally outside that input model. Inventing samples to
enrol them would compare different operations and could hide an endpoint or
error-envelope regression.

## Scope

- Keep a closed inventory of the metadata, exemplar, and duplicate-labelset
  fixtures excluded from sample-row reference parity.
- Require each inventory class to retain its executable owner in the layer
  that actually emits the endpoint SQL or wire error.
- Reject an accidental `parity:` section on those fixtures. Existing chDB
  seeds and expected projection rows remain valid because they are not
  Prometheus `Sample` input to the reference translator.

## Task List

1. Complete: add the closed regression contract and its owner bindings.
2. Complete: verify the regression contract, the untagged metadata/exemplar
   owners, and the chDB duplicate-labelset wire-error owners.
3. Residual under #1986: these contracts compare Cerberus against fixed
   endpoint/error semantics, not a live Prometheus HTTP response. A live
   reference comparison remains unavailable here because the metadata and
   exemplar fixtures have no equivalent remote-write sample representation,
   while duplicate-labelset is an error outcome rather than a sample result.
