#!/usr/bin/env python3
"""Coverage gate for HexaGo's core packages.

Reads the Go coverprofile and the Python Cobertura XML named in
tools/coverage_targets.yml, computes statement coverage for each listed
package, writes a Markdown table (stdout and $GITHUB_STEP_SUMMARY) and an
optional JSON summary, and exits 1 if any package is below the threshold.

A listed package with no statements in the report is "pending" (not yet
written) and does not fail the gate.

Usage:
    python tools/coverage_gate.py [--config tools/coverage_targets.yml]
                                  [--json coverage-summary.json] [--root .]
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import xml.etree.ElementTree as ET
from dataclasses import asdict, dataclass
from pathlib import Path

import yaml

REPO_ROOT = Path(__file__).resolve().parent.parent


@dataclass
class PackageCoverage:
    language: str
    package: str
    statements: int
    covered: int
    threshold: float

    @property
    def percent(self) -> float | None:
        if self.statements == 0:
            return None
        return 100.0 * self.covered / self.statements

    @property
    def status(self) -> str:
        pct = self.percent
        if pct is None:
            return "pending"
        # Exact comparison: 95.999% fails a 96% gate even though it prints as 96.00%.
        return "pass" if self.covered * 100 >= self.threshold * self.statements else "fail"

    def to_dict(self) -> dict:
        d = asdict(self)
        d["percent"] = None if self.percent is None else round(self.percent, 2)
        d["status"] = self.status
        return d


def _in_package(path: str, package: str) -> bool:
    package = package.strip("/")
    return path == package or path.startswith(package + "/")


def go_coverage(profile_text: str, module: str, packages: list[str],
                threshold: float) -> list[PackageCoverage]:
    """Per-package statement coverage from a Go coverprofile.

    With -coverpkg each test binary reports every block, so a block appears
    once per package tested; it counts as covered if any binary hit it.
    """
    blocks: dict[str, tuple[int, bool]] = {}
    for line in profile_text.splitlines():
        line = line.strip()
        if not line or line.startswith("mode:"):
            continue
        try:
            block, stmts, count = line.rsplit(" ", 2)
            n, hit = int(stmts), int(count) > 0
        except ValueError:
            continue
        prev = blocks.get(block)
        blocks[block] = (n, hit or (prev is not None and prev[1]))

    prefix = module.rstrip("/") + "/"
    results = []
    for pkg in packages:
        total = covered = 0
        for block, (n, hit) in blocks.items():
            file_path = block.split(":", 1)[0]
            rel = file_path[len(prefix):] if file_path.startswith(prefix) else file_path
            if _in_package(rel.rsplit("/", 1)[0], pkg):
                total += n
                covered += n if hit else 0
        results.append(PackageCoverage("go", pkg, total, covered, threshold))
    return results


def python_coverage(xml_text: str, packages: list[str],
                    threshold: float) -> list[PackageCoverage]:
    """Per-package (or per-file) line coverage from a Cobertura XML (pytest-cov)."""
    root = ET.fromstring(xml_text)
    sources = [(s.text or "").replace("\\", "/").rstrip("/")
               for s in root.iter("source")]
    # filename -> (statements, covered, paths the file may be known by).
    # Cobertura filenames are relative to a <source> (an absolute CI path),
    # so a package matches on the filename or on source/filename.
    files: dict[str, tuple[int, int, list[str]]] = {}
    for cls in root.iter("class"):
        filename = cls.get("filename", "").replace("\\", "/")
        lines = list(cls.iter("line"))
        hits = sum(1 for ln in lines if int(ln.get("hits", "0")) > 0)
        paths = [filename] + [f"{s}/{filename}" for s in sources if s]
        files[filename] = (len(lines), hits, paths)

    results = []
    for pkg in packages:
        pkg = pkg.strip("/")
        total = covered = 0
        for n, h, paths in files.values():
            # A package directory, or a single file (e.g. ".../fallback_parser.py").
            if any(p.startswith(pkg + "/") or f"/{pkg}/" in p or p == pkg or p.endswith("/" + pkg)
                   for p in paths):
                total += n
                covered += h
        results.append(PackageCoverage("python", pkg, total, covered, threshold))
    return results


def render_markdown(results: list[PackageCoverage], threshold: float) -> str:
    icon = {"pass": "✅", "fail": "❌", "pending": "⏳"}
    lines = [
        f"### Core coverage gate (threshold {threshold:g}%)",
        "",
        "| Language | Package | Statements | Covered | Coverage | Status |",
        "|---|---|---:|---:|---:|---|",
    ]
    for r in results:
        pct = "—" if r.percent is None else f"{r.percent:.2f}%"
        lines.append(f"| {r.language} | `{r.package}` | {r.statements} | "
                     f"{r.covered} | {pct} | {icon[r.status]} {r.status} |")
    failed = [r for r in results if r.status == "fail"]
    lines.append("")
    if failed:
        lines.append(f"**Gate failed:** {len(failed)} package(s) below {threshold:g}%.")
    else:
        lines.append("**Gate passed.**")
    return "\n".join(lines) + "\n"


def overall(results: list[PackageCoverage]) -> float | None:
    total = sum(r.statements for r in results)
    if total == 0:
        return None
    return round(100.0 * sum(r.covered for r in results) / total, 2)


def run(config_path: Path, root: Path, json_out: Path | None,
        summary_path: str | None) -> int:
    cfg = yaml.safe_load(config_path.read_text(encoding="utf-8"))
    threshold = float(cfg.get("threshold", 96.0))
    results: list[PackageCoverage] = []
    errors: list[str] = []

    go_cfg = cfg.get("go") or {}
    if go_cfg.get("packages"):
        profile = root / go_cfg["profile"]
        if profile.exists():
            results += go_coverage(profile.read_text(encoding="utf-8"),
                                   go_cfg["module"], go_cfg["packages"], threshold)
        else:
            errors.append(f"Go coverprofile not found: {go_cfg['profile']}")

    py_cfg = cfg.get("python") or {}
    if py_cfg.get("packages"):
        report = root / py_cfg["report"]
        if report.exists():
            results += python_coverage(report.read_text(encoding="utf-8"),
                                       py_cfg["packages"], threshold)
        else:
            errors.append(f"Python coverage report not found: {py_cfg['report']}")

    md = render_markdown(results, threshold)
    if errors:
        md += "\n" + "\n".join(f"**Error:** {e}" for e in errors) + "\n"
    print(md)
    if summary_path:
        with open(summary_path, "a", encoding="utf-8") as fh:
            fh.write(md)
    if json_out:
        json_out.write_text(json.dumps({
            "threshold": threshold,
            "overall_percent": overall(results),
            "packages": [r.to_dict() for r in results],
            "errors": errors,
        }, indent=2), encoding="utf-8")

    return 1 if errors or any(r.status == "fail" for r in results) else 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--config", type=Path, default=REPO_ROOT / "tools" / "coverage_targets.yml")
    ap.add_argument("--root", type=Path, default=REPO_ROOT)
    ap.add_argument("--json", type=Path, default=None)
    args = ap.parse_args(argv)
    if hasattr(sys.stdout, "reconfigure"):  # emoji on Windows consoles
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    return run(args.config, args.root, args.json, os.environ.get("GITHUB_STEP_SUMMARY"))


if __name__ == "__main__":
    sys.exit(main())
