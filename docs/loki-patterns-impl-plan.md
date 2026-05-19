# `/loki/api/v1/patterns` — implementation plan (drain wire-up)

Investigation-only doc. Plans the follow-up PRs that turn cerberus's currently
stubbed pattern endpoint (`internal/api/loki/patterns.go` returns
`{"data":{"patterns":[]}}`) into a real drain3-style template miner backed by
the upstream `grafana/loki/v3/pkg/pattern/drain` package.

## 1 — Drain package state

**Verdict: fully exported. No fork-accessor branch needed.**

`github.com/grafana/loki/v3/pkg/pattern/drain` lives at
`/home/thiago/go/pkg/mod/github.com/tsouza/loki/v3@v3.0.0-cerberus-parser/pkg/pattern/drain/`
through the existing `replace` directive in `go.mod`. `go doc` against the
package surfaces:

```text
const FormatLogfmt = "logfmt" ; FormatJSON = "json" ; FormatUnknown = "unknown"
const TimeResolution = model.Time(int64(time.Second*10) / 1e6)   // 10 s
func DetectLogFormat(line string) string
type Config struct { LogClusterDepth, SimTh, MaxChildren, ExtraDelimiters,
                     MaxClusters, ParamString, MaxEvictionRatio,
                     MaxAllowedLineLength int / float64 / []string ;
                     MaxChunkAge, SampleInterval time.Duration }
func DefaultConfig() *Config
type Limits interface { PatternIngesterTokenizableJSONFields(userID string) []string }
type Metrics struct { ... prometheus.Counter / Observer fields ... }
type Drain struct{ ... }
    func New(tenantID string, config *Config, limits Limits, format string, metrics *Metrics) *Drain
    func (d *Drain) Train(content string, ts int64) *LogCluster
    func (d *Drain) Clusters() []*LogCluster
    func (d *Drain) Delete(cluster *LogCluster)
    func (d *Drain) Prune()
type LogCluster struct{ Size; Tokens; TokenState; Stringer; Volume; SampleCount; Chunks ; ... }
    func (c *LogCluster) String() string
    func (c *LogCluster) Samples() []*logproto.PatternSample
    func (c *LogCluster) Iterator(lvl string, from, through, step, sampleInterval model.Time) iter.Iterator
    func (c *LogCluster) Prune(olderThan time.Duration) []*logproto.PatternSample
```

Everything cerberus needs (constructor + per-line `Train` + cluster
enumeration + per-cluster sample read-out) is already an exported method or
type. The Loki repo itself wires the drain instance directly inside
`pkg/pattern/stream.go` (`pattern.Train(entry.Line, entry.Timestamp.UnixNano())`)
and reads clusters back in `(s *stream).Iterator` — cerberus mirrors that
loop verbatim.

**Licensing note.** `drain.go` carries its own MIT header (faceair drain3
port, 2022). The wider Loki repo is AGPL-3.0. Cerberus already imports
several `grafana/loki/v3/pkg/...` paths (`logql/syntax`, `logql/log`,
`logqlmodel`, `logproto`, `logql/log/pattern`, `logql/log/jsonexpr`) — adding
`pkg/pattern/drain` does **not** widen the existing AGPL exposure footprint.
The fork's purpose (Dependabot watch boundary on a narrow subtree) does not
need to change.

## 2 — API cerberus will call

The handler stays thin: build a peek-window SQL (mirror
`buildDetectedFieldsSQL`), stream rows through `QueryStrings`, feed each
line to `Drain.Train`, then emit the resulting clusters as
`PatternSeries`. The signatures cerberus binds against:

```go
import (
    drainpkg "github.com/grafana/loki/v3/pkg/pattern/drain"
    "github.com/prometheus/common/model"
)

cfg := drainpkg.DefaultConfig()  // sane drain3 defaults; MaxChunkAge / SampleInterval set
d   := drainpkg.New(
    /*tenantID*/ "",            // single-tenant cerberus; the tenant string only flows to Limits
    cfg,
    /*limits*/   noLimits{},    // returns nil tokenizable-JSON-fields slice
    drainpkg.FormatUnknown,     // or drainpkg.DetectLogFormat(firstLine) for json/logfmt awareness
    /*metrics*/  nil,           // nil is supported throughout drain.go (guarded everywhere)
)

for _, line := range linesFromCH {              // most-recent-first peek window
    d.Train(line, peekTimestampNanos(line))     // ts is for chunk bucketing
}

resp := patternsResponse{Status: "success"}
for _, c := range d.Clusters() {
    if c.String() == "" { continue }
    series := patternSeries{
        Pattern: c.String(),
        Level:   "",                            // level inference deferred — see § 5
        Samples: projectSamples(c.Samples(), reqStart, reqEnd, reqStep),
    }
    resp.Data = append(resp.Data, series)
}
```

The `Limits` interface needs a one-method stub
(`PatternIngesterTokenizableJSONFields(userID string) []string` — return nil).
`Metrics` is a struct of `prometheus.Counter` / `Observer` interfaces; every
`drain.go` callsite is nil-guarded so passing `nil` is fine for the first
cut. A later PR can wire a non-nil `*drainpkg.Metrics` into cerberus's own
`telemetry` package once the endpoint demonstrates value in production.

## 3 — Step-by-step implementation plan

### PR A — wire format alignment + handler skeleton (no drain yet)

The current stub returns `{"data":{"patterns":[...]}}` but the upstream Loki
JSON contract (verified against
`pkg/util/marshal/marshal.go:WriteQueryPatternsResponseJSON` in the fork) is:

```json
{
  "status": "success",
  "data": [
    {"pattern": "GET /api/...", "level": "info",
     "samples": [[ts_unix_seconds, count], ...]},
    ...
  ]
}
```

— `data` is the top-level array, not `data.patterns`. Each `series` carries
`pattern` + `level` + `samples`, and each sample is a 2-tuple
`[unix_seconds, count]` (NOT unix_ms — upstream calls `sample.Timestamp.Unix()`
which strips the ms component since `model.Time` is itself a `ms`-typed
int64). The current `[2]int64` shape in `patterns.go` claims "unix_ms" — the
unit comment is wrong; the actual emit needs `.Unix()` semantics.

PR A is a pure wire-format fix:

- Rename `PatternsData` → drop the wrapper struct; emit `data` as a slice
  directly (mirrors what `WriteQueryPatternsResponseJSON` does).
- Add `Level` field to the `Pattern` struct.
- Document the sample units as unix seconds (not ms).
- Switch the handler to return `Response{Status: "success", Data:
  []Pattern{}}` — still the empty-array contract Grafana renders gracefully.
- Add `step` query-param validation lifted from
  `pkg/loghttp/patterns.go:ParsePatternsQuery` (step > 0; ≤ 11000 buckets).

Update `patterns_test.go` to assert the new top-level array shape. No drain
import yet — the test surface is the wire-format alignment alone.

### PR B — drain wire-up

The actual integration. Mirrors `handleDetectedFields` (peek window via
`Client.QueryStrings`, in-Go post-process, return JSON envelope) but the
post-process step trains a `*drainpkg.Drain` instead of the JSON / logfmt
heuristics.

SQL (mirrors `buildDetectedFieldsSQL` but also fetches `Timestamp` so the
drain has a real `ts` for chunk bucketing):

```sql
SELECT `Timestamp` AS ts, `Body` AS line
FROM `otel_logs`
WHERE <matchers> AND <time bounds>
ORDER BY `Timestamp` DESC
LIMIT <peek_limit>          -- new default: defaultPatternsLineLimit = 1000
```

Add a `QueryTimestampedLines(ctx, sql, args...) ([]TimestampedLine, error)`
method to `internal/api/loki.Querier` (and the corresponding `chclient.Client`
impl). The new type:

```go
type TimestampedLine struct {
    TS   int64   // unix nanoseconds (matches drain.Train's contract)
    Line string
}
```

Handler body:

```go
func (h *Handler) handlePatterns(w http.ResponseWriter, r *http.Request) {
    // ... existing param parse + step validation from PR A ...

    sqlStr, args, err := buildPatternsSQL(h.Schema, matchers, start, end, peekLimit)
    // ... CH query, error handling, debug log mirror handleDetectedFields ...
    rows, err := h.Client.QueryTimestampedLines(r.Context(), sqlStr, args...)

    cfg := drainpkg.DefaultConfig()
    d   := drainpkg.New("", cfg, noLimits{}, drainpkg.FormatUnknown, nil)
    for _, row := range rows {
        d.Train(row.Line, row.TS)
    }

    series := patternSeries(d.Clusters(), start, end, step)  // builds []Pattern
    writeJSON(w, http.StatusOK, Response{Status: "success", Data: series})
}
```

Add `defaultPatternsLineLimit = 1000` (matching upstream Loki's default for
the related `Push` window) and a `&line_limit=` query param so power users
can scale up if their pattern density is high.

### PR C — TXTAR fixture + chDB roundtrip

The existing metadata endpoints (`detected_fields`, `index_stats`, `labels`)
are covered by `internal/api/loki/*_test.go` against a stub Querier — they do
**not** carry a TXTAR fixture under `test/spec/logql/`. The pattern PR follows
the same convention:

- `internal/api/loki/patterns_test.go` — table-driven against `stubQuerier`
  feeding canned lines into the handler. Assert that semantically-equal lines
  collapse into one pattern (e.g.
  `["GET /api/users/1 200", "GET /api/users/2 200", "GET /api/users/42 200"]`
  yields one `pattern` series with `count >= 3`).
- `internal/api/loki/patterns_chdb_test.go` — build-tagged `chdb`. Seeds a
  small `otel_logs` table, runs the handler end-to-end, asserts the response
  carries the expected number of distinct patterns. Match the existing
  `detected_fields_test.go` layout where the chdb-tagged path exercises a
  realistic peek window.

If the team prefers a TXTAR-style golden, add one under
`test/spec/logql/loki_patterns_basic.txtar` with `-- seed --` + `-- input --`
+ `-- expected_rows --` sections, exercised by the `runner_chdb.go` driver.
The metadata-endpoint precedent (`detected_fields`, etc.) is "no TXTAR",
so PR C defaults to the `_test.go` approach.

## 4 — Test plan: determinism

Drain is deterministic given (a) the same `Config`, (b) the same line input
order, and (c) a single-threaded `Train` loop. The handler will use
`DefaultConfig()` unchanged, so determinism reduces to "feed lines in the
same order every time". The SQL emitted is
`ORDER BY Timestamp DESC LIMIT N` — chDB returns rows in deterministic order
given that ORDER BY. Test assertions:

- **Pattern count is exact.** Given canned lines whose token shapes differ
  by exactly one slot (e.g. `GET /api/foo`, `GET /api/bar`), expect exactly
  one pattern after `Train`. Lines whose token shapes differ by more than
  the `SimTh` distance produce distinct clusters.
- **Sample count matches input lines.** Sum of `samples[*].value` across all
  clusters equals the number of lines pushed (assuming none are skipped via
  the limiter — the default `MaxClusters` is generous enough that the
  limiter doesn't trip on a 1000-line peek window).
- **String stability — assert structure, not exact string.** Drain's
  `Stringer` produces tokens like `GET /api/<*> 200` where `<*>` is the
  variable-token placeholder controlled by `cfg.ParamString`. Tests assert
  via `strings.Contains(pattern, "GET")` + `strings.Contains(pattern, "200")`
  + a `strings.Count(pattern, "<*>")` constraint, not against a frozen
  exact-string. This avoids golden churn if drain's tokenizer changes its
  whitespace handling across upstream bumps.
- **Empty input is empty output.** `QueryStrings` returning zero rows
  produces `{"status":"success","data":[]}`. No nil-vs-empty-slice JSON
  drift (the existing detected_fields path uses the same idiom).
- **Sample bucketing follows step.** Given lines at `t`, `t+5s`, `t+15s`
  and `step=10s`, expect samples bucketed at `t-mod-10s` and `t+10s` (drain
  uses `TruncateTimestamp(ts, step)`).

## 5 — Open questions / risk areas

- **Level inference.** Upstream Loki bucket-by-level uses
  `entry.StructuredMetadata` — an OTel attribute set that cerberus does not
  fetch in the peek SQL. Easiest path: project `SeverityText` alongside
  `Body` + `Timestamp`, lowercase it (matching upstream's
  `strings.ToLower(metadata.Get(constants.LevelLabel))`), and train one
  drain instance per level (mirrors `s.patterns[lvl]` in
  `pkg/pattern/stream.go`). Alternative: one drain instance for all lines
  and emit `"level":""` for every series — Grafana renders both. PR B
  defaults to the multi-instance "by SeverityText" path; PR C exercises both
  the with-level and without-level branches.
- **Memory ceiling.** `DefaultConfig()` caps `MaxClusters` at 300; with one
  drain instance per level (~5 levels), peak resident state is bounded at
  ~1500 cluster nodes per request, which is small. The peek-line limit
  (1000) caps `Train` calls. No streaming-cursor concern (this is a peek,
  not a `query_range` matrix); the response itself is shaped by cluster
  count × samples-per-cluster, both bounded.
- **Throughput vs. peek-window staleness.** A 1000-line peek over a 1-day
  window samples patterns sparsely — busy streams emit far more than 1000
  lines/day. Grafana's pattern panel is "indicative, not exhaustive" so this
  matches user expectations; document the peek-line behaviour in the
  handler doc-comment.
- **Step lower bound.** `drain.TimeResolution` is 10 s. If the caller
  requests step < 10 s, upstream Loki clamps `step` to `TimeResolution`.
  PR A's step validation already enforces step > 0; PR B adds the
  `if step < drain.TimeResolution { step = drain.TimeResolution }` clamp
  mirroring `pattern/instance.go:Iterator`.
- **`logproto.PatternSample` projection unit.** Drain stores
  `Timestamp model.Time` (milliseconds since epoch). The wire format emits
  `.Unix()` (seconds). The projection helper must convert via
  `int64(model.Time(sample.Timestamp)) / 1000` — easy but easy to
  miss-shift. PR B includes a unit test on the conversion helper alone.
- **Long-lived drain state.** Upstream Loki maintains drain state across
  requests (it lives in the pattern-ingester process). Cerberus is
  stateless — every request rebuilds the drain from scratch over a fresh
  peek window. This trades latency (a few ms per `Train` call × 1000
  lines) for the architectural simplicity of "no cross-request state". The
  CLAUDE.md "No caching" rule reinforces the stateless choice: drain is a
  per-request artefact, not a memoization layer.
- **Test-stub vs. real chDB difference.** `stubQuerier.QueryStrings` returns
  a pre-baked slice in order; chDB returns rows in `ORDER BY` order. PR C
  needs both — the table-driven `_test.go` confirms the drain wiring;
  the `chdb`-tagged path confirms SQL emission shapes correctly.
