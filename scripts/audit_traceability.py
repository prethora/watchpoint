#!/usr/bin/env python3
"""
Traceability Audit Script

Verifies that every Flow ID defined in 99-traceability-matrix.md is referenced
somewhere in the codebase (.go and .py source files). Produces a detailed
report at reports/traceability_audit.md and exits with status 1 if any required
flows are missing.

Two-pass process:
  1. Parse the traceability matrix to extract the master list of required Flow IDs.
  2. Recursively scan all .go and .py source files for references to those IDs.
  3. Compute the gap (required - found) and generate the report.

Usage:
    python3 scripts/audit_traceability.py [--root DIR]

The --root flag overrides the project root directory (defaults to the parent of
the scripts/ directory where this file lives).
"""

import argparse
import os
import re
import sys
from collections import defaultdict
from datetime import datetime, timezone


# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

MATRIX_REL_PATH = os.path.join("architecture", "99-traceability-matrix.md")
REPORT_REL_PATH = os.path.join("reports", "traceability_audit.md")

# Regex to extract Flow IDs from the traceability matrix table rows.
# Matches rows like: | `FCST-001` | ... |
MATRIX_FLOW_RE = re.compile(r"^\|\s*`([A-Z]{2,5}-\d{3})`\s*\|", re.MULTILINE)

# Regex to find Flow ID references in source files.  Matches any occurrence
# of a well-formed Flow ID (2-5 uppercase letters, dash, 3 digits).  This is
# intentionally broad so it catches all comment styles:
#   // Flow: WPLC-001
#   // Per FCST-003 flow simulation
#   # Flow reference: EVAL-001
#   (NOTIF-005)
SOURCE_FLOW_RE = re.compile(r"\b([A-Z]{2,5}-\d{3})\b")

# File extensions to scan.
SOURCE_EXTENSIONS = {".go", ".py", ".yml", ".yaml"}

# Directories to skip during recursive scan.
SKIP_DIRS = {
    ".git",
    ".watchpoint",
    "vendor",
    "node_modules",
    "__pycache__",
    ".mypy_cache",
    ".venv",          # Virtual environments contain third-party code
    "architecture",   # Architecture docs are reference, not implementation
    ".aws-sam",
}


# ---------------------------------------------------------------------------
# Pass 1: Parse the traceability matrix
# ---------------------------------------------------------------------------

def parse_matrix(matrix_path: str) -> dict[str, dict]:
    """
    Parse 99-traceability-matrix.md and return a dict mapping each Flow ID
    to its metadata (name, impl type, component, domain).

    Only Section 2 ("Master Flow Traceability Matrix") rows are extracted.
    """
    with open(matrix_path, "r", encoding="utf-8") as f:
        content = f.read()

    flows: dict[str, dict] = {}
    current_domain = "Unknown"

    # Walk through lines to track the current domain heading.
    for line in content.splitlines():
        # Detect domain subsection headers like "### 2.1 Forecast Domain (FCST)"
        domain_match = re.match(r"^###\s+2\.\d+\s+(.+)$", line)
        if domain_match:
            current_domain = domain_match.group(1).strip()
            continue

        # Detect table rows with Flow IDs.
        row_match = re.match(
            r"^\|\s*`([A-Z]{2,5}-\d{3})`\s*\|\s*([^|]+)\|\s*`?([^|`]+)`?\s*\|\s*([^|]+)\|",
            line,
        )
        if row_match:
            flow_id = row_match.group(1)
            flow_name = row_match.group(2).strip()
            impl_type = row_match.group(3).strip()
            component = row_match.group(4).strip()
            flows[flow_id] = {
                "name": flow_name,
                "impl_type": impl_type,
                "component": component,
                "domain": current_domain,
            }

    return flows


# ---------------------------------------------------------------------------
# Pass 2: Scan source files
# ---------------------------------------------------------------------------

def scan_sources(root: str) -> dict[str, list[str]]:
    """
    Recursively scan .go and .py files under root for Flow ID references.

    Returns a dict mapping each found Flow ID to a list of
    "relative/path/to/file.go:line_number" strings.
    """
    found: dict[str, list[str]] = defaultdict(list)

    for dirpath, dirnames, filenames in os.walk(root):
        # Prune skipped directories (modifying dirnames in-place).
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]

        for filename in filenames:
            _, ext = os.path.splitext(filename)
            if ext not in SOURCE_EXTENSIONS:
                continue

            filepath = os.path.join(dirpath, filename)
            relpath = os.path.relpath(filepath, root)

            try:
                with open(filepath, "r", encoding="utf-8", errors="replace") as f:
                    for lineno, line in enumerate(f, start=1):
                        for match in SOURCE_FLOW_RE.finditer(line):
                            flow_id = match.group(1)
                            found[flow_id].append(f"{relpath}:{lineno}")
            except OSError:
                # Skip files we cannot read.
                continue

    return found


# ---------------------------------------------------------------------------
# Report generation
# ---------------------------------------------------------------------------

def generate_report(
    required: dict[str, dict],
    found: dict[str, list[str]],
    report_path: str,
) -> tuple[int, int, int]:
    """
    Generate the traceability audit report at report_path.

    Returns (total_required, covered_count, missing_count).
    """
    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    covered = {}
    missing = {}

    for flow_id, meta in sorted(required.items()):
        if flow_id in found:
            covered[flow_id] = meta
        else:
            missing[flow_id] = meta

    total = len(required)
    covered_count = len(covered)
    missing_count = len(missing)
    coverage_pct = (covered_count / total * 100) if total > 0 else 0.0

    # Group missing flows by domain for readability.
    missing_by_domain: dict[str, list[tuple[str, dict]]] = defaultdict(list)
    for flow_id, meta in sorted(missing.items()):
        missing_by_domain[meta["domain"]].append((flow_id, meta))

    # Group covered flows by domain.
    covered_by_domain: dict[str, list[tuple[str, dict]]] = defaultdict(list)
    for flow_id, meta in sorted(covered.items()):
        covered_by_domain[meta["domain"]].append((flow_id, meta))

    # Build the report.
    lines: list[str] = []
    w = lines.append

    w("# Traceability Audit Report")
    w("")
    w(f"> Generated: {now}")
    w(f"> Source: `architecture/99-traceability-matrix.md`")
    w("")
    w("## Summary")
    w("")
    w(f"| Metric | Value |")
    w(f"|---|---|")
    w(f"| Required Flows | {total} |")
    w(f"| Covered Flows | {covered_count} |")
    w(f"| Missing Flows | {missing_count} |")
    w(f"| Coverage | {coverage_pct:.1f}% |")
    w(f"| Status | {'PASS' if missing_count == 0 else 'FAIL'} |")
    w("")

    # --- Missing flows section ---
    if missing_count > 0:
        w("## Missing Flows")
        w("")
        w("The following flows from the traceability matrix have **no references** ")
        w("in the codebase source files (.go, .py):")
        w("")

        for domain, flow_list in sorted(missing_by_domain.items()):
            w(f"### {domain}")
            w("")
            w("| Flow ID | Flow Name | Impl Type | Component |")
            w("|---|---|---|---|")
            for flow_id, meta in flow_list:
                w(f"| `{flow_id}` | {meta['name']} | `{meta['impl_type']}` | {meta['component']} |")
            w("")
    else:
        w("## Missing Flows")
        w("")
        w("None. All required flows are referenced in the codebase.")
        w("")

    # --- Covered flows section ---
    w("## Covered Flows")
    w("")

    for domain, flow_list in sorted(covered_by_domain.items()):
        w(f"### {domain}")
        w("")
        w("| Flow ID | Flow Name | Reference Count | Sample Location |")
        w("|---|---|---|---|")
        for flow_id, meta in flow_list:
            refs = found[flow_id]
            sample = refs[0] if refs else "-"
            w(f"| `{flow_id}` | {meta['name']} | {len(refs)} | `{sample}` |")
        w("")

    # --- Extra flows (found in code but not in matrix) ---
    extra_ids = sorted(set(found.keys()) - set(required.keys()))
    if extra_ids:
        w("## Extra Flows (Not in Matrix)")
        w("")
        w("The following Flow IDs were found in source code but are not listed in")
        w("the traceability matrix. These may be implementation-specific sub-flows,")
        w("internal references, or candidates for addition to the matrix.")
        w("")
        w("| Flow ID | Reference Count | Sample Location |")
        w("|---|---|---|")
        for flow_id in extra_ids:
            refs = found[flow_id]
            sample = refs[0] if refs else "-"
            w(f"| `{flow_id}` | {len(refs)} | `{sample}` |")
        w("")

    # Write the report.
    os.makedirs(os.path.dirname(report_path), exist_ok=True)
    with open(report_path, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")

    return total, covered_count, missing_count


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main() -> int:
    parser = argparse.ArgumentParser(
        description="Audit flow traceability from architecture matrix to source code."
    )
    parser.add_argument(
        "--root",
        default=None,
        help="Project root directory (default: parent of scripts/).",
    )
    args = parser.parse_args()

    # Resolve project root.
    if args.root:
        root = os.path.abspath(args.root)
    else:
        # Default: parent directory of the scripts/ folder containing this file.
        root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

    matrix_path = os.path.join(root, MATRIX_REL_PATH)
    report_path = os.path.join(root, REPORT_REL_PATH)

    if not os.path.isfile(matrix_path):
        print(f"ERROR: Traceability matrix not found at {matrix_path}", file=sys.stderr)
        return 1

    # Pass 1: Parse the matrix.
    print(f"[1/3] Parsing traceability matrix: {MATRIX_REL_PATH}")
    required = parse_matrix(matrix_path)
    print(f"       Found {len(required)} required flow IDs.")

    if len(required) == 0:
        print("ERROR: No flow IDs extracted from the matrix. Check the file format.", file=sys.stderr)
        return 1

    # Pass 2: Scan sources.
    print(f"[2/3] Scanning source files (.go, .py) under {root}")
    found = scan_sources(root)
    unique_found = set(found.keys())
    print(f"       Found {len(unique_found)} unique flow IDs referenced in source files.")

    # Pass 3: Generate report.
    print(f"[3/3] Generating report: {REPORT_REL_PATH}")
    total, covered, missing = generate_report(required, found, report_path)

    # Print summary.
    print()
    print(f"  Required:  {total}")
    print(f"  Covered:   {covered}")
    print(f"  Missing:   {missing}")
    print(f"  Coverage:  {covered / total * 100:.1f}%" if total > 0 else "  Coverage:  N/A")
    print()

    if missing > 0:
        print(f"FAIL: {missing} flow(s) are not referenced in the codebase.")
        print(f"See {REPORT_REL_PATH} for details.")
        return 1

    print("PASS: All required flows are referenced in the codebase.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
