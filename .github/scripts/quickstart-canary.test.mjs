// Negative-control unit tests for quickstart-canary.mjs. The failure direction
// is load-bearing: a required aggregate that reports green when selection or
// execution evidence is absent is worse than having no check, because branch
// protection appears satisfied while the published quickstart was never run.

import assert from "node:assert/strict";
import test from "node:test";

import {
  CANONICAL_DATASOURCES,
  CANONICAL_HEADS,
  COMPOSE_SEED_TABLE_WAIT_MS,
  DEFAULT_PROBE_TIMEOUT_MS,
  HOME_DASHBOARD,
  HOME_DASHBOARD_TARGETS,
  QUICKSTART_OPEN_COMMAND,
  QUICKSTART_UP_COMMAND,
  QUICKSTART_SEED,
  QUICKSTART_TEMPO_SEARCH_LIMIT,
  REGISTRY_RETRY_WRAPPER,
  classifyAggregate,
  classifyCheckout,
  probePlan,
  probeUntilReady,
  publishedQuickstart,
  quickstartSelectionPolicy,
  quickstartUpInvocation,
  selectQuickstart,
  validateDatasource,
  validateGrafanaHealth,
  validateGrafanaRoot,
  validateHealthz,
  validateHomeDashboard,
  validateHomeDashboardPrometheusTarget,
  validateHomeDashboardTempoTarget,
  validateLokiProxy,
  validatePrometheusProxy,
  validateProbeSnapshot,
  validateReadyz,
  validateSelectionBinding,
  validateTempoProxy,
} from "./quickstart-canary.mjs";

const sha = (character) => character.repeat(40);
const policy = quickstartSelectionPolicy(process.cwd());
const select = (eventName, changed) =>
  selectQuickstart({ eventName, changed, policy });
const pr = (changed) => select("pull_request", changed);
const response = (body, contentType = "application/json", status = 200) => ({
  status,
  headers: { "content-type": contentType },
  body: typeof body === "string" ? body : JSON.stringify(body),
});

const readmeQuickstart = ({
  clone = "git clone https://github.com/example/cerberus.git && cd cerberus",
  up = QUICKSTART_UP_COMMAND.join(" "),
  open = `${QUICKSTART_OPEN_COMMAND}   # Grafana`,
  extra = [],
} = {}) => `# Cerberus

## Quick start

Copy this:

\`\`\`sh
${clone}
${up}
${open}
${extra.join("\n")}
\`\`\`

## Next
`;

function healthyReadyz() {
  return response({
    clickhouse: "ok",
    schema: "ready",
    heads: Object.fromEntries(CANONICAL_HEADS.map((head) => [head, "closed"])),
  });
}

function healthyHomeDashboard() {
  const panels = new Map();
  for (const target of HOME_DASHBOARD_TARGETS) {
    if (!panels.has(target.panelID)) {
      panels.set(target.panelID, {
        id: target.panelID,
        datasource: {
          uid: target.datasourceUID,
          type: target.kind,
        },
        targets: [],
      });
    }
    panels.get(target.panelID).targets.push({
      refId: target.refID,
      datasource: {
        uid: target.datasourceUID,
        type: target.kind,
      },
      ...(target.kind === "tempo"
        ? { query: target.expression }
        : { expr: target.expression }),
    });
  }
  return {
    dashboard: {
      ...HOME_DASHBOARD,
      panels: [...panels.values()],
    },
  };
}

function healthyPrometheusProxy() {
  return response({
    status: "success",
    data: {
      resultType: "vector",
      result: [
        {
          metric: {
            __name__: QUICKSTART_SEED.prometheus.metric,
            job: QUICKSTART_SEED.prometheus.job,
          },
          value: [1234, String(QUICKSTART_SEED.prometheus.value)],
        },
      ],
    },
  });
}

function healthyLokiProxy() {
  return response({
    status: "success",
    data: {
      resultType: "streams",
      result: [
        {
          stream: { service_name: QUICKSTART_SEED.loki.serviceName },
          values: [["1234000000000", "seeded log"]],
        },
      ],
    },
  });
}

function healthyTempoProxy() {
  return response({ traces: [{ ...QUICKSTART_SEED.tempo }] });
}

function healthySnapshots(plan) {
  const snapshots = new Map();
  for (const probe of plan) {
    if (probe.id === "healthz")
      snapshots.set(probe.id, response("ok", "text/plain"));
    else if (probe.id === "readyz") snapshots.set(probe.id, healthyReadyz());
    else if (probe.id === "grafana-root") {
      snapshots.set(
        probe.id,
        response(
          "<!doctype html><html><head><title>Grafana</title></head></html>",
          "text/html",
        ),
      );
    } else if (probe.id === "grafana-health")
      snapshots.set(probe.id, response({ database: "ok" }));
    else if (probe.id === "proxy-cerberus-prometheus")
      snapshots.set(probe.id, healthyPrometheusProxy());
    else if (probe.id === "proxy-cerberus-loki")
      snapshots.set(probe.id, healthyLokiProxy());
    else if (probe.id === "proxy-cerberus-tempo")
      snapshots.set(probe.id, healthyTempoProxy());
    else if (probe.id === "home-dashboard") {
      snapshots.set(probe.id, response(healthyHomeDashboard()));
    } else if (probe.id.startsWith("home-panel-7-")) {
      snapshots.set(probe.id, response({ traces: [] }));
    } else if (probe.id.startsWith("home-panel-")) {
      snapshots.set(
        probe.id,
        response({
          status: "success",
          data: { resultType: "vector", result: [] },
        }),
      );
    } else {
      const datasource = CANONICAL_DATASOURCES.find(
        (item) => probe.id === `datasource-${item.uid}`,
      );
      snapshots.set(probe.id, response(datasource));
    }
  }
  return snapshots;
}

test("startup command is exactly the command published by the root quickstart", () => {
  assert.deepEqual(QUICKSTART_UP_COMMAND, [
    "docker",
    "compose",
    "up",
    "--wait",
  ]);
  const invocation = quickstartUpInvocation("/checkout", 1234);
  assert.equal(invocation.command, process.execPath);
  assert.deepEqual(invocation.args, [
    `/checkout/${REGISTRY_RETRY_WRAPPER}`,
    ...QUICKSTART_UP_COMMAND,
  ]);
  assert.equal(invocation.cwd, "/checkout");
  assert.equal(invocation.timeout, 1234);
});

test("functional probing outlives the seeder table wait with positive margin", () => {
  assert.ok(DEFAULT_PROBE_TIMEOUT_MS > COMPOSE_SEED_TABLE_WAIT_MS);
});

test("the checked-out README is bound to the exact startup and Grafana commands", () => {
  assert.equal(publishedQuickstart(readmeQuickstart()).ok, true);
  for (const [name, readme] of [
    ["different startup", readmeQuickstart({ up: "docker compose up" })],
    [
      "extra startup flag",
      readmeQuickstart({ up: "docker compose up --wait --profile demo" }),
    ],
    [
      "different browser endpoint",
      readmeQuickstart({ open: "open http://localhost:3001" }),
    ],
    [
      "different repository",
      readmeQuickstart({
        clone: "git clone https://github.com/example/other.git && cd other",
      }),
    ],
    ["extra command", readmeQuickstart({ extra: ["docker compose ps"] })],
    ["missing section", "# Cerberus\n\n```sh\ndocker compose up --wait\n```"],
    ["duplicate section", `${readmeQuickstart()}\n## Quick start\n`],
    ["unclosed fence", readmeQuickstart().replace("\n```\n\n## Next", "\n\n## Next")],
  ]) {
    assert.equal(publishedQuickstart(readme).ok, false, name);
  }
});

test("push always runs, independent of a documentation-looking diff", () => {
  const verdict = select("push", ["docs/operations.md"]);
  assert.equal(verdict.ok, true);
  assert.equal(verdict.selected, true);
});

test("every non-documentation PR and merge-group change runs", () => {
  for (const eventName of ["pull_request", "merge_group"]) {
    for (const path of [
      "internal/api/health/health.go",
      "docker-compose.yml",
      "Dockerfile.local",
      ".github/workflows/ci.yml",
      "compatibility/loki/cmd/seed/main.go",
      "test/e2e/grafana/compose/datasources/cerberus.yaml",
      "test/property/promql_test.go",
      "new-surface/file.unknown",
    ]) {
      const verdict = select(eventName, [path]);
      assert.equal(verdict.selected, true, `${eventName} ${path} must run`);
    }
  }
});

test("only an honestly documentation-only diff may omit the canary", () => {
  assert.equal(
    pr([
      "docs/engine.md",
      "docs/images/pipeline.svg",
      "CONTRIBUTING.md",
      "guide.markdown",
      "LICENSE",
      "LICENSE.notice",
      "CHANGELOG",
      "CLAUDE",
      "AGENTS.md",
      ".markdownlint.yaml",
      ".markdownlintignore",
    ]).selected,
    false,
  );
  assert.equal(
    pr(["docs/engine.md", "internal/chsql/emitter.go"]).selected,
    true,
  );
});

test("the root README always runs even though it is Markdown", () => {
  const verdict = pr(["README.md"]);
  assert.equal(verdict.selected, true);
  assert.match(verdict.reason, /root README/);
});

test("empty and uncomputable diffs run instead of manufacturing docs-only evidence", () => {
  assert.equal(pr([]).selected, true);
  assert.equal(pr(null).selected, true);
});

test("blank events fail selection; unfamiliar named events run fail-open", () => {
  assert.equal(
    selectQuickstart({ eventName: "", changed: [], policy }).ok,
    false,
  );
  assert.equal(select("future_event", ["docs/engine.md"]).selected, true);
  assert.equal(
    selectQuickstart({
      eventName: "pull_request",
      changed: ["internal/chsql/emitter.go"],
    }).ok,
    false,
  );
});

test("selection is bound to one full projected-checkout SHA", () => {
  assert.equal(
    validateSelectionBinding({ checkoutSha: sha("a"), headSha: sha("a") }).ok,
    true,
  );
  assert.equal(
    validateSelectionBinding({ checkoutSha: "", headSha: "" }).ok,
    false,
  );
  assert.equal(
    validateSelectionBinding({ checkoutSha: "abc", headSha: "abc" }).ok,
    false,
  );
  const mismatch = validateSelectionBinding({
    checkoutSha: sha("a"),
    headSha: sha("b"),
  });
  assert.equal(mismatch.ok, false);
  assert.match(mismatch.message, /differs/);
});

test("checkout verification requires the exact clean commit at repository root", () => {
  const good = {
    expectedSha: sha("c"),
    actualSha: sha("c"),
    status: "",
    root: "/repo",
    cwd: "/repo",
    requiredFilesPresent: true,
  };
  assert.equal(classifyCheckout(good).ok, true);
  assert.equal(classifyCheckout({ ...good, actualSha: sha("d") }).ok, false);
  assert.equal(
    classifyCheckout({ ...good, status: " M docker-compose.yml\n" }).ok,
    false,
  );
  assert.equal(classifyCheckout({ ...good, cwd: "/repo/subdir" }).ok, false);
  assert.equal(
    classifyCheckout({ ...good, requiredFilesPresent: false }).ok,
    false,
  );
});

test("/healthz requires HTTP 200 and the exact body", () => {
  assert.equal(validateHealthz(response("ok", "text/plain")), null);
  assert.match(validateHealthz(response("ok\n", "text/plain")), /exactly/);
  assert.match(validateHealthz(response("ok", "text/plain", 503)), /HTTP 503/);
});

test("/readyz requires ClickHouse, schema, and exactly three healthy heads", () => {
  assert.equal(validateReadyz(healthyReadyz()), null);
  const base = JSON.parse(healthyReadyz().body);
  for (const malformed of [
    { ...base, clickhouse: "error" },
    { ...base, schema: "pending" },
    { ...base, heads: { prom: "closed", loki: "closed" } },
    { ...base, heads: { ...base.heads, tempo: "open" } },
    { ...base, heads: { ...base.heads, extra: "closed" } },
  ]) {
    assert.notEqual(
      validateReadyz(response(malformed)),
      null,
      JSON.stringify(malformed),
    );
  }
  assert.match(validateReadyz(response("{broken")), /invalid JSON/);
});

test("Grafana root must be usable HTML, not merely any HTTP 200", () => {
  const html =
    "<!doctype html><html><head><title>Grafana</title></head><body></body></html>";
  assert.equal(
    validateGrafanaRoot(response(html, "text/html; charset=UTF-8")),
    null,
  );
  assert.match(validateGrafanaRoot(response("{}")), /content-type/);
  assert.match(
    validateGrafanaRoot(
      response("<html><title>Other</title></html>", "text/html"),
    ),
    /usable/,
  );
});

test("Grafana health requires database ok", () => {
  assert.equal(
    validateGrafanaHealth(response({ database: "ok", version: "12" })),
    null,
  );
  assert.match(
    validateGrafanaHealth(response({ database: "failed" })),
    /want "ok"/,
  );
  assert.match(
    validateGrafanaHealth(response("{}", "application/json", 503)),
    /HTTP 503/,
  );
});

test("all three canonical datasources must preserve uid, name, type, and backend URL", () => {
  assert.equal(CANONICAL_DATASOURCES.length, 3);
  for (const expected of CANONICAL_DATASOURCES) {
    assert.equal(validateDatasource(response(expected), expected), null);
    for (const field of ["uid", "name", "type", "url"]) {
      assert.notEqual(
        validateDatasource(
          response({ ...expected, [field]: "wrong" }),
          expected,
        ),
        null,
        `${expected.uid} ${field} mismatch must fail`,
      );
    }
  }
});

test("all three Grafana datasource proxies must return seeded user-visible data", () => {
  assert.equal(validatePrometheusProxy(healthyPrometheusProxy()), null);
  assert.equal(validateLokiProxy(healthyLokiProxy()), null);
  assert.equal(validateTempoProxy(healthyTempoProxy()), null);

  assert.match(
    validatePrometheusProxy(
      response({
        status: "success",
        data: { resultType: "vector", result: [] },
      }),
    ),
    /no seeded/,
  );
  assert.match(
    validatePrometheusProxy(
      response({
        status: "success",
        data: { resultType: "matrix", result: [{}] },
      }),
    ),
    /resultType/,
  );
  const wrongPrometheusSeed = JSON.parse(healthyPrometheusProxy().body);
  wrongPrometheusSeed.data.result[0].metric.job = "db";
  assert.match(
    validatePrometheusProxy(response(wrongPrometheusSeed)),
    /no exact seeded/,
  );
  assert.match(
    validateLokiProxy(
      response({
        status: "success",
        data: { resultType: "streams", result: [] },
      }),
    ),
    /no seeded/,
  );
  assert.match(
    validateLokiProxy(
      response({
        status: "success",
        data: { resultType: "streams", result: [{ stream: {}, values: [] }] },
      }),
    ),
    /no usable/,
  );
  const wrongLokiSeed = JSON.parse(healthyLokiProxy().body);
  wrongLokiSeed.data.result[0].stream.service_name = "frontend";
  assert.match(validateLokiProxy(response(wrongLokiSeed)), /no usable/);
  assert.match(validateTempoProxy(response({ traces: [] })), /no seeded/);
  assert.match(
    validateTempoProxy(
      response({ traces: [{ traceID: "other", rootServiceName: "frontend" }] }),
    ),
    /exact seeded/,
  );
  assert.match(
    validateTempoProxy(
      response({
        traces: [
          {
            traceID: QUICKSTART_SEED.tempo.traceID,
            rootServiceName: QUICKSTART_SEED.tempo.rootServiceName,
            rootTraceName: "wrong",
          },
        ],
      }),
    ),
    /wrong root identity/,
  );
  for (const validator of [
    validatePrometheusProxy,
    validateLokiProxy,
    validateTempoProxy,
  ]) {
    assert.match(validator(response("{broken")), /invalid JSON/);
    assert.match(validator(response({}, "application/json", 502)), /HTTP 502/);
  }
});

test("home dashboard probe proves the provisioned dashboard is active and query-bearing", () => {
  assert.equal(validateHomeDashboard(response(healthyHomeDashboard())), null);
  assert.match(
    validateHomeDashboard(
      response({ dashboard: { uid: "other", title: HOME_DASHBOARD.title } }),
    ),
    /uid/,
  );
  assert.match(
    validateHomeDashboard(
      response({ dashboard: { uid: HOME_DASHBOARD.uid, title: "Other" } }),
    ),
    /title/,
  );
  assert.match(
    validateHomeDashboard(
      response({ dashboard: { ...HOME_DASHBOARD, panels: [] } }),
    ),
    /no panels/,
  );
  const missingTempo = healthyHomeDashboard();
  missingTempo.dashboard.panels = missingTempo.dashboard.panels.filter(
    (panel) => panel.datasource.uid !== "cerberus-tempo",
  );
  assert.match(validateHomeDashboard(response(missingTempo)), /cerberus-tempo/);
  const driftedQuery = healthyHomeDashboard();
  driftedQuery.dashboard.panels[0].targets[0].expr = "up";
  assert.match(
    validateHomeDashboard(response(driftedQuery)),
    /target roster differs/,
  );
});

test("every home-dashboard expression must execute through its Grafana datasource proxy", () => {
  assert.equal(
    validateHomeDashboardPrometheusTarget(
      response({
        status: "success",
        data: { resultType: "vector", result: [] },
      }),
    ),
    null,
  );
  assert.equal(
    validateHomeDashboardTempoTarget(response({ traces: [] })),
    null,
  );
  assert.match(
    validateHomeDashboardPrometheusTarget(
      response({ status: "error", data: {} }),
    ),
    /status/,
  );
  assert.match(
    validateHomeDashboardPrometheusTarget(
      response({ status: "success", data: { resultType: "matrix" } }),
    ),
    /resultType/,
  );
  assert.match(
    validateHomeDashboardTempoTarget(response({ traces: null })),
    /traces must be an array/,
  );
});

test("probe plan covers liveness, readiness, Grafana, three live datasource queries, and home", () => {
  const plan = probePlan({
    cerberusURL: "http://gateway/",
    grafanaURL: "http://grafana/",
  });
  assert.deepEqual(
    plan.map((probe) => probe.url),
    [
      "http://gateway/healthz",
      "http://gateway/readyz",
      "http://grafana/",
      "http://grafana/api/health",
      ...CANONICAL_DATASOURCES.map(
        (item) => `http://grafana/api/datasources/uid/${item.uid}`,
      ),
      `http://grafana/api/datasources/proxy/uid/cerberus-prometheus/api/v1/query?query=${encodeURIComponent(`${QUICKSTART_SEED.prometheus.metric}{job="${QUICKSTART_SEED.prometheus.job}"}`)}`,
      `http://grafana/api/datasources/proxy/uid/cerberus-loki/loki/api/v1/query?query=${encodeURIComponent(`{service_name="${QUICKSTART_SEED.loki.serviceName}"}`)}`,
      `http://grafana/api/datasources/proxy/uid/cerberus-tempo/api/search?q=${encodeURIComponent(`{ resource.service.name = "${QUICKSTART_SEED.tempo.rootServiceName}" }`)}&limit=${QUICKSTART_TEMPO_SEARCH_LIMIT}`,
      "http://grafana/api/dashboards/home",
      ...HOME_DASHBOARD_TARGETS.map((target) => {
        const path =
          target.kind === "prometheus" ? "/api/v1/query?query=" : "/api/search?q=";
        const limit =
          target.kind === "tempo"
            ? `&limit=${QUICKSTART_TEMPO_SEARCH_LIMIT}`
            : "";
        return (
          `http://grafana/api/datasources/proxy/uid/${target.datasourceUID}${path}` +
          `${encodeURIComponent(target.expression)}${limit}`
        );
      }),
    ],
  );
  assert.deepEqual(validateProbeSnapshot(plan, healthySnapshots(plan)), []);
});

test("one missing or malformed HTTP result fails the whole snapshot", () => {
  const plan = probePlan({
    cerberusURL: "http://gateway",
    grafanaURL: "http://grafana",
  });
  for (const probe of plan) {
    const snapshots = healthySnapshots(plan);
    snapshots.delete(probe.id);
    assert.ok(
      validateProbeSnapshot(plan, snapshots).length > 0,
      `${probe.id} absence must fail`,
    );
  }
  const snapshots = healthySnapshots(plan);
  snapshots.set("readyz", { error: "connection refused" });
  assert.match(
    validateProbeSnapshot(plan, snapshots).join("\n"),
    /connection refused/,
  );
});

test("probe retry recovers only after a complete healthy snapshot", async () => {
  const plan = probePlan({
    cerberusURL: "http://gateway",
    grafanaURL: "http://grafana",
  });
  const good = healthySnapshots(plan);
  let clock = 0;
  let requests = 0;
  const firstAttemptRequests = plan.length;
  const verdict = await probeUntilReady({
    plan,
    timeoutMs: 100,
    requestTimeoutMs: 10,
    retryDelayMs: 5,
    now: () => clock,
    sleep: async (milliseconds) => {
      clock += milliseconds;
    },
    fetcher: async (_url, _timeout) => {
      const probe = plan[requests % plan.length];
      const attempt = Math.floor(requests / plan.length);
      requests += 1;
      if (attempt === 0 && probe.id === "readyz")
        return response(
          { clickhouse: "ok", schema: "pending", heads: {} },
          "application/json",
          503,
        );
      return good.get(probe.id);
    },
  });
  assert.equal(requests > firstAttemptRequests, true);
  assert.equal(verdict.ok, true);
  assert.equal(verdict.attempts, 2);
});

test("probe retry remains bounded and returns the final negative evidence", async () => {
  const plan = probePlan({
    cerberusURL: "http://gateway",
    grafanaURL: "http://grafana",
  });
  let clock = 0;
  const verdict = await probeUntilReady({
    plan,
    timeoutMs: 20,
    requestTimeoutMs: 5,
    retryDelayMs: 5,
    now: () => clock,
    sleep: async (milliseconds) => {
      clock += milliseconds;
    },
    fetcher: async () => {
      clock += 1;
      return { error: "still unavailable" };
    },
  });
  assert.equal(verdict.ok, false);
  assert.ok(verdict.attempts > 0);
  assert.match(verdict.problems.join("\n"), /still unavailable/);
  assert.ok(
    clock <= 20 + plan.length,
    `probe escaped its bound too far: clock=${clock}`,
  );
});

test("aggregate passes only a selected successful run", () => {
  assert.equal(
    classifyAggregate({
      selectResult: "success",
      selected: "true",
      runResult: "success",
    }).ok,
    true,
  );
  for (const runResult of [
    "",
    "failure",
    "cancelled",
    "skipped",
    "neutral",
    "timed_out",
  ]) {
    const verdict = classifyAggregate({
      selectResult: "success",
      selected: "true",
      runResult,
    });
    assert.equal(
      verdict.ok,
      false,
      `selected run result ${JSON.stringify(runResult)} must fail`,
    );
  }
});

test("aggregate permits skipped only after a successful not-impacted decision", () => {
  assert.equal(
    classifyAggregate({
      selectResult: "success",
      selected: "false",
      runResult: "skipped",
    }).ok,
    true,
  );
  for (const selectResult of ["", "failure", "cancelled", "skipped"]) {
    assert.equal(
      classifyAggregate({
        selectResult,
        selected: "false",
        runResult: "skipped",
      }).ok,
      false,
    );
  }
  for (const runResult of ["", "success", "failure", "cancelled"]) {
    assert.equal(
      classifyAggregate({
        selectResult: "success",
        selected: "false",
        runResult,
      }).ok,
      false,
    );
  }
});

test("aggregate rejects blank or malformed selector output", () => {
  for (const selected of ["", "TRUE", "yes", "null", "undefined"]) {
    assert.equal(
      classifyAggregate({
        selectResult: "success",
        selected,
        runResult: "skipped",
      }).ok,
      false,
      `selected=${JSON.stringify(selected)} must fail`,
    );
  }
});
