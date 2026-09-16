#!/usr/bin/env node
// compat-execution-report.mjs — fans ONE compatibility CI run's already-
// uploaded case-set artifact(s) out over every ACTIVE "reference"-evidence
// binding the semantic model declares under compatibility/<HEAD> (cerberus
// issue #3499).
//
// lib/semantic-execution-adapter.mjs's own CLI (MODE=compat) asks for
// exactly one BINDING per invocation BY DESIGN — a compat binding is scoped
// to the WHOLE driver invocation, and the CLI's own header names the tempo
// HTTP/gRPC transports as the reason two distinct case sets exist for one
// job. That is the right granularity for tempo (BINDING-TRACEQL-TRANSPORT-
// ARM-HTTP / -GRPC, one binding per transport), but compatibility/prometheus
// and compatibility/loki each bind SEVERAL active "reference" contracts to
// the SAME one case set their job produces — the corpus-wide differential
// is one run, evidencing several distinct behaviors at once. This script is
// the multi-binding orchestration the CLI's own comments point a caller at
// for that case: it calls no adapter logic of its own — every record comes
// from the SAME classifyRevisionBinding/toExecutionRecord/parseCaseSet/
// sharedContext/execIdFor functions the CLI uses, just looped over the
// model's own binding roster instead of one caller-named BINDING id.
//
// Never writes test/semantic/executions.json — see the CLI's own header;
// prints/writes normalized records for a human (or a later promotion step)
// to review, exactly like the CLI. A "manual-review" evidence_class binding
// (e.g. a third-party corpus provenance review) is never selected here —
// only "reference"-evidence bindings, the class an automated differential
// run can actually speak to.
//
// Transport arms (tempo only): a binding whose test_ref names a "grpc"
// driver file (case-insensitive substring match, e.g. grpc_diff.go) is
// classified against CASES_PATH_GRPC when given; every other active
// binding under the head is classified against CASES_PATH. Heads with only
// one arm (prometheus, loki) simply never set CASES_PATH_GRPC, so every
// selected binding resolves to the one CASES_PATH.
//
// Env:
//   HEAD               required — selects the active "reference" bindings
//                      whose test_ref starts with `compatibility/<HEAD>`
//   CASES_PATH         required — a compatibility/internal/score.CaseSet
//                      JSON document (the non-gRPC / only arm)
//   CASES_PATH_GRPC    optional — a second CaseSet document for a
//                      gRPC-named binding (tempo only)
//   CORPUS_PATH        optional — one file/directory, or several ":"-joined
//                      file/directory paths (loki's harness draws queries
//                      from TWO separate roots — upstream/loki-bench/queries
//                      AND cerberus-queries — and a fingerprint over only
//                      one of them is blind to drift in the other), folded
//                      into one dataset_fingerprint shared by every record
//                      this run emits
//   REFERENCE_VERSION           optional — stamped on every record's
//                      reference_version
//   EXPECT_REFERENCE_VERSION    optional — asserted; a mismatch against
//                      REFERENCE_VERSION flags non-evidence
//   CANDIDATE_SHA / RUN_REF / OBSERVED_AT / MODEL_DIR / OUT — see
//                      semantic-execution-adapter.mjs's own header; same
//                      defaults, resolved through the same sharedContext().
//   SOFT_FAIL          when "1", an error that would otherwise exit 1 (a
//                      missing HEAD/CASES_PATH, no matching active
//                      binding, an unreadable case set, …) is instead
//                      annotated with ::warning:: and this process exits
//                      0 — same mechanism and rationale as
//                      semantic-execution-adapter.mjs's own SOFT_FAIL (see
//                      its header): a protected/release-required CI lane
//                      cannot use `continue-on-error: true`
//                      (test/regression/ci_lane_registry_test.go bans it
//                      with no exceptions), so the script itself has to be
//                      the thing that never fails the job. compatibility.yml
//                      sets this; a developer running the script by hand
//                      leaves it unset and keeps the immediate, hard-fail
//                      feedback.

import process from "node:process";
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { error, warning } from "./lib/gh.mjs";
import { DEFAULT_SEMANTIC_MODEL_DIR, loadSemanticModel } from "./lib/semantic-model.mjs";
import {
  SemanticExecutionAdapterError,
  classifyRevisionBinding,
  execIdFor,
  hashCorpus,
  parseCaseSet,
  sharedContext,
  toExecutionRecord,
} from "./lib/semantic-execution-adapter.mjs";

/** True for a binding whose test_ref names a gRPC driver (tempo's second transport arm). */
export function isGrpcBinding(testRef) {
  return testRef.toLowerCase().includes("grpc");
}

/**
 * Parses CORPUS_PATH into what hashCorpus() expects: a single string for
 * one root (byte-identical fingerprint to before this multi-root form
 * existed), or an array for several ":"-joined roots (loki's two corpus
 * directories). Empty segments (a stray leading/trailing/doubled ":") are
 * dropped rather than handed to hashCorpus as an empty path.
 */
export function parseCorpusPath(raw) {
  const parts = raw.split(":").filter((p) => p.length > 0);
  return parts.length <= 1 ? (parts[0] ?? raw) : parts;
}

/**
 * The active "reference"-evidence bindings a HEAD's compat job can speak
 * to: status "active", evidence_class "reference", test_ref prefixed
 * `compatibility/<head>`. A "manual-review" binding under the same prefix
 * (a corpus-provenance sign-off) is deliberately excluded — no automated
 * differential run is evidence for it.
 */
export function selectBindings(model, head) {
  const prefix = `compatibility/${head}`;
  return [...model.bindings.values()].filter(
    (b) => b.status === "active" && b.evidence_class === "reference" && b.test_ref.startsWith(prefix),
  );
}

function main() {
  const env = process.env;
  const head = env.HEAD;
  if (!head) throw new SemanticExecutionAdapterError(["HEAD is required"]);
  const casesPath = env.CASES_PATH;
  if (!casesPath) throw new SemanticExecutionAdapterError(["CASES_PATH is required"]);
  const casesPathGrpc = env.CASES_PATH_GRPC || null;

  const root = process.cwd();
  const { candidateSha, runRef, observedAt, run } = sharedContext(env);
  const model = loadSemanticModel(env.MODEL_DIR || DEFAULT_SEMANTIC_MODEL_DIR, { root });

  const bindings = selectBindings(model, head);
  if (bindings.length === 0) {
    throw new SemanticExecutionAdapterError([
      `no active "reference" binding in the semantic model has a test_ref starting with ${JSON.stringify(`compatibility/${head}`)}`,
    ]);
  }

  const caseSetCache = new Map();
  const loadCaseSet = (path) => {
    if (!caseSetCache.has(path)) {
      caseSetCache.set(path, parseCaseSet(JSON.parse(readFileSync(path, "utf8"))));
    }
    return caseSetCache.get(path);
  };

  const datasetFingerprint = env.CORPUS_PATH ? hashCorpus(parseCorpusPath(env.CORPUS_PATH)) : null;
  const referenceVersion = env.REFERENCE_VERSION || null;

  const candidate = { sourceSha: candidateSha };
  if (env.EXPECT_REFERENCE_VERSION) candidate.referenceVersion = env.EXPECT_REFERENCE_VERSION;

  const records = bindings.map((binding) => {
    const path = casesPathGrpc && isGrpcBinding(binding.test_ref) ? casesPathGrpc : casesPath;
    const caseSet = loadCaseSet(path);
    const classification = classifyRevisionBinding(candidate, {
      sourceSha: candidateSha,
      referenceVersion,
      datasetFingerprint,
      selectedCount: caseSet.selectedCount,
      ranCount: caseSet.ranCount,
      aggregateNoOp: false,
      result: caseSet.result,
    });
    return toExecutionRecord({
      id: execIdFor(binding.id, observedAt),
      binding: binding.id,
      observedAt,
      runRef,
      classification,
      context: {
        sourceSha: candidateSha,
        runId: run.runId,
        runAttempt: run.runAttempt,
        job: run.job,
        event: run.event,
        substrate: "reference-stack",
        referenceVersion,
        datasetFingerprint,
      },
    });
  });

  const json = `${JSON.stringify(records, null, 2)}\n`;
  if (env.OUT) {
    writeFileSync(env.OUT, json);
    process.stdout.write(`compat-execution-report: wrote ${records.length} record(s) for ${head} to ${env.OUT}\n`);
  } else {
    process.stdout.write(json);
  }

  const nonEvidence = records.filter((r) => r.selection !== "executed");
  if (nonEvidence.length > 0) {
    process.stderr.write(
      `compat-execution-report: ${nonEvidence.length}/${records.length} record(s) are NON-EVIDENCE (see selection_reason)\n`,
    );
  }
}

const invokedDirectly =
  process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    if (process.env.SOFT_FAIL === "1") {
      warning(`compat-execution-report: ${message}`, { title: "Compat execution report (soft-fail)" });
      process.exit(0);
    }
    error(message, { title: "compat execution report" });
    process.exit(1);
  }
}
