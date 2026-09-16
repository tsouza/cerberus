#!/usr/bin/env node
// semantic-execution-adapter.mjs — CLI over lib/semantic-execution-adapter.mjs
// (cerberus issue #3459). Prints ONE revision-bound execution observation,
// normalized to test/semantic/executions.json's record shape, as JSON on
// stdout (or to OUT). Never writes test/semantic/executions.json itself —
// that file stays hand-authored/reviewed like every other file under
// test/semantic/ (see .github/scripts/README.md's "Semantic contract
// model" section); a human or a future CI step (issue #3462, which this
// issue blocks) decides whether/where to append the printed record.
//
// Three modes, selected by MODE:
//
//   MODE=property   Reads a `go test -json` stream (GOTEST_JSON_PATH) and
//                    emits one record PER active property-shape binding in
//                    the semantic model whose ShapeID appears in the
//                    stream — the join is exact (a property-shape
//                    binding's own test_ref IS the ShapeID, see
//                    lib/semantic-evidence-adapter.mjs's classifyTestRef),
//                    so no BINDING env var is needed; a binding whose
//                    shape never appears reports "selected_not_run" rather
//                    than being silently skipped.
//   MODE=compat     Reads a compat harness's compat-cases.json
//                    (CASES_PATH, compatibility/internal/score.CaseSet's
//                    JSON shape) and emits ONE record for the explicitly
//                    named BINDING — a compat binding is scoped to the
//                    WHOLE driver invocation, not one case, so there is no
//                    generic case-to-binding join to derive (the HTTP vs
//                    gRPC Tempo transports are two files/two bindings for
//                    exactly this reason — pass BINDING-TRACEQL-
//                    TRANSPORT-ARM-HTTP against compat-cases.json and
//                    BINDING-TRACEQL-TRANSPORT-ARM-GRPC against
//                    compat-cases-grpc.json separately, keeping the two
//                    transports independently attributable under the
//                    one compat/tempo job that produces both).
//   MODE=verify     Re-classifies an EXISTING executions.json record
//                    (EXECUTION_ID) against the CURRENT candidate facts —
//                    demonstrates and lets a caller actually exercise
//                    staleness/wrong-SHA detection against real stored
//                    data, independent of either producer mode above.
//
// Env (documented per mode above; shared across all three):
//   CANDIDATE_SHA        the commit this classification is FOR (default
//                         GITHUB_SHA)
//   RUN_REF               default constructed from GITHUB_SERVER_URL /
//                         GITHUB_REPOSITORY / GITHUB_RUN_ID when unset
//   OBSERVED_AT           default: now, ISO-8601 UTC
//   MODEL_DIR             default test/semantic (DEFAULT_SEMANTIC_MODEL_DIR)
//   OUT                   optional output path; default stdout
//   CORPUS_PATH            (compat, verify) file or directory to hash for
//                         dataset_fingerprint — omit to leave it null
//                         (never asserted/checked)
//   REFERENCE_VERSION      (compat) the reference backend version/build
//                         this observation actually ran against
//   EXPECT_REFERENCE_VERSION (compat, verify) asserts a candidate
//                         reference-version expectation; a mismatch
//                         against REFERENCE_VERSION flags non-evidence

import process from "node:process";
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { DEFAULT_SEMANTIC_MODEL_DIR, loadSemanticModel } from "./lib/semantic-model.mjs";
import { classifyTestRef } from "./lib/semantic-evidence-adapter.mjs";
import {
  SemanticExecutionAdapterError,
  classifyRevisionBinding,
  execIdFor,
  hashCorpus,
  parseCaseSet,
  parseGoTestJSONShapeResults,
  propertyShapeObservation,
  sharedContext,
  toExecutionRecord,
} from "./lib/semantic-execution-adapter.mjs";

function errorAnnotation(message) {
  const oneLine = message.replaceAll("%", "%25").replaceAll("\r", "%0D").replaceAll("\n", "%0A");
  process.stderr.write(`::error title=Semantic execution adapter::${oneLine}\n`);
}

function runProperty(env, root) {
  const gotestPath = env.GOTEST_JSON_PATH;
  if (!gotestPath) throw new SemanticExecutionAdapterError(["GOTEST_JSON_PATH is required for MODE=property"]);
  const { candidateSha, runRef, observedAt, run } = sharedContext(env);

  const model = loadSemanticModel(env.MODEL_DIR || DEFAULT_SEMANTIC_MODEL_DIR, { root });
  const bySegment = parseGoTestJSONShapeResults(readFileSync(gotestPath, "utf8"));

  const records = [];
  for (const [, binding] of model.bindings) {
    if (binding.status !== "active") continue;
    const classified = classifyTestRef(binding.test_ref);
    if (classified.system !== "property-shape") continue;
    const observation = propertyShapeObservation(bySegment, classified.shapeID);
    const classification = classifyRevisionBinding({ sourceSha: candidateSha }, {
      ...observation,
      sourceSha: candidateSha, // the whole go-test-json stream is one process, one commit
      aggregateNoOp: bySegment.size === 0,
    });
    records.push(
      toExecutionRecord({
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
          substrate: "runner",
        },
      }),
    );
  }
  return records;
}

function runCompat(env, root) {
  const casesPath = env.CASES_PATH;
  const bindingId = env.BINDING;
  if (!casesPath) throw new SemanticExecutionAdapterError(["CASES_PATH is required for MODE=compat"]);
  if (!bindingId) throw new SemanticExecutionAdapterError(["BINDING is required for MODE=compat"]);
  const { candidateSha, runRef, observedAt, run } = sharedContext(env);

  const model = loadSemanticModel(env.MODEL_DIR || DEFAULT_SEMANTIC_MODEL_DIR, { root });
  const binding = model.bindings.get(bindingId);
  if (!binding || binding.status !== "active") {
    throw new SemanticExecutionAdapterError([`BINDING ${JSON.stringify(bindingId)} is not an active binding in the semantic model`]);
  }

  const caseSet = parseCaseSet(JSON.parse(readFileSync(casesPath, "utf8")));
  const datasetFingerprint = env.CORPUS_PATH ? hashCorpus(env.CORPUS_PATH) : null;
  const referenceVersion = env.REFERENCE_VERSION || null;

  const candidate = { sourceSha: candidateSha };
  if (env.EXPECT_REFERENCE_VERSION) candidate.referenceVersion = env.EXPECT_REFERENCE_VERSION;

  const classification = classifyRevisionBinding(candidate, {
    sourceSha: candidateSha,
    referenceVersion,
    datasetFingerprint,
    selectedCount: caseSet.selectedCount,
    ranCount: caseSet.ranCount,
    aggregateNoOp: false,
    result: caseSet.result,
  });

  return [
    toExecutionRecord({
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
    }),
  ];
}

function runVerify(env, root) {
  const executionId = env.EXECUTION_ID;
  if (!executionId) throw new SemanticExecutionAdapterError(["EXECUTION_ID is required for MODE=verify"]);
  const { candidateSha } = sharedContext(env);

  const model = loadSemanticModel(env.MODEL_DIR || DEFAULT_SEMANTIC_MODEL_DIR, { root });
  const record = model.executions.get(executionId);
  if (!record) throw new SemanticExecutionAdapterError([`no execution ${JSON.stringify(executionId)} in the loaded model`]);

  const currentDatasetFingerprint = env.CORPUS_PATH ? hashCorpus(env.CORPUS_PATH) : null;
  const candidate = { sourceSha: candidateSha };
  if (env.EXPECT_REFERENCE_VERSION) candidate.referenceVersion = env.EXPECT_REFERENCE_VERSION;
  if (currentDatasetFingerprint) candidate.datasetFingerprint = currentDatasetFingerprint;

  const classification = classifyRevisionBinding(candidate, {
    sourceSha: record.source_sha,
    referenceVersion: record.reference_version,
    datasetFingerprint: record.dataset_fingerprint,
    selectedCount: record.selection === "executed" ? 1 : 0,
    ranCount: record.selection === "executed" ? 1 : 0,
    aggregateNoOp: record.selection === "no_op",
    result: record.result,
  });

  return [{ execution_id: executionId, still_valid_evidence: classification.evidence, classification }];
}

function main() {
  const mode = process.env.MODE;
  const root = process.cwd();
  let records;
  if (mode === "property") records = runProperty(process.env, root);
  else if (mode === "compat") records = runCompat(process.env, root);
  else if (mode === "verify") records = runVerify(process.env, root);
  else throw new SemanticExecutionAdapterError([`MODE must be one of property, compat, verify; got ${JSON.stringify(mode)}`]);

  const json = `${JSON.stringify(records, null, 2)}\n`;
  if (process.env.OUT) {
    writeFileSync(process.env.OUT, json);
    process.stdout.write(`semantic-execution-adapter: wrote ${records.length} record(s) to ${process.env.OUT}\n`);
  } else {
    process.stdout.write(json);
  }

  const nonEvidence = records.filter((r) => r.classification ? !r.classification.evidence : r.selection !== "executed");
  if (nonEvidence.length > 0) {
    process.stderr.write(
      `semantic-execution-adapter: ${nonEvidence.length}/${records.length} record(s) are NON-EVIDENCE (see selection_reason / classification.reason)\n`,
    );
  }
}

const invokedDirectly =
  process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (error) {
    errorAnnotation(error instanceof Error ? error.message : String(error));
    process.exit(1);
  }
}
