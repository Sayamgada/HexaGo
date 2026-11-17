#!/usr/bin/env python3
"""Test-first check: a PR that adds a source file under services/ must also
have a matching test.

Matching rules:
  Go      foo.go       -> foo_test.go in the same directory, or any *_test.go
                          added to that directory in the same PR.
  Python  foo.py       -> test_foo.py or foo_test.py anywhere in the
                          service's tests/ tree (existing or added).

Skipped: test files themselves, generated files (*.pb.go, *_gen.go,
zz_generated*, *_pb2.py, or a "Code generated ... DO NOT EDIT" header),
entrypoints (main.go, cmd/**, main.py, __main__.py, __init__.py, conftest.py),
and migrations (any path segment named migrations or alembic).

Usage: python tools/test_first_check.py --base <sha> [--head <sha>]
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
from pathlib import Path
from typing import Callable

REPO_ROOT = Path(__file__).resolve().parent.parent
GENERATED_HEADER = re.compile(r"^// Code generated .* DO NOT EDIT\.$|^# Code generated .* DO NOT EDIT", re.M)
SOURCE_EXTS = (".go", ".py")


def is_test(path: str) -> bool:
    name = path.rsplit("/", 1)[-1]
    return (name.endswith("_test.go") or (name.startswith("test_") and name.endswith(".py"))
            or name.endswith("_test.py") or "/tests/" in f"/{path}")


def skip_reason(path: str, read: Callable[[str], str] | None = None) -> str | None:
    """Why `path` needs no test (None means it does)."""
    parts = path.split("/")
    name = parts[-1]
    if not path.startswith("services/") or not name.endswith(SOURCE_EXTS):
        return "not a services/ source file"
    if is_test(path):
        return "test file"
    if any(p in ("migrations", "alembic") for p in parts):
        return "migration"
    if name in ("main.go", "main.py", "__main__.py", "__init__.py", "conftest.py") or "cmd" in parts:
        return "entrypoint"
    if (name.endswith((".pb.go", "_gen.go", "_pb2.py", "_pb2_grpc.py"))
            or name.startswith("zz_generated")):
        return "generated"
    if read is not None:
        try:
            head = read(path)[:2_000]
        except OSError:
            head = ""
        if GENERATED_HEADER.search(head):
            return "generated"
    return None


def service_root(path: str) -> str:
    return "/".join(path.split("/")[:2])  # services/<name>


def has_matching_test(path: str, added: set[str], all_files: set[str]) -> bool:
    directory, name = path.rsplit("/", 1)
    stem = name.rsplit(".", 1)[0]
    if name.endswith(".go"):
        if f"{directory}/{stem}_test.go" in all_files:
            return True
        return any(a.rsplit("/", 1)[0] == directory and a.endswith("_test.go") for a in added)
    root = service_root(path)
    wanted = {f"test_{stem}.py", f"{stem}_test.py"}
    return any(f.startswith(root + "/") and f.rsplit("/", 1)[-1] in wanted for f in all_files)


def check(added: list[str], all_files: set[str], read: Callable[[str], str] | None = None) -> tuple[list[str], list[str]]:
    """Return (missing, checked): new source files lacking a test, and all checked."""
    added_set = set(added)
    missing, checked = [], []
    for path in added:
        if skip_reason(path, read):
            continue
        checked.append(path)
        if not has_matching_test(path, added_set, all_files):
            missing.append(path)
    return missing, checked


def git_lines(*args: str) -> list[str]:
    out = subprocess.run(["git", *args], cwd=REPO_ROOT, capture_output=True, text=True, check=True)
    return [ln for ln in out.stdout.splitlines() if ln.strip()]


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Fail if new services/ source lacks a test.")
    ap.add_argument("--base", required=True)
    ap.add_argument("--head", default="HEAD")
    args = ap.parse_args(argv)
    if hasattr(sys.stdout, "reconfigure"):  # emoji on Windows consoles
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")

    added = git_lines("diff", "--name-only", "--diff-filter=A", f"{args.base}...{args.head}", "--", "services/")
    all_files = set(git_lines("ls-tree", "-r", "--name-only", args.head))

    def read(p: str) -> str:
        return subprocess.run(["git", "show", f"{args.head}:{p}"], cwd=REPO_ROOT,
                              capture_output=True, text=True).stdout

    missing, checked = check(added, all_files, read)
    lines = ["### Test-first check", ""]
    if not checked:
        lines.append("No new source files under `services/`.")
    for p in checked:
        lines.append(f"- {'❌ no test for' if p in missing else '✅'} `{p}`")
    if missing:
        lines += ["", f"**{len(missing)} new source file(s) have no matching test.** "
                  "Add `<name>_test.go` (Go) or `tests/test_<name>.py` (Python)."]
    md = "\n".join(lines) + "\n"
    print(md)
    if summary := os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(summary, "a", encoding="utf-8") as fh:
            fh.write(md)
    return 1 if missing else 0


if __name__ == "__main__":
    sys.exit(main())
