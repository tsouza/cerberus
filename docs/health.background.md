# Cerberus health probes — background

This document collects the design rationale behind
[`health.md`](health.md). It answers "why is it built this way" rather than
"what does it do" — nothing here is required to wire up or interpret the
probes correctly; `health.md` is self-sufficient for that.

## Why combined mode does not evict a pod with one tripped head

`/readyz`'s status code turns on head *exhaustion* rather than on any single
head's breaker. Under split mode the two are the same thing, so the choice
only shows in combined mode, where one tripped head leaves two working ones
behind.

Evicting the pod there would take those two down for a fault that is already
contained — the whole point of per-head breakers — and, since the breakers
trip on a shared ClickHouse, would tend to evict every replica at once. So
the phases are reported and the pod stays in its Service.

## Why heads this process does not serve are never counted

A head that was never built has no requests to fail, and counting it would
evict pods for a breaker nothing can reach.

## Why the startup benchmark ceiling is 2500 ms

The target is < 2000 ms from process spawn to first `200 OK` on `/healthz`.
The enforced ceiling adds a 500 ms safety margin on top of that target to
absorb CI scheduler jitter.

## Why the startup benchmark is informational rather than a required gate

Running `startup-bench` as a required PR gate would let a slow VM block
merges on a measurement that is sensitive to runner scheduling. Running it on
push-to-main, nightly and manual dispatch instead means a real regression —
for example a new synchronous startup hook that blocks the listener bind —
still shows up on the very next merge.

## Why an unreachable ClickHouse boots into unready instead of failing fast

Booting into unready is the readiness-gating contract Kubernetes expects: a
replica scaled up while ClickHouse is saturated waits out the outage out of
the Service endpoints instead of converting it into a CrashLoopBackOff.
Fail-fast is reserved for misconfiguration that can never succeed, which no
amount of waiting would resolve.
