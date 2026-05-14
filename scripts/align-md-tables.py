#!/usr/bin/env python3
"""Align markdown tables in-place so MD060 (table-column-style: aligned) passes.

markdownlint's MD060 measures pipe positions by Unicode display width via the
`string-width` npm package, not bytes or codepoints. For the cerberus repo
every char in every table is display-width 1 (or whitespace), so codepoint
counting matches display width exactly. If that ever stops being true
(emoji or CJK in tables), swap `len(stripped)` for `wcwidth.wcswidth(stripped)`.

Usage: align-md-tables.py FILE [FILE ...]
"""

import re
import sys


def align_table(table_lines: list[str]) -> list[str]:
    rows = []
    for line in table_lines:
        s = line.rstrip("\n").strip()
        if not s.startswith("|") or not s.endswith("|"):
            return table_lines
        rows.append([c for c in s[1:-1].split("|")])
    if not rows:
        return table_lines
    n_cols = len(rows[0])
    widths = [0] * n_cols
    sep_idx = None
    for i, row in enumerate(rows):
        if len(row) != n_cols:
            return table_lines
        for j, cell in enumerate(row):
            stripped = cell.strip()
            if re.fullmatch(r":?-+:?", stripped):
                sep_idx = i
            w = len(stripped)
            if w > widths[j]:
                widths[j] = w
    out = []
    for i, row in enumerate(rows):
        cells = []
        for j, cell in enumerate(row):
            stripped = cell.strip()
            if i == sep_idx:
                cells.append(" " + "-" * widths[j] + " ")
            else:
                pad = widths[j] - len(stripped)
                cells.append(" " + stripped + " " * pad + " ")
        out.append("|" + "|".join(cells) + "|\n")
    return out


def align_file(path: str) -> bool:
    with open(path, "r", encoding="utf-8") as f:
        lines = f.readlines()
    out = []
    i = 0
    while i < len(lines):
        if lines[i].lstrip().startswith("|") and lines[i].rstrip().endswith("|"):
            j = i
            while (
                j < len(lines)
                and lines[j].lstrip().startswith("|")
                and lines[j].rstrip().endswith("|")
            ):
                j += 1
            out.extend(align_table(lines[i:j]))
            i = j
        else:
            out.append(lines[i])
            i += 1
    if out != lines:
        with open(path, "w", encoding="utf-8") as f:
            f.writelines(out)
        return True
    return False


def main() -> int:
    changed = False
    for path in sys.argv[1:]:
        if not path.endswith(".md"):
            continue
        if align_file(path):
            changed = True
    return 0 if not changed else 0  # always exit 0; lefthook stages fixed files


if __name__ == "__main__":
    sys.exit(main())
