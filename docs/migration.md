# Migrating to cerberus

This guide walks you through moving a Prometheus-backed setup onto cerberus
(ClickHouse) **without rebuilding dashboards or rewriting alert rules**, and —
just as important — how to check what will change *before* you send real
traffic.

The tool that supports this guide is the `migrate` CLI. It is **read-only** and
**offline-first**: it never writes to Prometheus, Grafana, or ClickHouse.

---

## The one idea to hold onto

Your `prometheus.yml` does **not** tell you whether a migration will be smooth.
Metric names, label sets, and — the big one — **label cardinality** are produced
by your exporters at runtime, not declared in config.

So the migration is organised around your **real queries** (the PromQL in your
recording rules, alerting rules, and Grafana panels), not your config files.
The plan is: **lint your queries offline, then prove them against live data.**

---

## Before you start

You need three things:

1. **A ClickHouse instance** receiving your telemetry via the OpenTelemetry
   Collector's ClickHouse exporter (the same OTel-shaped tables cerberus reads).
2. **A dual-write / shadow period.** For a while, your data flows to Prometheus
   **and** to ClickHouse at the same time. This overlap is what makes a real
   before/after comparison possible. You never cut over cold.
3. **Your real queries**, reachable either as local files or through the Grafana
   API (see [Step 1](#step-1-harvest-your-real-queries)).

---

## The migration in five steps

```text
1. Harvest  →  2. Explain  →  3. Schema  →  4. Verify  →  5. Cut over
   (queries)    (offline)      (offline)     (live diff)    (flip URLs)
```

### Step 1: Harvest your real queries

Point the tool at where your queries live. Two options — pick whichever fits.

**Option A — local files** (works fully offline, air-gapped friendly):

```bash
migrate explain --rules ./prometheus/rules/ --dashboards ./grafana/dashboards/
```

- `--rules` — a file or directory of Prometheus recording/alerting rule files.
- `--dashboards` — a directory of exported Grafana dashboard JSON.

**Option B — live Grafana** (recommended — it sees the queries your teams
*actually* run, which files often miss):

```bash
export CERBERUS_GRAFANA_TOKEN=<service-account-token>   # Viewer role is enough
migrate explain --grafana-url https://grafana.internal --grafana-datasource prometheus-prod
```

- The token is **read-only** and is **never** printed or logged.
- `--grafana-datasource` scopes harvesting to the Prometheus datasource you are
  migrating. Panels pointing at other datasources (Loki, Tempo, …) are dropped
  with a count, never silently mixed in.

### Step 2: Read the explain report

For every query it harvested, `explain` shows:

- **The exact ClickHouse SQL** cerberus will run — byte-for-byte what the live
  server would execute.
- **The physical tables** the query touches.
- **A risk flag**, if the query's shape is dangerous (for example, an unbounded
  time window).
- Or an **`UNSUPPORTED`** marker, if the query cannot be translated yet.

Each row is labelled with where it came from (`grafana:<dashboard>`,
`grafana-alert`, or `rules-file`), so when something needs fixing you know
exactly which dashboard or rule to open.

The goal of this step is a clean list: everything either `OK`, flagged with a
reason you understand, or on the `UNSUPPORTED` list to address before cutover.

### Step 3: Render the schema

See the exact tables cerberus expects — offline, no database connection:

```bash
migrate --schema
```

This prints the `CREATE` statements cerberus would apply, straight from your
`CERBERUS_*` environment. It is pipeable into `clickhouse-client`:

```bash
migrate --schema | clickhouse-client -h clickhouse.internal --multiquery
```

Because it is rendered from the same code path the server uses at startup, what
you preview is exactly what you would get.

### Step 4: Verify against live data (the cutover gate)

Once your dual-write period is running, replay your harvested queries against
**both** backends over the same time window and diff the answers:

```bash
export CERBERUS_VERIFY_REFERENCE_URL=http://prometheus.internal:9090
export CERBERUS_VERIFY_CERBERUS_URL=http://cerberus.internal:9090
migrate verify --rules ./prometheus/rules/ --start -1h --end now --step 1m
```

For each query you get one of: `match`, `diverge`, `unsupported`, or `error`.
On a divergence it shows the **first** differing point (`series`, `timestamp`,
`reference value`, `cerberus value`) so you can chase it down.

Run it, fix each divergence, and re-run. **You are done when the diverge count
reaches zero.** That number is your permission to flip traffic — not a leap of
faith.

### Step 5: Cut over

- Point your Grafana datasource URL at cerberus (or swap the DNS / service in
  front of it). Dashboards and alert rules are unchanged — that is the whole
  point.
- Keep the dual-write running for a bit as a safety net.
- Monitor, then decommission the Prometheus write path when you are confident.

---

## What this tool will NOT tell you

Being honest about the blind spots is the point of the tool. It will never
pretend to know these:

- **Cardinality.** A query whose *shape* looks fine can still run out of memory
  on a metric with millions of label combinations. That number is not in any
  config or dashboard — it is runtime data. `explain` flags dangerous *shapes*;
  it never estimates row counts. For the real inventory, point cerberus at your
  running Prometheus's `/api/v1/metadata` and `/api/v1/status/tsdb`.
- **Whether the numbers match.** `explain` proving a query *translates* is not
  proof the *results* match your old Prometheus. Only **Step 4 (`verify`)**
  proves that — and only where both backends hold overlapping data.
- **Retention.** TTL is set by a launch flag, not by `prometheus.yml`, so the
  rendered schema uses no retention unless you pass one.

When a query is dropped, a variable can't be resolved, or a translation isn't
supported, the tool **counts and reports it**. It never silently skips.

---

## Troubleshooting

| Symptom                                                   | What it means                                                    | What to do                                                                |
| --------------------------------------------------------- | ---------------------------------------------------------------- | ------------------------------------------------------------------------- |
| A query is on the `UNSUPPORTED` list                      | cerberus can't translate that PromQL shape yet                   | Note the panel/rule; raise it — it's a real gap to fix at the source      |
| A panel is reported as `skipped: unresolved-template-var` | A Grafana `$variable` or macro couldn't be resolved              | Pass the variable's value, or accept that panel is checked at verify time |
| Non-Prometheus panels are dropped                         | They target Loki/Tempo/etc., not the datasource you're migrating | Expected — v1 harvests PromQL only                                        |
| `verify` shows a divergence                               | Real result difference between Prometheus and cerberus           | Read the first-diff point; fix the cause; re-run until zero               |
| An `explain` query errors at emit time                    | The query hit a resource-bound guard (e.g. unbounded window)     | That's cerberus refusing an unsafe query honestly — bound the query       |

---

## Scope (v1)

- **PromQL only.** LogQL and TraceQL panels are dropped with a count, not
  migrated.
- **Query-result parity**, not alert-firing parity. `verify` diffs query
  results — it does not re-implement `for:` durations or Alertmanager routing.
- **Read-only.** The tool never provisions schema or mutates Grafana; applying
  the rendered DDL is a deliberate, separate step you run yourself.
