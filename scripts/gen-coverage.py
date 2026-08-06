#!/usr/bin/env python3
"""Regenerate the per-function coverage tables in docs/coverage.md from the
test/surface-parity conformance ledger.

The surface-parity inventory (test/surface-parity/inventory.json) is a
machine-readable conformance LEDGER: every grammar symbol the three upstream
parsers expose, paired with cerberus's accept/reject verdict and the reference
backend's verdict, classified four ways (parity-accept / parity-reject /
wrong-reject / wrong-accept). That vocabulary is correct for the test ratchet
but wrong for a human reader — an experimental function cerberus implements is
a SUPPORTED function, and range()/step() land in "wrong-accept" only because
the ledger probes them as bare top-level calls, a shape the reference rejects
for query-context reasons. This script performs that translation and emits the
tables between the AUTOGEN markers in docs/coverage.md.

Usage:  python3 scripts/gen-coverage.py        # rewrites docs/coverage.md tables
        python3 scripts/gen-coverage.py --check # exit 1 if the doc is stale
"""
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
INVENTORY = os.path.join(ROOT, "test", "surface-parity", "inventory.json")
DOC = os.path.join(ROOT, "docs", "coverage.md")

BEGIN = "<!-- BEGIN AUTOGEN: coverage-tables (scripts/gen-coverage.py) -->"
END = "<!-- END AUTOGEN: coverage-tables -->"

GLANCE_BEGIN = "<!-- BEGIN AUTOGEN: coverage-glance (scripts/gen-coverage.py) -->"
GLANCE_END = "<!-- END AUTOGEN: coverage-glance -->"

HEAD_LABELS = [("promql", "PromQL"), ("logql", "LogQL"), ("traceql", "TraceQL")]

# The glance table's four columns and the ledger classes each one tallies.
# `wrong-accept` folds into Supported for the same reason status() surfaces it
# that way: it is cerberus answering a shape the bare-call probe's reference
# rejects, not a gap. `wrong-reject` gets a column of its own, headed with the
# scope it actually measures — symbols, not argument shapes.
GLANCE_COLUMNS = [
    ("Supported (incl. experimental)", {"parity-accept", "wrong-accept"}),
    ("Intentionally rejected (parity)", {"parity-reject"}),
    ("Wrong-rejected symbols", {"wrong-reject"}),
]

# PromQL functions the upstream parser marks Experimental (gated behind
# --enable-feature=promql-experimental-functions, which cerberus enables in its
# prod parser config; internal/api/prom/handler.go). Kept in sync with the
# upstream parser.Functions table.
PROMQL_EXPERIMENTAL = {
    "double_exponential_smoothing", "first_over_time", "histogram_quantiles",
    "info", "mad_over_time", "sort_by_label", "sort_by_label_desc",
    "ts_of_first_over_time", "ts_of_last_over_time", "ts_of_max_over_time",
    "ts_of_min_over_time", "range", "step", "start", "end",
}

KIND_TITLES = {
    "promql": {
        "aggregator": "Aggregations", "function": "Functions",
        "binary-op": "Binary operators", "modifier": "Modifiers",
    },
    "logql": {
        "vector-agg": "Vector aggregations", "range-agg": "Range aggregations",
        "parser-stage": "Parser stages", "label-fn": "Label / format stages",
        "label-filter": "Label filters", "line-filter": "Line filters",
        "conv-fn": "Conversion functions", "binary-op": "Binary operators",
    },
    "traceql": {
        "aggregate": "Aggregates", "intrinsic": "Intrinsics",
        "metrics-op": "Metrics operators",
    },
}


def status(entry):
    """Translate a ledger class into honest user-facing support state."""
    sym = entry["symbol"].split(":", 1)[-1]
    cls = entry["class"]
    is_exp = entry["head"] == "promql" and sym in PROMQL_EXPERIMENTAL
    if cls == "parity-accept":
        return "Supported (experimental)" if is_exp else "Supported"
    if cls == "parity-reject":
        # Both cerberus and the reference reject. For the experimental
        # query-context fns (start/end) this is a real intentional gate.
        return "Rejected (parity with reference)"
    if cls == "wrong-accept":
        # Cerberus accepts, the probe's reference rejects. For
        # range()/step() this is a faithful experimental implementation the
        # bare-call probe can't see; surface it as supported-experimental.
        if is_exp:
            return "Supported (experimental)"
        return "Supported (cerberus extension)"
    if cls == "wrong-reject":
        return "Not yet supported"
    return cls


def _mdwidth(cell):
    """Cell width as markdownlint's MD060 measures it. It aligns by source
    column but treats a backslash escape of an ordinary char (`\\w`, `\\d`) as
    a single glyph (dropping the backslash), while keeping a genuinely-escaped
    pipe (`\\|`) at its two source bytes. So the effective width is the source
    length minus the number of `\\<non-pipe>` escapes."""
    backslash_escapes = len(re.findall(r"\\.", cell))
    escaped_pipes = cell.count("\\|")
    return len(cell) - (backslash_escapes - escaped_pipes)


def aligned_table(header, rows):
    """Emit a GitHub table with columns padded so the source pipes land where
    markdownlint's MD060 'aligned' style expects them."""
    cols = list(zip(*([header] + rows)))
    widths = [max(3, max(_mdwidth(c) for c in col)) for col in cols]

    def pad(cell, w):
        return cell + " " * (w - _mdwidth(cell))

    def fmt(cells):
        return "| " + " | ".join(pad(c, widths[i])
                                 for i, c in enumerate(cells)) + " |"

    sep = "| " + " | ".join("-" * widths[i] for i in range(len(header))) + " |"
    return [fmt(header), sep] + [fmt(r) for r in rows]


def render_head(head, entries):
    out = []
    titles = KIND_TITLES[head]
    by_kind = {}
    for x in entries:
        by_kind.setdefault(x["kind"], []).append(x)
    for kind in [k for k in titles if k in by_kind]:
        out.append(f"#### {titles[kind]}\n")
        rows = []
        for x in sorted(by_kind[kind], key=lambda y: y["symbol"]):
            sym = x["symbol"].split(":", 1)[-1]
            probe = x["probe"].replace("|", "\\|")
            rows.append([f"`{sym}`", f"`{probe}`", status(x)])
        out.extend(aligned_table(["Symbol", "Probe", "Status"], rows))
        out.append("")
    return "\n".join(out)


def build_glance(entries):
    """Render the per-head summary counts. Every cell is a tally over the
    ledger's `class` field, so the headline coverage figures a reader acts on
    are derived rather than typed. An unknown class raises: a ledger that grew
    a fourth verdict must not be able to shrink a column by falling through."""
    known = {c for _, classes in GLANCE_COLUMNS for c in classes}
    unknown = {x["class"] for x in entries} - known
    if unknown:
        raise SystemExit(f"gen-coverage: ledger class(es) {sorted(unknown)} "
                         f"map to no glance column")
    rows = []
    totals = [0] * (1 + len(GLANCE_COLUMNS))
    for head, label in HEAD_LABELS:
        he = [x for x in entries if x["head"] == head]
        counts = [len(he)] + [sum(1 for x in he if x["class"] in classes)
                              for _, classes in GLANCE_COLUMNS]
        totals = [t + c for t, c in zip(totals, counts)]
        rows.append([label] + [str(c) for c in counts])
    rows.append(["**Total**"] + [f"**{c}**" for c in totals])
    header = ["Head", "Symbols probed"] + [c for c, _ in GLANCE_COLUMNS]
    return "\n".join(aligned_table(header, rows)) + "\n"


def build():
    with open(INVENTORY) as f:
        inv = json.load(f)
    e = inv["entries"]
    blocks = []
    for head, label in HEAD_LABELS:
        he = [x for x in e if x["head"] == head]
        blocks.append(f"### {label} ({len(he)} symbols)\n")
        blocks.append(render_head(head, he))
    return "\n".join(blocks).rstrip() + "\n"


def splice(doc, begin, end, body):
    return re.sub(re.escape(begin) + r".*?" + re.escape(end),
                  begin + "\n\n" + body + "\n" + end, doc, flags=re.S)


def main():
    with open(INVENTORY) as f:
        entries = json.load(f)["entries"]
    with open(DOC) as f:
        doc = f.read()
    new = splice(doc, GLANCE_BEGIN, GLANCE_END, build_glance(entries))
    new = splice(new, BEGIN, END, build())
    if "--check" in sys.argv:
        sys.exit(0 if new == doc else 1)
    with open(DOC, "w") as f:
        f.write(new)


if __name__ == "__main__":
    main()
