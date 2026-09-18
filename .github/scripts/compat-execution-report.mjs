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
// for that case: it calls no CLASSIFICATION logic of its own — every
// record's classification comes from the SAME compatExecutionRecord (which
// itself composes classifyRevisionBinding/toExecutionRecord/execIdFor) the
// CLI's own runCompat uses, just looped over the model's own binding roster
// instead of one caller-named BINDING id. This script's own logic is
// exactly the fan-out itself: resolving each binding to the right case set
// (the transport-arm split below), degrading a binding gracefully instead
// of losing the whole report when ITS case set can't be read (issue #3509),
// and correcting one fan-out-specific unsoundness a single-binding CLI call
// never hits — a binding whose test_ref names one corpus file within a
// shared case set cannot have an aggregate "fail" attributed to it
// specifically (issue #3508; see isCorpusFileScopedBinding below).
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
// A case-set path that cannot be read/parsed NEVER aborts the whole report
// (issue #3509) — it degrades only the binding(s) that resolve to that one
// path to selection "unavailable", with a reason naming the path and the
// underlying error; every other binding's record is produced normally.
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
import process from "node:process";
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { error } from "./lib/gh.mjs";
import { DEFAULT_SEMANTIC_MODEL_DIR, loadSemanticModel } from "./lib/semantic-model.mjs";
import {
  SemanticExecutionAdapterError,
  compatExecutionRecord,
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
 * True for a binding whose test_ref names ONE corpus DATA file (a `.yml` /
 * `.yaml` query-corpus fixture) rather than the whole driver invocation. A
 * driver source file (`.go`) or the bare `compatibility/<head>` directory
 * names the WHOLE run a case set already aggregates over — sound in both
 * directions, exactly the granularity tempo's per-transport bindings and
 * the corpus-wide bindings already rely on. A corpus data file names only
 * a SUBSET of the cases the shared run's case set carries, and
 * score.Case today has no field attributing a case back to the corpus
 * file it came from (cerberus issue #3508) — so an aggregate "fail" driven
 * by a case outside this file is not evidence THIS binding's own behavior
 * regressed, only that reading this predicate false is unsafe to skip.
 */
export function isCorpusFileScopedBinding(testRef) {
  return /\.ya?ml$/i.test(testRef);
}

/**
 * The reason stamped on a corpus-file-scoped binding's record when the
 * shared case set's aggregate result is "fail" — see
 * isCorpusFileScopedBinding above and issue #3508's own analysis. A pass
 * verdict is never touched: every case in the set agreeing IS real
 * evidence every behavior agreed, corpus-file-scoped bindings included.
 */
const CORPUS_FILE_ATTRIBUTION_REASON =
  "the shared compat run's case set has a failing case, but this binding's test_ref names one corpus " +
  "file within that larger run and score.Case carries no per-case corpus-file attribution today — the " +
  "failure cannot be confirmed to belong to this binding's own file rather than an unrelated case in " +
  "the same run (cerberus issue #3508)";

/**
 * Degrades a corpus-file-scoped binding's "executed"/"fail" record to
 * non-evidence ("unavailable") — the one correction compat-execution-
 * report.mjs's fan-out needs beyond the shared compatExecutionRecord()
 * composition (issue #3508). Every other record (a pass, or a
 * driver-scoped binding) passes through unchanged.
 */
export function guardCorpusFileAttribution(record, testRef) {
  if (!isCorpusFileScopedBinding(testRef)) return record;
  if (record.selection !== "executed" || record.result !== "fail") return record;
  return {
    ...record,
    selection: "unavailable",
    result: "error",
    selection_reason: CORPUS_FILE_ATTRIBUTION_REASON,
  };
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
  const matchesHead = (testRef) => testRef === prefix || testRef.startsWith(`${prefix}/`);
  return [...model.bindings.values()].filter(
    (b) => b.status === "active" && b.evidence_class === "reference" && matchesHead(b.test_ref),
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

  // Resolved once per distinct path (a Map, not a re-read per binding), and
  // a read/parse failure NEVER throws out of this — it degrades only the
  // binding(s) that resolve to that path (issue #3509). Losing the whole
  // report to one bad case-set artifact would also lose every OTHER
  // binding's perfectly good evidence, which is exactly the failure this
  // script exists to avoid.
  const caseSetCache = new Map();
  const loadCaseSet = (path) => {
    if (caseSetCache.has(path)) return caseSetCache.get(path);
    let outcome;
    try {
      outcome = { ok: true, caseSet: parseCaseSet(JSON.parse(readFileSync(path, "utf8"))) };
    } catch (err) {
      // stderr, not the warning()/::warning:: workflow-command helper: this
      // runs mid-report, with a real JSON payload for the successfully
      // resolved bindings still to be printed on stdout afterward (see
      // main()'s own OUT-unset branch below) — an annotation on stdout
      // would interleave with and corrupt that JSON, exactly the failure
      // mode issue #3509 exists to avoid for the RECORDS themselves.
      const message = err instanceof Error ? err.message : String(err);
      const reason = `case set at ${JSON.stringify(path)} could not be loaded: ${message}`;
      process.stderr.write(`compat-execution-report: ${reason}\n`);
      outcome = { ok: false, reason };
    }
    caseSetCache.set(path, outcome);
    return outcome;
  };

  const datasetFingerprint = env.CORPUS_PATH ? hashCorpus(parseCorpusPath(env.CORPUS_PATH)) : null;
  const referenceVersion = env.REFERENCE_VERSION || null;

  const candidate = { sourceSha: candidateSha };
  if (env.EXPECT_REFERENCE_VERSION) candidate.referenceVersion = env.EXPECT_REFERENCE_VERSION;

  const records = bindings.map((binding) => {
    const path = casesPathGrpc && isGrpcBinding(binding.test_ref) ? casesPathGrpc : casesPath;
    const outcome = loadCaseSet(path);
    if (!outcome.ok) {
      return toExecutionRecord({
        id: execIdFor(binding.id, observedAt),
        binding: binding.id,
        observedAt,
        runRef,
        classification: { selection: "unavailable", evidence: false, result: "error", reason: outcome.reason },
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
    }
    const record = compatExecutionRecord({
      binding,
      candidate,
      caseSet: outcome.caseSet,
      observedAt,
      runRef,
      run,
      referenceVersion,
      datasetFingerprint,
    });
    return guardCorpusFileAttribution(record, binding.test_ref);
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
    error(message, { title: "compat execution report" });
    process.exit(1);
  }
}
