// quickstart-canary.mjs — the fast, required proof that the repository's
// published quickstart still works on the exact tree proposed for main.
//
// Modes (MODE, or argv[2]):
//   select          bind path selection to the projected checkout and emit
//                   `selected`, `checkout_sha`, and `reason` to GITHUB_OUTPUT.
//   verify-checkout prove the checkout is the requested clean commit and still
//                   contains the two root quickstart entrypoints.
//   up              run the published startup command, from the repository
//                   root, through the registry-transport retry wrapper.
//   probe           retry the user-visible HTTP contract until every service
//                   and provisioned Grafana object is usable.
//   aggregate       turn selection + execution results into one stable,
//                   fail-closed required check.
//
// Environment:
//   EVENT_NAME              select: github.event_name.
//   BASE_SHA                select: projected change's base SHA.
//   HEAD_SHA                select: projected change's head SHA. When present,
//                           it must equal CHECKOUT_SHA.
//   CHECKOUT_SHA            select: exact projected commit to check out.
//   EXPECTED_SHA            verify-checkout: exact commit expected at HEAD.
//   QUICKSTART_UP_TIMEOUT_MS
//                           up: hard outer startup bound (default 720000).
//   CERBERUS_URL            probe: gateway base URL (default localhost:8080).
//   GRAFANA_URL             probe: Grafana base URL (default localhost:3000).
//   QUICKSTART_PROBE_TIMEOUT_MS
//                           probe: whole-probe retry bound (default 180000).
//   SELECT_RESULT           aggregate: needs.select.result.
//   SELECTED                aggregate: needs.select.outputs.selected.
//   RUN_RESULT              aggregate: needs.run.result.
//
// Exit: 0 only when the selected mode's full contract holds. Unknown modes,
// missing setup evidence, malformed selector output, timeouts, and every
// selected job result other than `success` fail closed.
//
// node: builtins only — no npm dependencies or setup-node step.

import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import process from "node:process";
import { pathToFileURL } from "node:url";

import { capture, error, log, notice, setOutput } from "./lib/gh.mjs";
import { changedPaths } from "./lib/scope-gate.mjs";
import { loadRegistry, matchesGlob } from "./ci-lane-contract.mjs";

export const MODES = Object.freeze([
  "select",
  "verify-checkout",
  "up",
  "probe",
  "aggregate",
]);
export const DIFF_SCOPED_EVENTS = Object.freeze([
  "pull_request",
  "merge_group",
]);
export const QUICKSTART_LANE_ID = "e2e.quickstart";

// This literal is load-bearing. A structural regression test binds it to the
// root README command, and MODE=up passes these four argv elements through
// unchanged. In particular, there is no profile, override file, service subset,
// or extra flag that could make CI exercise a different stack from a new user.
export const QUICKSTART_UP_COMMAND = Object.freeze([
  "docker",
  "compose",
  "up",
  "--wait",
]);
export const QUICKSTART_OPEN_COMMAND = "open http://localhost:3000";
export const REGISTRY_RETRY_WRAPPER =
  ".github/scripts/build-with-registry-retry.mjs";

export const DEFAULT_UP_TIMEOUT_MS = 12 * 60 * 1000;
// The Compose seeder gives the collector-owned tables three minutes to appear.
// The functional probe must outlive that internal wait with room to observe the
// first inserted rows; an equal deadline races the seeder at its failure edge.
export const COMPOSE_SEED_TABLE_WAIT_MS = 3 * 60 * 1000;
export const PROBE_SEED_READINESS_MARGIN_MS = 2 * 60 * 1000;
export const DEFAULT_PROBE_TIMEOUT_MS =
  COMPOSE_SEED_TABLE_WAIT_MS + PROBE_SEED_READINESS_MARGIN_MS;
export const DEFAULT_REQUEST_TIMEOUT_MS = 5 * 1000;
export const DEFAULT_RETRY_DELAY_MS = 2 * 1000;
export const QUICKSTART_TEMPO_SEARCH_LIMIT = 20;

export const CANONICAL_HEADS = Object.freeze(["prom", "loki", "tempo"]);
export const CANONICAL_DATASOURCES = Object.freeze([
  Object.freeze({
    uid: "cerberus-prometheus",
    name: "Cerberus-Prometheus",
    type: "prometheus",
    url: "http://cerberus:8080",
  }),
  Object.freeze({
    uid: "cerberus-loki",
    name: "Cerberus-Loki",
    type: "loki",
    url: "http://cerberus:8080",
  }),
  Object.freeze({
    uid: "cerberus-tempo",
    name: "Cerberus-Tempo",
    type: "tempo",
    url: "http://cerberus:8080",
  }),
]);
export const HOME_DASHBOARD = Object.freeze({
  uid: "cerberus-self",
  title: "Cerberus",
});
export const HOME_DASHBOARD_DATASOURCES = Object.freeze([
  "cerberus-prometheus",
  "cerberus-tempo",
]);
// Every query a user opening the provisioned home dashboard asks Grafana to
// execute. The metadata validator below binds this roster back to the live
// provisioned dashboard, and probePlan replays each expression through the
// same Grafana datasource proxy it names. A dashboard edit therefore cannot
// silently leave the required canary probing an unrelated canned query.
export const HOME_DASHBOARD_TARGETS = Object.freeze([
  Object.freeze({
    panelID: 1,
    refID: "A",
    datasourceUID: "cerberus-prometheus",
    kind: "prometheus",
    expression:
      "sum by (cerberus_ql) (rate(cerberus_queries_total[5m]))",
  }),
  Object.freeze({
    panelID: 2,
    refID: "A",
    datasourceUID: "cerberus-prometheus",
    kind: "prometheus",
    expression:
      "histogram_quantile(0.95, sum by (le, cerberus_ql) (rate(cerberus_queries_duration_seconds_bucket[5m])))",
  }),
  Object.freeze({
    panelID: 3,
    refID: "A",
    datasourceUID: "cerberus-prometheus",
    kind: "prometheus",
    expression:
      'sum by (cerberus_ql) (rate(cerberus_queries_total{result="error"}[5m])) / clamp_min(sum by (cerberus_ql) (rate(cerberus_queries_total[5m])), 1e-9)',
  }),
  Object.freeze({
    panelID: 4,
    refID: "A",
    datasourceUID: "cerberus-prometheus",
    kind: "prometheus",
    expression: "sum by (cerberus_ql) (cerberus_query_inflight)",
  }),
  Object.freeze({
    panelID: 5,
    refID: "A",
    datasourceUID: "cerberus-prometheus",
    kind: "prometheus",
    expression:
      "sum by (cerberus_ql, budget, reason) (rate(cerberus_admit_rejected_total[5m]))",
  }),
  Object.freeze({
    panelID: 6,
    refID: "A",
    datasourceUID: "cerberus-prometheus",
    kind: "prometheus",
    expression:
      'sum by (cerberus_status_class) (rate(cerberus_queries_total{result="error"}[5m]))',
  }),
  Object.freeze({
    panelID: 7,
    refID: "A",
    datasourceUID: "cerberus-tempo",
    kind: "tempo",
    expression:
      '{ resource.service.name = "cerberus" && duration > 100ms }',
  }),
  Object.freeze({
    panelID: 8,
    refID: "A",
    datasourceUID: "cerberus-prometheus",
    kind: "prometheus",
    expression:
      'sum by (cerberus_error_reason) (rate(cerberus_queries_total{result="error"}[5m]))',
  }),
]);
export const QUICKSTART_SEED = Object.freeze({
  prometheus: Object.freeze({ metric: "up", job: "api", value: 1 }),
  loki: Object.freeze({ serviceName: "api" }),
  tempo: Object.freeze({
    traceID: "a0000000000000000000000000000001",
    rootServiceName: "frontend",
    rootTraceName: "GET /home",
  }),
});

const fullSHA = /^[0-9a-f]{40}$/;
const cloneAndEnterRepository =
  /^git clone https:\/\/github\.com\/[^/\s]+\/cerberus\.git && cd cerberus$/;

function shellCommands(lines) {
  return lines
    .map((line) => line.split("#", 1)[0].trim())
    .filter(Boolean);
}

/** Parse and validate the shell fence users copy from README's Quick start. */
export function publishedQuickstart(readme) {
  const lines = String(readme ?? "").split(/\r?\n/);
  const headings = lines
    .map((line, index) => [line.trim(), index])
    .filter(([line]) => line === "## Quick start");
  if (headings.length !== 1) {
    return {
      ok: false,
      message: `README must contain exactly one Quick start section (found ${headings.length})`,
    };
  }

  const sectionStart = headings[0][1] + 1;
  let fenceStart = -1;
  for (let index = sectionStart; index < lines.length; index += 1) {
    const line = lines[index].trim();
    if (line.startsWith("## ")) break;
    if (line === "```sh" || line === "```bash") {
      fenceStart = index + 1;
      break;
    }
  }
  if (fenceStart < 0) {
    return {
      ok: false,
      message: "README Quick start must contain one sh or bash command fence",
    };
  }

  let fenceEnd = -1;
  for (let index = fenceStart; index < lines.length; index += 1) {
    if (lines[index].trim() === "```") {
      fenceEnd = index;
      break;
    }
  }
  if (fenceEnd < 0) {
    return {
      ok: false,
      message: "README Quick start command fence is not closed",
    };
  }

  const commands = shellCommands(lines.slice(fenceStart, fenceEnd));
  if (commands.length !== 3) {
    return {
      ok: false,
      message: `README Quick start must contain exactly three commands (found ${commands.length})`,
    };
  }
  if (!cloneAndEnterRepository.test(commands[0])) {
    return {
      ok: false,
      message:
        "README Quick start must first clone this repository over HTTPS and enter cerberus",
    };
  }
  if (commands[1] !== QUICKSTART_UP_COMMAND.join(" ")) {
    return {
      ok: false,
      message:
        "README Quick start startup command is not bound to the required canary invocation",
    };
  }
  if (commands[2] !== QUICKSTART_OPEN_COMMAND) {
    return {
      ok: false,
      message:
        "README Quick start browser command is not bound to the Grafana endpoint probed by the canary",
    };
  }
  return { ok: true, message: "published quickstart contract is bound" };
}

export function normalisePath(path) {
  return String(path ?? "")
    .replace(/\\/g, "/")
    .replace(/^\.\//, "")
    .replace(/\/+$/, "");
}

export function quickstartSelectionPolicy(root) {
  const registry = loadRegistry(".github/ci-lanes.json", { root });
  const lane = registry.lanes.find(
    (candidate) => candidate.id === QUICKSTART_LANE_ID,
  );
  if (!lane)
    throw new Error(`CI lane registry has no ${QUICKSTART_LANE_ID} lane`);
  return Object.freeze({
    quickstartGlobs: Object.freeze([...lane.package_globs]),
    knownNonimpactGlobs: Object.freeze([
      ...registry.impact_selection.known_nonimpact_globs,
    ]),
  });
}

function matchesAnyGlob(path, globs) {
  return globs.some((glob) => matchesGlob(path, glob));
}

/** Pure selection over an already-computed changed-path set. */
export function selectQuickstart({ eventName, changed, policy }) {
  const event = String(eventName ?? "").trim();
  if (!event) {
    return {
      ok: false,
      selected: null,
      reason: "EVENT_NAME is blank; the selector cannot identify its event",
    };
  }

  if (!DIFF_SCOPED_EVENTS.includes(event)) {
    return {
      ok: true,
      selected: true,
      reason: `event "${event}" always runs the quickstart canary`,
    };
  }

  if (changed === null) {
    return {
      ok: true,
      selected: true,
      reason: "the projected changed-path set could not be computed",
    };
  }

  const paths = [...changed].map(normalisePath).filter(Boolean);
  if (paths.length === 0) {
    return {
      ok: true,
      selected: true,
      reason:
        "the projected changed-path set is empty, which is not evidence of a documentation-only change",
    };
  }

  if (paths.includes("README.md")) {
    return {
      ok: true,
      selected: true,
      reason: "the root README quickstart changed",
    };
  }

  if (
    !policy?.quickstartGlobs?.length ||
    !policy?.knownNonimpactGlobs?.length
  ) {
    return {
      ok: false,
      selected: null,
      reason: "quickstart impact policy is missing or empty",
    };
  }

  const impacted = paths.filter(
    (path) =>
      matchesAnyGlob(path, policy.quickstartGlobs) ||
      !matchesAnyGlob(path, policy.knownNonimpactGlobs),
  );
  if (impacted.length > 0) {
    return {
      ok: true,
      selected: true,
      reason: `non-documentation or quickstart-contract paths changed (${impacted.join(", ")})`,
    };
  }

  return {
    ok: true,
    selected: false,
    reason:
      "every changed path is trusted documentation and no quickstart contract file changed",
  };
}

export function validateSelectionBinding({ checkoutSha, headSha }) {
  const checkout = String(checkoutSha ?? "").trim();
  const head = String(headSha ?? "").trim();
  if (!fullSHA.test(checkout)) {
    return {
      ok: false,
      message: `CHECKOUT_SHA must be one full lowercase commit SHA (got ${JSON.stringify(checkout)})`,
    };
  }
  if (head && !fullSHA.test(head)) {
    return {
      ok: false,
      message: `HEAD_SHA must be one full lowercase commit SHA (got ${JSON.stringify(head)})`,
    };
  }
  if (head && head !== checkout) {
    return {
      ok: false,
      message:
        `HEAD_SHA (${head}) differs from CHECKOUT_SHA (${checkout}); selection must inspect the same projected ` +
        "commit the canary will boot",
    };
  }
  return {
    ok: true,
    message: "selection and checkout are bound to the same projected commit",
  };
}

export function classifyCheckout({
  expectedSha,
  actualSha,
  status,
  root,
  cwd,
  requiredFilesPresent,
}) {
  const expected = String(expectedSha ?? "").trim();
  const actual = String(actualSha ?? "").trim();
  if (!fullSHA.test(expected)) {
    return {
      ok: false,
      message: `EXPECTED_SHA must be one full lowercase commit SHA (got ${JSON.stringify(expected)})`,
    };
  }
  if (actual !== expected) {
    return {
      ok: false,
      message: `checkout HEAD is ${actual || "<missing>"}, expected exactly ${expected}`,
    };
  }
  if (resolve(String(root ?? "")) !== resolve(String(cwd ?? ""))) {
    return {
      ok: false,
      message: `quickstart must run at repository root ${root}, but the current directory is ${cwd}`,
    };
  }
  if (String(status ?? "") !== "") {
    return {
      ok: false,
      message: `projected checkout is not clean:\n${status}`,
    };
  }
  if (!requiredFilesPresent) {
    return {
      ok: false,
      message:
        "root README.md or docker-compose.yml is missing from the projected checkout",
    };
  }
  return {
    ok: true,
    message: `clean projected checkout ${actual} verified at repository root`,
  };
}

export function quickstartUpInvocation(
  repoRoot,
  timeoutMs = DEFAULT_UP_TIMEOUT_MS,
) {
  return Object.freeze({
    command: process.execPath,
    args: Object.freeze([
      join(repoRoot, REGISTRY_RETRY_WRAPPER),
      ...QUICKSTART_UP_COMMAND,
    ]),
    cwd: repoRoot,
    timeout: timeoutMs,
  });
}

export function parsePositiveMilliseconds(value, fallback, name) {
  const raw = String(value ?? "").trim();
  if (!raw) return fallback;
  if (!/^\d+$/.test(raw))
    throw new TypeError(
      `${name} must be a positive integer number of milliseconds`,
    );
  const parsed = Number(raw);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) {
    throw new TypeError(
      `${name} must be a positive safe integer number of milliseconds`,
    );
  }
  return parsed;
}

function parseJSON(body, label) {
  try {
    return { value: JSON.parse(body), problem: null };
  } catch (err) {
    return {
      value: null,
      problem: `${label} returned invalid JSON: ${err.message}`,
    };
  }
}

function expectHTTP200(response, label) {
  if (!response || response.status !== 200) {
    return `${label} returned HTTP ${response?.status ?? "<no response>"}, want 200`;
  }
  return null;
}

export function validateHealthz(response) {
  const statusProblem = expectHTTP200(response, "/healthz");
  if (statusProblem) return statusProblem;
  if (response.body !== "ok")
    return `/healthz body is ${JSON.stringify(response.body)}, want exactly "ok"`;
  return null;
}

export function validateReadyz(response) {
  const statusProblem = expectHTTP200(response, "/readyz");
  if (statusProblem) return statusProblem;
  const { value, problem } = parseJSON(response.body, "/readyz");
  if (problem) return problem;
  if (!value || typeof value !== "object" || Array.isArray(value))
    return "/readyz body must be a JSON object";
  if (value.clickhouse !== "ok")
    return `/readyz clickhouse is ${JSON.stringify(value.clickhouse)}, want "ok"`;
  if (value.schema !== "ready")
    return `/readyz schema is ${JSON.stringify(value.schema)}, want "ready"`;
  if (
    !value.heads ||
    typeof value.heads !== "object" ||
    Array.isArray(value.heads)
  ) {
    return "/readyz heads must be a JSON object containing prom, loki, and tempo";
  }
  const actualHeads = Object.keys(value.heads).sort();
  const wantedHeads = [...CANONICAL_HEADS].sort();
  if (JSON.stringify(actualHeads) !== JSON.stringify(wantedHeads)) {
    return `/readyz heads are ${JSON.stringify(actualHeads)}, want exactly ${JSON.stringify(wantedHeads)}`;
  }
  for (const head of CANONICAL_HEADS) {
    if (value.heads[head] !== "closed") {
      return `/readyz head ${head} is ${JSON.stringify(value.heads[head])}, want healthy breaker phase "closed"`;
    }
  }
  return null;
}

export function validateGrafanaRoot(response) {
  const statusProblem = expectHTTP200(response, "Grafana root");
  if (statusProblem) return statusProblem;
  const contentType = String(
    response.headers?.["content-type"] ?? "",
  ).toLowerCase();
  if (!contentType.includes("text/html"))
    return `Grafana root content-type is ${JSON.stringify(contentType)}, want text/html`;
  if (
    !/<html(?:\s|>)/i.test(response.body) ||
    !/<title>\s*grafana\s*<\/title>/i.test(response.body)
  ) {
    return "Grafana root did not return usable Grafana HTML";
  }
  return null;
}

export function validateGrafanaHealth(response) {
  const statusProblem = expectHTTP200(response, "Grafana /api/health");
  if (statusProblem) return statusProblem;
  const { value, problem } = parseJSON(response.body, "Grafana /api/health");
  if (problem) return problem;
  if (!value || typeof value !== "object" || value.database !== "ok") {
    return `Grafana database health is ${JSON.stringify(value?.database)}, want "ok"`;
  }
  return null;
}

export function validateDatasource(response, expected) {
  const label = `Grafana datasource ${expected.uid}`;
  const statusProblem = expectHTTP200(response, label);
  if (statusProblem) return statusProblem;
  const { value, problem } = parseJSON(response.body, label);
  if (problem) return problem;
  for (const field of ["uid", "name", "type", "url"]) {
    if (value?.[field] !== expected[field]) {
      return `${label} ${field} is ${JSON.stringify(value?.[field])}, want ${JSON.stringify(expected[field])}`;
    }
  }
  return null;
}

export function validatePrometheusProxy(response) {
  const label = "Grafana-proxied Prometheus query";
  const statusProblem = expectHTTP200(response, label);
  if (statusProblem) return statusProblem;
  const { value, problem } = parseJSON(response.body, label);
  if (problem) return problem;
  if (value?.status !== "success")
    return `${label} status is ${JSON.stringify(value?.status)}, want "success"`;
  if (value?.data?.resultType !== "vector") {
    return `${label} resultType is ${JSON.stringify(value?.data?.resultType)}, want "vector"`;
  }
  if (!Array.isArray(value?.data?.result) || value.data.result.length === 0) {
    return `${label} returned no seeded metric series`;
  }
  const hasSeededSample = value.data.result.some(
    (sample) =>
      sample?.metric?.__name__ === QUICKSTART_SEED.prometheus.metric &&
      sample?.metric?.job === QUICKSTART_SEED.prometheus.job &&
      Array.isArray(sample?.value) &&
      sample.value.length === 2 &&
      Number(sample.value[1]) === QUICKSTART_SEED.prometheus.value,
  );
  if (!hasSeededSample) {
    return `${label} returned no exact seeded ${QUICKSTART_SEED.prometheus.metric}{job="${QUICKSTART_SEED.prometheus.job}"} sample`;
  }
  return null;
}

export function validateLokiProxy(response) {
  const label = "Grafana-proxied Loki query";
  const statusProblem = expectHTTP200(response, label);
  if (statusProblem) return statusProblem;
  const { value, problem } = parseJSON(response.body, label);
  if (problem) return problem;
  if (value?.status !== "success")
    return `${label} status is ${JSON.stringify(value?.status)}, want "success"`;
  if (value?.data?.resultType !== "streams") {
    return `${label} resultType is ${JSON.stringify(value?.data?.resultType)}, want "streams"`;
  }
  if (!Array.isArray(value?.data?.result) || value.data.result.length === 0) {
    return `${label} returned no seeded log streams`;
  }
  const hasLogLine = value.data.result.some(
    (stream) =>
      stream?.stream?.service_name === QUICKSTART_SEED.loki.serviceName &&
      Array.isArray(stream?.values) &&
      stream.values.some(
        (entry) =>
          Array.isArray(entry) &&
          entry.length === 2 &&
          entry.every((item) => typeof item === "string"),
      ),
  );
  if (!hasLogLine) return `${label} returned no usable log line`;
  return null;
}

export function validateTempoProxy(response) {
  const label = "Grafana-proxied Tempo search";
  const statusProblem = expectHTTP200(response, label);
  if (statusProblem) return statusProblem;
  const { value, problem } = parseJSON(response.body, label);
  if (problem) return problem;
  if (!Array.isArray(value?.traces) || value.traces.length === 0) {
    return `${label} returned no seeded trace summaries`;
  }
  const seededTrace = value.traces.find(
    (trace) => trace?.traceID === QUICKSTART_SEED.tempo.traceID,
  );
  if (!seededTrace) return `${label} did not return the exact seeded trace`;
  if (
    seededTrace.rootServiceName !== QUICKSTART_SEED.tempo.rootServiceName ||
    seededTrace.rootTraceName !== QUICKSTART_SEED.tempo.rootTraceName
  ) {
    return `${label} returned the seeded trace with the wrong root identity`;
  }
  return null;
}

function orderedDashboardTargets(dashboard) {
  const targets = [];
  for (const panel of dashboard?.panels ?? []) {
    for (const target of panel?.targets ?? []) {
      const datasourceUID =
        target?.datasource?.uid ?? panel?.datasource?.uid ?? "";
      const expression = target?.expr ?? target?.query ?? "";
      const kind =
        target?.datasource?.type ?? panel?.datasource?.type ?? "";
      targets.push({
        panelID: panel?.id,
        refID: target?.refId,
        datasourceUID,
        kind,
        expression,
      });
    }
  }
  return targets.sort(
    (left, right) =>
      Number(left.panelID) - Number(right.panelID) ||
      String(left.refID).localeCompare(String(right.refID)),
  );
}

export function validateHomeDashboard(response) {
  const statusProblem = expectHTTP200(response, "Grafana home dashboard");
  if (statusProblem) return statusProblem;
  const { value, problem } = parseJSON(response.body, "Grafana home dashboard");
  if (problem) return problem;
  if (value?.dashboard?.uid !== HOME_DASHBOARD.uid) {
    return `Grafana home dashboard uid is ${JSON.stringify(value?.dashboard?.uid)}, want ${JSON.stringify(HOME_DASHBOARD.uid)}`;
  }
  if (value?.dashboard?.title !== HOME_DASHBOARD.title) {
    return `Grafana home dashboard title is ${JSON.stringify(value?.dashboard?.title)}, want ${JSON.stringify(HOME_DASHBOARD.title)}`;
  }
  if (
    !Array.isArray(value.dashboard.panels) ||
    value.dashboard.panels.length === 0
  ) {
    return "Grafana home dashboard has no panels";
  }
  const datasourceUIDs = new Set();
  for (const panel of value.dashboard.panels) {
    if (panel?.datasource?.uid) datasourceUIDs.add(panel.datasource.uid);
    for (const target of panel?.targets ?? []) {
      if (target?.datasource?.uid) datasourceUIDs.add(target.datasource.uid);
    }
  }
  for (const uid of HOME_DASHBOARD_DATASOURCES) {
    if (!datasourceUIDs.has(uid))
      return `Grafana home dashboard has no panel target for ${uid}`;
  }
  const actualTargets = orderedDashboardTargets(value.dashboard);
  if (
    JSON.stringify(actualTargets) !== JSON.stringify(HOME_DASHBOARD_TARGETS)
  ) {
    return (
      "Grafana home dashboard target roster differs from the expressions replayed by the required canary: " +
      `got ${JSON.stringify(actualTargets)}, want ${JSON.stringify(HOME_DASHBOARD_TARGETS)}`
    );
  }
  return null;
}

export function validateHomeDashboardPrometheusTarget(response) {
  const label = "Grafana home-dashboard Prometheus target";
  const statusProblem = expectHTTP200(response, label);
  if (statusProblem) return statusProblem;
  const { value, problem } = parseJSON(response.body, label);
  if (problem) return problem;
  if (value?.status !== "success")
    return `${label} status is ${JSON.stringify(value?.status)}, want "success"`;
  if (value?.data?.resultType !== "vector") {
    return `${label} resultType is ${JSON.stringify(value?.data?.resultType)}, want "vector"`;
  }
  if (!Array.isArray(value?.data?.result))
    return `${label} result must be an array`;
  return null;
}

export function validateHomeDashboardTempoTarget(response) {
  const label = "Grafana home-dashboard Tempo target";
  const statusProblem = expectHTTP200(response, label);
  if (statusProblem) return statusProblem;
  const { value, problem } = parseJSON(response.body, label);
  if (problem) return problem;
  if (!Array.isArray(value?.traces))
    return `${label} traces must be an array`;
  return null;
}

function homeDashboardTargetProbe(grafana, target) {
  const encoded = encodeURIComponent(target.expression);
  if (target.kind === "prometheus") {
    return Object.freeze({
      id: `home-panel-${target.panelID}-${target.refID}`,
      url:
        `${grafana}/api/datasources/proxy/uid/${encodeURIComponent(target.datasourceUID)}` +
        `/api/v1/query?query=${encoded}`,
      validate: validateHomeDashboardPrometheusTarget,
    });
  }
  if (target.kind === "tempo") {
    return Object.freeze({
      id: `home-panel-${target.panelID}-${target.refID}`,
      url:
        `${grafana}/api/datasources/proxy/uid/${encodeURIComponent(target.datasourceUID)}` +
        `/api/search?q=${encoded}&limit=${QUICKSTART_TEMPO_SEARCH_LIMIT}`,
      validate: validateHomeDashboardTempoTarget,
    });
  }
  throw new Error(
    `home dashboard target ${target.panelID}/${target.refID} has unsupported kind ${JSON.stringify(target.kind)}`,
  );
}

export function probePlan({ cerberusURL, grafanaURL }) {
  const gateway = String(cerberusURL).replace(/\/+$/, "");
  const grafana = String(grafanaURL).replace(/\/+$/, "");
  return Object.freeze([
    Object.freeze({
      id: "healthz",
      url: `${gateway}/healthz`,
      validate: validateHealthz,
    }),
    Object.freeze({
      id: "readyz",
      url: `${gateway}/readyz`,
      validate: validateReadyz,
    }),
    Object.freeze({
      id: "grafana-root",
      url: `${grafana}/`,
      validate: validateGrafanaRoot,
    }),
    Object.freeze({
      id: "grafana-health",
      url: `${grafana}/api/health`,
      validate: validateGrafanaHealth,
    }),
    ...CANONICAL_DATASOURCES.map((expected) =>
      Object.freeze({
        id: `datasource-${expected.uid}`,
        url: `${grafana}/api/datasources/uid/${encodeURIComponent(expected.uid)}`,
        validate: (response) => validateDatasource(response, expected),
      }),
    ),
    Object.freeze({
      id: "proxy-cerberus-prometheus",
      url: `${grafana}/api/datasources/proxy/uid/cerberus-prometheus/api/v1/query?query=${encodeURIComponent(`${QUICKSTART_SEED.prometheus.metric}{job="${QUICKSTART_SEED.prometheus.job}"}`)}`,
      validate: validatePrometheusProxy,
    }),
    Object.freeze({
      id: "proxy-cerberus-loki",
      url: `${grafana}/api/datasources/proxy/uid/cerberus-loki/loki/api/v1/query?query=${encodeURIComponent(`{service_name="${QUICKSTART_SEED.loki.serviceName}"}`)}`,
      validate: validateLokiProxy,
    }),
    Object.freeze({
      id: "proxy-cerberus-tempo",
      url: `${grafana}/api/datasources/proxy/uid/cerberus-tempo/api/search?q=${encodeURIComponent(`{ resource.service.name = "${QUICKSTART_SEED.tempo.rootServiceName}" }`)}&limit=${QUICKSTART_TEMPO_SEARCH_LIMIT}`,
      validate: validateTempoProxy,
    }),
    Object.freeze({
      id: "home-dashboard",
      url: `${grafana}/api/dashboards/home`,
      validate: validateHomeDashboard,
    }),
    ...HOME_DASHBOARD_TARGETS.map((target) =>
      homeDashboardTargetProbe(grafana, target),
    ),
  ]);
}

export function validateProbeSnapshot(plan, responses) {
  const problems = [];
  for (const probe of plan) {
    const response = responses.get(probe.id);
    if (response?.error) {
      problems.push(`${probe.id}: request failed: ${response.error}`);
      continue;
    }
    const problem = probe.validate(response);
    if (problem) problems.push(`${probe.id}: ${problem}`);
  }
  return problems;
}

export function classifyAggregate({ selectResult, selected, runResult }) {
  if (selectResult !== "success") {
    return {
      ok: false,
      message: `selector result is ${JSON.stringify(selectResult)}; selection did not succeed, so silence cannot pass`,
    };
  }
  if (selected !== "true" && selected !== "false") {
    return {
      ok: false,
      message: `selector output selected=${JSON.stringify(selected)}; want the literal "true" or "false"`,
    };
  }
  if (selected === "true") {
    if (runResult !== "success") {
      return {
        ok: false,
        message: `quickstart was selected but its run result is ${JSON.stringify(runResult)}, not "success"`,
      };
    }
    return {
      ok: true,
      message: "the published quickstart passed on the projected checkout",
    };
  }
  if (runResult !== "skipped") {
    return {
      ok: false,
      message: `quickstart was omitted as not impacted, but run result is ${JSON.stringify(runResult)}, not "skipped"`,
    };
  }
  return {
    ok: true,
    message:
      "not quickstart-impacting; selector succeeded and the quickstart run was explicitly skipped",
  };
}

function repositoryRoot(cwd = process.cwd()) {
  const result = capture("git", ["rev-parse", "--show-toplevel"], { cwd });
  if (result.status !== 0 || !result.stdout.trim()) {
    throw new Error(
      `cannot resolve repository root: ${result.stderr.trim() || "git returned no path"}`,
    );
  }
  return resolve(result.stdout.trim());
}

function requireCleanCheckout(expectedSha) {
  const cwd = resolve(process.cwd());
  const root = repositoryRoot(cwd);
  const actualResult = capture("git", ["rev-parse", "--verify", "HEAD"], {
    cwd: root,
  });
  if (actualResult.status !== 0)
    throw new Error(`git rev-parse HEAD failed: ${actualResult.stderr.trim()}`);
  const statusResult = capture(
    "git",
    ["status", "--porcelain=v1", "--untracked-files=all"],
    { cwd: root },
  );
  if (statusResult.status !== 0)
    throw new Error(`git status failed: ${statusResult.stderr.trim()}`);

  const verdict = classifyCheckout({
    expectedSha,
    actualSha: actualResult.stdout.trim(),
    status: statusResult.stdout,
    root,
    cwd,
    requiredFilesPresent:
      existsSync(join(root, "README.md")) &&
      existsSync(join(root, "docker-compose.yml")),
  });
  if (!verdict.ok) throw new Error(verdict.message);

  const published = publishedQuickstart(
    readFileSync(join(root, "README.md"), "utf8"),
  );
  if (!published.ok) throw new Error(published.message);
  return verdict;
}

function selectMode() {
  const checkoutSha = String(process.env.CHECKOUT_SHA ?? "").trim();
  const headSha = String(process.env.HEAD_SHA ?? "").trim() || checkoutSha;
  const binding = validateSelectionBinding({ checkoutSha, headSha });
  if (!binding.ok) throw new Error(binding.message);

  // Selection is evidence about the checked-out projected commit, not merely
  // about two environment strings that claim to name it. Prove HEAD and the
  // working tree before computing the diff, so a stale or mutated checkout
  // makes the selector fail and the stable aggregate stays red.
  requireCleanCheckout(checkoutSha);

  const eventName = String(process.env.EVENT_NAME ?? "").trim();
  const changed = DIFF_SCOPED_EVENTS.includes(eventName)
    ? changedPaths({ baseSha: process.env.BASE_SHA, headSha })
    : null;
  const verdict = selectQuickstart({
    eventName,
    changed,
    policy: quickstartSelectionPolicy(repositoryRoot()),
  });
  if (!verdict.ok) throw new Error(verdict.reason);

  setOutput("selected", String(verdict.selected));
  setOutput("checkout_sha", checkoutSha);
  setOutput("reason", verdict.reason);
  notice(
    `quickstart selection: ${verdict.selected ? "run" : "omit"} — ${verdict.reason}`,
  );
}

function verifyCheckoutMode() {
  const verdict = requireCleanCheckout(process.env.EXPECTED_SHA);
  notice(`quickstart checkout: ${verdict.message}`);
}

function upMode() {
  const root = repositoryRoot();
  const timeout = parsePositiveMilliseconds(
    process.env.QUICKSTART_UP_TIMEOUT_MS,
    DEFAULT_UP_TIMEOUT_MS,
    "QUICKSTART_UP_TIMEOUT_MS",
  );
  const invocation = quickstartUpInvocation(root, timeout);
  log(`==> ${QUICKSTART_UP_COMMAND.join(" ")}`);
  const result = spawnSync(invocation.command, invocation.args, {
    cwd: invocation.cwd,
    env: process.env,
    stdio: "inherit",
    timeout: invocation.timeout,
    killSignal: "SIGTERM",
  });
  if (result.error) {
    throw new Error(
      `quickstart startup did not complete within its ${timeout}ms bound: ${result.error.message}`,
    );
  }
  if (result.status !== 0) {
    throw new Error(
      `quickstart startup exited with status ${result.status ?? "<signal>"}`,
    );
  }
  notice(
    "quickstart startup: root docker compose stack reached its declared healthy state",
  );
}

async function fetchProbe(url, timeoutMs) {
  try {
    const response = await fetch(url, {
      method: "GET",
      headers: { accept: "application/json, text/html;q=0.9" },
      redirect: "follow",
      signal: AbortSignal.timeout(timeoutMs),
    });
    return {
      status: response.status,
      headers: { "content-type": response.headers.get("content-type") ?? "" },
      body: await response.text(),
    };
  } catch (err) {
    return { error: err.message };
  }
}

function delay(milliseconds) {
  return new Promise((resolveDelay) => setTimeout(resolveDelay, milliseconds));
}

export async function probeUntilReady({
  plan,
  timeoutMs = DEFAULT_PROBE_TIMEOUT_MS,
  requestTimeoutMs = DEFAULT_REQUEST_TIMEOUT_MS,
  retryDelayMs = DEFAULT_RETRY_DELAY_MS,
  fetcher = fetchProbe,
  now = Date.now,
  sleep = delay,
}) {
  const deadline = now() + timeoutMs;
  let attempt = 0;
  let lastProblems = ["no probe attempt completed"];

  while (now() < deadline) {
    attempt += 1;
    const remaining = deadline - now();
    const perRequest = Math.max(1, Math.min(requestTimeoutMs, remaining));
    const snapshots = await Promise.all(
      plan.map(async (probe) => [
        probe.id,
        await fetcher(probe.url, perRequest),
      ]),
    );
    lastProblems = validateProbeSnapshot(plan, new Map(snapshots));
    if (lastProblems.length === 0)
      return { ok: true, attempts: attempt, problems: [] };

    const afterAttempt = now();
    if (afterAttempt >= deadline) break;
    const wait = Math.min(retryDelayMs, deadline - afterAttempt);
    if (wait > 0) await sleep(wait);
  }

  return { ok: false, attempts: attempt, problems: lastProblems };
}

async function probeMode() {
  const timeout = parsePositiveMilliseconds(
    process.env.QUICKSTART_PROBE_TIMEOUT_MS,
    DEFAULT_PROBE_TIMEOUT_MS,
    "QUICKSTART_PROBE_TIMEOUT_MS",
  );
  const plan = probePlan({
    cerberusURL: process.env.CERBERUS_URL || "http://localhost:8080",
    grafanaURL: process.env.GRAFANA_URL || "http://localhost:3000",
  });
  const verdict = await probeUntilReady({ plan, timeoutMs: timeout });
  if (!verdict.ok) {
    throw new Error(
      `quickstart HTTP contract did not become healthy within ${timeout}ms after ${verdict.attempts} attempt(s):\n` +
        verdict.problems.map((problem) => `- ${problem}`).join("\n"),
    );
  }
  notice(
    `quickstart probes: all ${plan.length} user-visible checks passed after ${verdict.attempts} attempt(s)`,
  );
}

function aggregateMode() {
  const verdict = classifyAggregate({
    selectResult: String(process.env.SELECT_RESULT ?? ""),
    selected: String(process.env.SELECTED ?? ""),
    runResult: String(process.env.RUN_RESULT ?? ""),
  });
  if (!verdict.ok) throw new Error(verdict.message);
  notice(`quickstart aggregate: ${verdict.message}`);
}

export async function main(
  mode = (process.env.MODE || process.argv[2] || "").trim(),
) {
  if (!MODES.includes(mode)) {
    throw new Error(
      `MODE must be one of ${MODES.join(", ")} (got ${JSON.stringify(mode)})`,
    );
  }
  if (mode === "select") return selectMode();
  if (mode === "verify-checkout") return verifyCheckoutMode();
  if (mode === "up") return upMode();
  if (mode === "probe") return probeMode();
  return aggregateMode();
}

const invokedDirectly =
  process.argv[1] &&
  import.meta.url === pathToFileURL(resolve(process.argv[1])).href;
if (invokedDirectly) {
  main().catch((err) => {
    error(`quickstart-canary: ${err.message}`, {
      title: "quickstart canary failed closed",
    });
    process.exitCode = 1;
  });
}
