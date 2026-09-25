// surface-coverage.mjs — the "Coverage at a glance" tally over the
// surface-parity ledger (test/surface-parity/inventory/), shared by the
// renderer that writes the table into docs/coverage.md (gen-coverage.mjs) and
// the gate that re-derives every cell of it (doc-counts.mjs).
//
// Each ledger entry carries a `head` and a `class`. The glance table has one
// row per head plus a total row, and one column per GLANCE_COLUMNS entry:
// `probed` counts every entry; each other column counts the entries whose
// class it lists. `wrong-accept` folds into Supported: it is cerberus
// answering a shape the bare-call probe's reference rejects (range() /
// step() driven outside a query context), which is supported surface for a
// reader, not a gap. `wrong-reject` has a column of its own, headed with the
// scope it measures — symbols, not argument shapes.

// The heads the ledger probes, in the order docs/coverage.md tabulates them.
export const SURFACE_HEADS = [
  { head: 'promql', label: 'PromQL' },
  { head: 'logql', label: 'LogQL' },
  { head: 'traceql', label: 'TraceQL' },
];

// The key of the aggregate row that closes the table.
export const SURFACE_TOTAL_ROW = 'total';

// The glance-table columns: the tally key, the header docs/coverage.md
// prints, and the ledger classes the column counts (null = every entry).
export const GLANCE_COLUMNS = [
  { key: 'probed', title: 'Symbols probed', classes: null },
  { key: 'supported', title: 'Supported (incl. experimental)', classes: ['parity-accept', 'wrong-accept'] },
  { key: 'parityRejected', title: 'Intentionally rejected (parity)', classes: ['parity-reject'] },
  { key: 'wrongRejected', title: 'Wrong-rejected symbols', classes: ['wrong-reject'] },
];

const CLASS_COLUMN = Object.fromEntries(
  GLANCE_COLUMNS.filter((c) => c.classes).flatMap((c) => c.classes.map((cls) => [cls, c.key])),
);

// surfaceParityTotals — the per-head tallies (plus the total row) for
// `inventory.entries`. An unknown head or class throws rather than landing in
// no column: a ledger that grew a fourth verdict must not be able to shrink
// the wrong-rejection cell by falling off the end of the class map.
export function surfaceParityTotals(inventory) {
  const blank = () => Object.fromEntries(GLANCE_COLUMNS.map((c) => [c.key, 0]));
  const out = { [SURFACE_TOTAL_ROW]: blank() };
  for (const { head } of SURFACE_HEADS) out[head] = blank();
  for (const entry of inventory.entries ?? []) {
    const row = out[entry.head];
    if (!row || entry.head === SURFACE_TOTAL_ROW) {
      throw new Error(`surface-parity inventory carries head "${entry.head}", which docs/coverage.md does not tabulate`);
    }
    const column = CLASS_COLUMN[entry.class];
    if (!column) {
      throw new Error(`surface-parity inventory carries class "${entry.class}", which maps to no glance column`);
    }
    for (const key of ['probed', column]) {
      row[key] += 1;
      out[SURFACE_TOTAL_ROW][key] += 1;
    }
  }
  return out;
}
