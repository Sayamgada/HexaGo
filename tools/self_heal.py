#!/usr/bin/env python3
"""Self-healing CI for HexaGo.

Run by .github/workflows/self-heal.yml after a CI run fails. Two phases:

  plan   Decide whether to heal (guardrails) and which suites failed, and
         write `skip`, `reason`, `suites`, `need_go`, `need_python`,
         `need_flutter` to $GITHUB_OUTPUT so the workflow sets up only the
         toolchains it needs.
  fix    Download the failing job logs, extract failing tests and stack
         traces, collect the referenced source and test files (~60k chars),
         ask the LLM (LiteLLM, model from LLM_MODEL) for a minimal unified
         diff, apply it, rerun only the failing suites, and loop up to
         MAX_ATTEMPTS times feeding each new failure back in. Then push
         autofix/<run-id> and open a PR: labelled "ai-autofix" with
         auto-merge (squash) when green, or a draft labelled
         "ai-autofix-failed" when still red.

Guardrails: never touch .github/, never delete tests, at most
MAX_FILES_PER_PATCH files per patch, skip runs triggered from autofix
branches or autofix merge commits, skip re-run attempts, and one healing
run (at most MAX_ATTEMPTS LLM calls) per failing workflow run.

Configuration comes from environment variables (see Event.from_env).
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable

# The workflow runs trusted tools from the default branch against the failing
# checkout, so the repo being healed can differ from this file's repo.
REPO_ROOT = Path(os.environ.get("HEXAGO_REPO_ROOT") or Path(__file__).resolve().parent.parent).resolve()

MAX_ATTEMPTS = 3
MAX_FILES_PER_PATCH = 5
CONTEXT_CHAR_CAP = 60_000
LOG_EXCERPT_CAP = 12_000
OUTPUT_TAIL_CAP = 6_000
DEFAULT_MODEL = "gemini/gemini-3.8-flash"
# Tried in order after LLM_MODEL (override with LLM_FALLBACK_MODELS, comma-separated).
DEFAULT_FALLBACK_MODELS = ("gemini/gemini-3.5-flash-lite", "gemini/gemini-3.8-flash")
AUTOFIX_PREFIX = "autofix/"
AUTOFIX_MARKER = "[ai-autofix"  # in autofix PR titles, hence squash-merge commits
LABEL_OK = "ai-autofix"
LABEL_FAILED = "ai-autofix-failed"
PROTECTED_PREFIXES = (".github/",)


# ---------------------------------------------------------------------------
# Suites: CI job name -> how to rerun it locally.
# ---------------------------------------------------------------------------

@dataclass(frozen=True)
class Suite:
    name: str
    root: str                 # where its sources live (for path resolution)
    commands: tuple[str, ...]  # run from the repo root, in order
    toolchains: tuple[str, ...]
    hint: str = ""


GO = "cd services/dispatch-go && "
PY = "cd services/agents-py && "
SUITES: dict[str, Suite] = {
    "go": Suite("go", "services/dispatch-go",
                (GO + "go vet ./...", GO + "go test -race ./..."), ("go",)),
    "python": Suite("python", "services/agents-py",
                    (PY + "ruff check --config ../../tools/ruff.toml .", PY + "pytest -q"),
                    ("python",)),
    "flutter": Suite("flutter", "apps/hexago_app",
                     ("cd apps/hexago_app && flutter pub get",
                      "cd apps/hexago_app && flutter analyze",
                      "cd apps/hexago_app && flutter test"), ("flutter",)),
    "tools": Suite("tools", "tools",
                   ("ruff check --config tools/ruff.toml tools",
                    "python -m pytest tools/tests -q"), ()),
    "coverage-gate": Suite(
        "coverage-gate", "services",
        (GO + "go test -covermode=atomic -coverpkg=./internal/... -coverprofile=coverage.out ./...",
         PY + "pytest -q --cov=src --cov-report=xml:coverage.xml",
         "python tools/coverage_gate.py"),
        ("go", "python"),
        hint="The coverage gate failed: add or extend tests for the uncovered code. "
             "Do not lower the threshold or edit tools/coverage_targets.yml."),
    "test-first": Suite(
        "test-first", "services",
        ("python tools/test_first_check.py --base {base_sha}",), (),
        hint="A new source file has no matching test file: add a real test for it."),
}


def suite_for_job(job_name: str) -> Suite | None:
    """Map a CI job name (e.g. 'go' or 'go (dispatch-go)') to a Suite."""
    key = job_name.strip().lower().split(" ")[0]
    return SUITES.get(key)


# ---------------------------------------------------------------------------
# Event + guardrails
# ---------------------------------------------------------------------------

@dataclass
class Event:
    repo: str
    run_id: str
    run_attempt: int = 1
    conclusion: str = "failure"
    head_branch: str = ""
    head_sha: str = ""
    head_commit_message: str = ""
    head_repo: str = ""
    base_sha: str = ""          # PR base, when the run came from a pull request
    run_url: str = ""

    @classmethod
    def from_env(cls, env: dict[str, str] | None = None) -> "Event":
        env = dict(os.environ if env is None else env)
        repo = env.get("GITHUB_REPOSITORY", "")
        run_id = env.get("RUN_ID", "")
        return cls(
            repo=repo,
            run_id=run_id,
            run_attempt=int(env.get("RUN_ATTEMPT") or 1),
            conclusion=env.get("RUN_CONCLUSION", "failure"),
            head_branch=env.get("HEAD_BRANCH", ""),
            head_sha=env.get("HEAD_SHA", ""),
            head_commit_message=env.get("HEAD_COMMIT_MESSAGE", ""),
            head_repo=env.get("HEAD_REPO", repo),
            base_sha=env.get("PR_BASE_SHA", ""),
            run_url=env.get("RUN_URL") or f"https://github.com/{repo}/actions/runs/{run_id}",
        )

    @property
    def branch(self) -> str:
        return f"{AUTOFIX_PREFIX}{self.run_id}"


def trigger_skip_reason(ev: Event) -> str | None:
    """Return why this run must not be healed, or None to proceed."""
    if ev.conclusion != "failure":
        return f"run concluded {ev.conclusion!r}, not failure"
    if not ev.run_id or not ev.repo:
        return "missing RUN_ID or GITHUB_REPOSITORY"
    if ev.head_branch.startswith(AUTOFIX_PREFIX):
        return f"triggering branch {ev.head_branch} is an autofix branch (no loops)"
    if AUTOFIX_MARKER in ev.head_commit_message:
        return "triggering commit is an autofix merge (no loops)"
    if ev.head_repo and ev.head_repo != ev.repo:
        return f"run is from fork {ev.head_repo}; untrusted code is not healed"
    if ev.run_attempt > 1:
        return f"run attempt {ev.run_attempt} is a re-run; only the first attempt is healed"
    return None


# ---------------------------------------------------------------------------
# GitHub API (urllib; injectable for tests)
# ---------------------------------------------------------------------------

class GitHubError(RuntimeError):
    def __init__(self, status: int, message: str):
        super().__init__(f"GitHub API {status}: {message}")
        self.status = status


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):  # noqa: D401
        return None


class GitHub:
    def __init__(self, repo: str, token: str, api: str = "https://api.github.com"):
        self.repo, self.token, self.api = repo, token, api.rstrip("/")

    def _request(self, method: str, url: str, body: dict | None = None,
                 follow: bool = True) -> tuple[int, bytes, dict]:
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Accept", "application/vnd.github+json")
        req.add_header("X-GitHub-Api-Version", "2022-11-28")
        req.add_header("User-Agent", "hexago-self-heal")
        if self.token:
            req.add_header("Authorization", f"Bearer {self.token}")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        opener = urllib.request.build_opener() if follow else urllib.request.build_opener(_NoRedirect)
        try:
            with opener.open(req, timeout=60) as resp:
                return resp.status, resp.read(), dict(resp.headers)
        except urllib.error.HTTPError as e:
            if not follow and e.code in (301, 302, 303, 307, 308):
                return e.code, b"", dict(e.headers)
            raise GitHubError(e.code, e.read().decode(errors="replace")[:500]) from None

    def _url(self, path: str) -> str:
        return path if path.startswith("http") else f"{self.api}/{path.lstrip('/')}"

    def get(self, path: str) -> dict | list:
        return json.loads(self._request("GET", self._url(path))[1] or b"null")

    def post(self, path: str, body: dict) -> dict:
        return json.loads(self._request("POST", self._url(path), body)[1] or b"null")

    def put(self, path: str, body: dict) -> dict:
        return json.loads(self._request("PUT", self._url(path), body)[1] or b"null")

    def get_text(self, path: str) -> str:
        """GET a text resource, following the log redirect without auth."""
        status, body, headers = self._request("GET", self._url(path), follow=False)
        if status in (301, 302, 303, 307, 308):
            loc = headers.get("Location") or headers.get("location")
            with urllib.request.urlopen(loc, timeout=120) as resp:  # signed URL, no auth
                body = resp.read()
        return body.decode("utf-8", errors="replace")

    def graphql(self, query: str, variables: dict) -> dict:
        return self.post(f"{self.api}/graphql", {"query": query, "variables": variables})

    # -- helpers used by the healer ------------------------------------
    def branch_exists(self, branch: str) -> bool:
        try:
            self.get(f"repos/{self.repo}/branches/{branch}")
            return True
        except GitHubError as e:
            if e.status == 404:
                return False
            raise

    def failed_jobs(self, run_id: str) -> list[dict]:
        jobs = self.get(f"repos/{self.repo}/actions/runs/{run_id}/jobs?per_page=100&filter=latest")
        return [j for j in jobs.get("jobs", []) if j.get("conclusion") == "failure"]

    def job_log(self, job_id: int) -> str:
        return self.get_text(f"repos/{self.repo}/actions/jobs/{job_id}/logs")


# ---------------------------------------------------------------------------
# Log parsing
# ---------------------------------------------------------------------------

TIMESTAMP_RE = re.compile(r"^\ufeff?\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z ?")
ANSI_RE = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")
GO_FAIL_TEST_RE = re.compile(r"--- FAIL: (\S+)")
GO_FAIL_PKG_RE = re.compile(r"^FAIL\s+(\S+/\S+)\s")
PY_FAILED_RE = re.compile(r"^(?:FAILED|ERROR) ([\w./\\-]+\.py)(?:::(\S+))?")
DART_FAIL_RE = re.compile(r"^\d+:\d\d \+\d+(?: ~\d+)?(?: -\d+)?: (.+?) \[E\]\s*$")
FILE_REF_RES = (
    re.compile(r"([\w./\\-]+\.(?:go|py|dart)):(\d+)"),
    re.compile(r'File "([^"]+\.py)", line (\d+)'),
    # Dart stack frames: "package:hexago_app/x.dart 44:7" or "test/x_test.dart 30:5"
    re.compile(r"((?:package:hexago_app/)?[\w./-]+\.dart) (\d+):\d+"),
)
DART_PACKAGE = "package:hexago_app/"
MARKER_RE = re.compile(
    r"(--- FAIL|^FAIL\b|panic:|FAILED|ERROR\b|Error:|error:|\[E\]|Traceback|"
    r"AssertionError|Expected:|Actual:|_test\.go:\d+|\.py:\d+|\.dart:\d+|"
    r"Gate failed|below \d|Process completed with exit code)")


@dataclass
class Failure:
    tests: list[str] = field(default_factory=list)
    go_packages: list[str] = field(default_factory=list)
    file_refs: list[tuple[str, int]] = field(default_factory=list)
    excerpt: str = ""


def clean_log(text: str) -> list[str]:
    return [ANSI_RE.sub("", TIMESTAMP_RE.sub("", ln)).rstrip() for ln in text.splitlines()]


def _dedupe(seq):
    seen, out = set(), []
    for x in seq:
        if x not in seen:
            seen.add(x)
            out.append(x)
    return out


def excerpt(lines: list[str], cap: int = LOG_EXCERPT_CAP, radius: int = 12) -> str:
    """Lines around failure markers (merged windows), keeping the tail if long."""
    hits = [i for i, ln in enumerate(lines) if MARKER_RE.search(ln)]
    if not hits:
        text = "\n".join(lines)
        return text[-cap:]
    keep: set[int] = set()
    for i in hits:
        keep.update(range(max(0, i - radius), min(len(lines), i + radius + 1)))
    out, prev = [], -2
    for i in sorted(keep):
        if i != prev + 1:
            out.append("...")
        out.append(lines[i])
        prev = i
    text = "\n".join(out)
    return text if len(text) <= cap else "...\n" + text[-cap:]


def parse_failures(log_text: str) -> Failure:
    lines = clean_log(log_text)
    f = Failure()
    for ln in lines:
        if m := GO_FAIL_TEST_RE.search(ln):
            f.tests.append(m.group(1))
        if m := GO_FAIL_PKG_RE.search(ln):
            f.go_packages.append(m.group(1))
        if m := PY_FAILED_RE.search(ln.strip()):
            f.tests.append(m.group(1) + (f"::{m.group(2)}" if m.group(2) else ""))
            f.file_refs.append((m.group(1).replace("\\", "/"), 0))
        if m := DART_FAIL_RE.search(ln.strip()):
            f.tests.append(m.group(1))
        for rx in FILE_REF_RES:
            for m in rx.finditer(ln):
                path = m.group(1).replace("\\", "/")
                if path.startswith(DART_PACKAGE):
                    path = "lib/" + path[len(DART_PACKAGE):]
                f.file_refs.append((path, int(m.group(2))))
    f.tests = _dedupe(f.tests)
    f.go_packages = _dedupe(f.go_packages)
    f.file_refs = _dedupe(f.file_refs)
    f.excerpt = excerpt(lines)
    return f


def merge_failures(failures: list[Failure]) -> Failure:
    out = Failure()
    for f in failures:
        out.tests += f.tests
        out.go_packages += f.go_packages
        out.file_refs += f.file_refs
    out.tests, out.go_packages = _dedupe(out.tests), _dedupe(out.go_packages)
    out.file_refs = _dedupe(out.file_refs)
    out.excerpt = "\n\n".join(f.excerpt for f in failures if f.excerpt)[-LOG_EXCERPT_CAP:]
    return out


# ---------------------------------------------------------------------------
# Context collection
# ---------------------------------------------------------------------------

def tracked_files(repo_root: Path) -> list[str]:
    out = subprocess.run(["git", "ls-files"], cwd=repo_root, capture_output=True,
                         text=True, check=True).stdout
    return [p for p in out.splitlines() if p]


def resolve_ref(ref: str, tracked: list[str], prefer: list[str]) -> str | None:
    """Map a log path (absolute, relative to a module, or a basename) to a tracked file."""
    parts = [p for p in ref.replace("\\", "/").split("/") if p and p not in (".", "..")]
    for i in range(len(parts)):  # longest suffix first; shorter ones only get vaguer
        suffix = "/".join(parts[i:])
        matches = [t for t in tracked if t == suffix or t.endswith("/" + suffix)]
        if not matches:
            continue
        if len(matches) == 1:
            return matches[0]
        preferred = [m for m in matches if any(m.startswith(p.rstrip("/") + "/") for p in prefer)]
        return preferred[0] if len(preferred) == 1 else None
    return None


def go_module(repo_root: Path) -> str:
    mod = repo_root / "services/dispatch-go/go.mod"
    if mod.exists():
        m = re.search(r"^module\s+(\S+)", mod.read_text(encoding="utf-8"), re.M)
        if m:
            return m.group(1)
    return ""


def go_package_dirs(packages: list[str], module: str) -> list[str]:
    dirs = []
    for pkg in packages:
        if module and pkg.startswith(module):
            dirs.append("services/dispatch-go" + pkg[len(module):])
    return dirs


def is_test_file(path: str) -> bool:
    name = path.rsplit("/", 1)[-1]
    return (name.endswith("_test.go") or name.endswith("_test.dart")
            or (name.endswith(".py") and (name.startswith("test_") or name.endswith("_test.py")))
            or "/tests/" in f"/{path}" or "/test/" in f"/{path}")


def collect_context(failure: Failure, suites: list[Suite], repo_root: Path,
                    tracked: list[str], cap: int = CONTEXT_CHAR_CAP) -> tuple[str, list[str]]:
    """Referenced files first, then the rest of each failing Go package."""
    prefer = go_package_dirs(failure.go_packages, go_module(repo_root)) + [s.root for s in suites]
    ordered: list[str] = []
    for ref, _ in failure.file_refs:
        hit = resolve_ref(ref, tracked, prefer)
        if hit and not hit.startswith(PROTECTED_PREFIXES):
            ordered.append(hit)
    for d in go_package_dirs(failure.go_packages, go_module(repo_root)):
        ordered += sorted(t for t in tracked if t.startswith(d + "/")
                          and t.endswith(".go") and "/" not in t[len(d) + 1:])
    ordered = _dedupe(ordered)

    chunks, used, included = [], 0, []
    for path in ordered:
        p = repo_root / path
        if not p.is_file():
            continue
        body = p.read_text(encoding="utf-8", errors="replace")
        block = f"=== {path} ===\n{body}\n"
        if used + len(block) > cap:
            room = cap - used - len(path) - 64
            if room < 2_000:
                break
            block = f"=== {path} (truncated) ===\n{body[:room]}\n... [truncated]\n"
        chunks.append(block)
        included.append(path)
        used += len(block)
    return "".join(chunks), included


# ---------------------------------------------------------------------------
# LLM prompt + response
# ---------------------------------------------------------------------------

SYSTEM_PROMPT = f"""You are the self-healing CI engineer for HexaGo, a Go / Python / Flutter monorepo.
A CI suite failed. Produce the smallest correct change that makes it pass.

Rules:
1. Reply with "EXPLANATION:" followed by at most 5 sentences on the root cause and the fix,
   then exactly one ```diff fenced block holding a unified diff in git format: paths relative
   to the repo root with a/ and b/ prefixes, correct @@ hunk headers, and at least 3 lines of
   unchanged context copied exactly from the files shown.
2. Fix the code under test. Change a test only if the test itself is clearly wrong.
3. Never modify anything under .github/.
4. Never delete tests, skip them, or weaken their assertions.
5. Change at most {MAX_FILES_PER_PATCH} files.
6. Edit only files shown in the context unless a new file is unavoidable."""


@dataclass
class Attempt:
    number: int
    status: str            # passed | failed | rejected | apply-failed | no-diff | llm-error
    explanation: str = ""
    diff: str = ""
    files: list[str] = field(default_factory=list)
    output: str = ""


def tail(text: str, cap: int = OUTPUT_TAIL_CAP) -> str:
    return text if len(text) <= cap else "...\n" + text[-cap:]


def build_messages(suites: list[Suite], failure: Failure, before_output: str,
                   context: str, history: list[Attempt]) -> list[dict]:
    hints = "\n".join(s.hint for s in suites if s.hint)
    user = [
        f"Failing suites: {', '.join(s.name for s in suites)}",
        f"Failing tests: {', '.join(failure.tests) or '(see output)'}",
    ]
    if hints:
        user.append(hints)
    user += [
        "\n## CI log excerpt\n```\n" + failure.excerpt + "\n```",
        "\n## Output when rerun in the sandbox\n```\n" + tail(before_output) + "\n```",
        "\n## Source and test files\n" + context,
    ]
    for a in history:
        user.append(
            f"\n## Attempt {a.number} ({a.status})\nYour patch:\n```diff\n{a.diff}\n```\n"
            f"Result:\n```\n{tail(a.output, 4_000)}\n```")
    if history:
        user.append("\nThe previous attempt did not work. Produce a new complete patch "
                    "against the ORIGINAL files shown above (earlier patches were reverted).")
    return [{"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": "\n".join(user)}]


UNSUPPORTED_PARAM_RE = re.compile(r"UnsupportedParams|does not support param", re.I)
DIFF_BLOCK_RE = re.compile(r"```(?:diff|patch)?[ \t]*\n(.*?)```", re.S)


def parse_llm_response(text: str) -> tuple[str, str]:
    """Return (diff, explanation) from an LLM reply."""
    diff = ""
    for m in DIFF_BLOCK_RE.finditer(text):
        body = m.group(1)
        if re.search(r"^(--- |\+\+\+ |diff --git |@@ )", body, re.M):
            diff = body
            break
    if not diff:
        m = re.search(r"^(diff --git |--- a/)", text, re.M)
        diff = text[m.start():] if m else ""
    m = re.search(r"EXPLANATION:\s*(.*?)(?:```|$)", text, re.S | re.I)
    explanation = (m.group(1) if m else DIFF_BLOCK_RE.sub("", text)).strip()
    if diff and not diff.endswith("\n"):
        diff += "\n"
    return diff, explanation[:1_500]


def call_llm(messages: list[dict], model: str,
             completion: Callable | None = None) -> str:
    if completion is None:
        import litellm  # heavy import; only needed for real runs
        completion = litellm.completion
    kwargs = {"temperature": 0, "timeout": 180, "drop_params": True}
    try:
        resp = completion(model=model, messages=messages, **kwargs)
    except Exception as e:
        # Some models reject sampling params outright (e.g. gemini-3.x: temperature).
        if not UNSUPPORTED_PARAM_RE.search(f"{type(e).__name__} {e}"):
            raise
        kwargs.pop("temperature")
        resp = completion(model=model, messages=messages, **kwargs)
    try:
        return resp.choices[0].message.content or ""
    except (AttributeError, IndexError, KeyError):
        return resp["choices"][0]["message"]["content"] or ""


RETIRED_MODEL_RE = re.compile(r"404|NotFound|not found|no longer available|deprecated", re.I)
# A spent daily/plan quota will not recover within the run: drop the model, don't back off.
QUOTA_RE = re.compile(r"exceeded your current quota|quota exceeded|RESOURCE_EXHAUSTED.*quota", re.I | re.S)


TRANSIENT_RE = re.compile(
    r"(429|500|502|503|504)|UNAVAILABLE|overloaded|high demand|RateLimit|"
    r"ServiceUnavailable|InternalServerError|Timeout|APIConnectionError", re.I)
RETRY_DELAYS_S = (15, 30, 60, 90)


class LLM:
    """Calls the first model that works.

    Transient errors (429/5xx/timeouts) are retried with backoff on the same
    model; a retired model (404) is dropped for the rest of the run.
    """

    def __init__(self, models: list[str], completion: Callable | None = None,
                 sleep: Callable[[float], None] = time.sleep,
                 delays: tuple[float, ...] = RETRY_DELAYS_S):
        self.models = _dedupe([m for m in models if m])
        self.completion = completion
        self.sleep, self.delays = sleep, delays
        self.used: str | None = None
        self.errors: list[str] = []

    def __call__(self, messages: list[dict]) -> str:
        failures = []
        for model in list(self.models):
            for wait in (*self.delays, None):
                try:
                    text = call_llm(messages, model, self.completion)
                except Exception as e:  # provider errors vary by backend
                    kind = f"{type(e).__name__} {e}"
                    msg = f"{model}: {type(e).__name__}: {e}"[:600]
                    self.errors.append(msg)
                    if QUOTA_RE.search(kind) or (RETIRED_MODEL_RE.search(kind) and not TRANSIENT_RE.search(kind)):
                        self.models.remove(model)
                    elif TRANSIENT_RE.search(kind) and wait is not None:
                        print(f"self-heal: {model} transient error, retrying in {wait}s", flush=True)
                        self.sleep(wait)
                        continue
                    failures.append(msg)
                    break
                self.used = model
                return text
        raise RuntimeError("every model failed: " + " | ".join(failures or ["no models configured"]))


def models_from_env(env: dict[str, str] | None = None) -> list[str]:
    env = dict(os.environ if env is None else env)
    fallbacks = [m.strip() for m in (env.get("LLM_FALLBACK_MODELS") or "").split(",") if m.strip()]
    return _dedupe([env.get("LLM_MODEL") or DEFAULT_MODEL, *(fallbacks or DEFAULT_FALLBACK_MODELS)])


# ---------------------------------------------------------------------------
# Patch guardrails + application
# ---------------------------------------------------------------------------

TEST_DEF_RES = (
    re.compile(r"^func ((?:Test|Benchmark|Fuzz|Example)\w*)\("),
    re.compile(r"^\s*(?:async\s+)?def (test_\w+)\s*\("),
    re.compile(r"""^\s*(?:test|testWidgets|group)\(\s*(['"])(.+?)\1"""),
)
SKIP_ADD_RES = (
    re.compile(r"\bt\.Skip(?:Now|f)?\("),
    re.compile(r"@pytest\.mark\.(?:skip|xfail)|pytest\.skip\("),
    re.compile(r"\bskip:\s*(?:true|['\"])"),
)


@dataclass
class PatchInfo:
    files: list[str]
    deleted: list[str]
    created: list[str]


DIFF_PATH_RE = re.compile(r"^(--- |\+\+\+ )(a/|b/)?(\S+)(.*)$")
DIFF_GIT_RE = re.compile(r"^diff --git a/(\S+) b/(\S+)$")


def normalize_diff_paths(diff: str, tracked: list[str], roots: list[str]) -> str:
    """Rewrite paths the LLM gave relative to a service (e.g. src/x.py) to repo paths.

    A path that is already tracked is kept; otherwise it is resolved to the
    tracked file it uniquely names (preferring the failing suites' roots), and
    a new file is placed under the single suite root whose directory exists.
    """
    tracked_set = set(tracked)
    dirs = {t.rsplit("/", 1)[0] for t in tracked if "/" in t}
    cache: dict[str, str] = {}

    def fix(path: str) -> str:
        if path == "/dev/null" or path in tracked_set:
            return path
        if path not in cache:
            hit = resolve_ref(path, tracked, roots)
            if hit is None:
                parent = path.rsplit("/", 1)[0] if "/" in path else ""
                under = [f"{r.rstrip('/')}/{path}" for r in roots
                         if not parent or f"{r.rstrip('/')}/{parent}" in dirs]
                hit = under[0] if len(under) == 1 else path
            cache[path] = hit
        return cache[path]

    out = []
    for ln in diff.splitlines(keepends=True):
        body = ln.rstrip("\r\n")
        end = ln[len(body):]
        if m := DIFF_GIT_RE.match(body):
            body = f"diff --git a/{fix(m.group(1))} b/{fix(m.group(2))}"
        elif (m := DIFF_PATH_RE.match(body)) and (m.group(2) or m.group(3) == "/dev/null"):
            body = f"{m.group(1)}{m.group(2) or ''}{fix(m.group(3))}{m.group(4)}"
        out.append(body + end)
    return "".join(out)


def patch_files(diff: str) -> PatchInfo:
    files, deleted, created = [], [], []
    old = None
    for ln in diff.splitlines():
        if ln.startswith("--- "):
            old = ln[4:].split("\t")[0].strip()
        elif ln.startswith("+++ ") and old is not None:
            new = ln[4:].split("\t")[0].strip()
            strip = lambda p: p[2:] if p[:2] in ("a/", "b/") else p  # noqa: E731
            if new == "/dev/null":
                files.append(strip(old))
                deleted.append(strip(old))
            else:
                files.append(strip(new))
                if old == "/dev/null":
                    created.append(strip(new))
            old = None
    return PatchInfo(_dedupe(files), deleted, created)


def _test_names(lines: list[str]) -> set[str]:
    names = set()
    for ln in lines:
        for rx in TEST_DEF_RES:
            if m := rx.search(ln):
                names.add(m.group(m.lastindex))
    return names


def validate_patch(diff: str, max_files: int = MAX_FILES_PER_PATCH) -> list[str]:
    """Guardrail violations for a patch (empty list = acceptable)."""
    if not diff.strip():
        return ["no unified diff found in the reply"]
    info = patch_files(diff)
    errors = []
    if not info.files:
        errors.append("diff names no files")
    if len(info.files) > max_files:
        errors.append(f"patch changes {len(info.files)} files (max {max_files})")
    for p in info.files:
        if p.startswith(PROTECTED_PREFIXES) or p.startswith("/") or ".." in p.split("/"):
            errors.append(f"patch touches protected or unsafe path {p}")
    for p in info.deleted:
        if is_test_file(p):
            errors.append(f"patch deletes test file {p}")
    removed = [ln[1:] for ln in diff.splitlines() if ln.startswith("-") and not ln.startswith("---")]
    added = [ln[1:] for ln in diff.splitlines() if ln.startswith("+") and not ln.startswith("+++")]
    gone = _test_names(removed) - _test_names(added)
    if gone:
        errors.append(f"patch removes tests: {', '.join(sorted(gone))}")
    if any(rx.search(ln) for ln in added for rx in SKIP_ADD_RES):
        errors.append("patch adds a test skip")
    return errors


Runner = Callable[..., subprocess.CompletedProcess]


def apply_patch(diff: str, repo_root: Path, run: Runner = subprocess.run) -> tuple[bool, str]:
    """git apply (with recount), falling back to GNU patch with fuzz."""
    with tempfile.NamedTemporaryFile("w", suffix=".diff", delete=False, encoding="utf-8",
                                     newline="") as fh:
        fh.write(diff)
        path = fh.name
    errors = []
    try:
        for cmd in (["git", "apply", "--recount", "--whitespace=nowarn", path],
                    ["patch", "-p1", "--forward", "--batch", "--fuzz=3",
                     "--no-backup-if-mismatch", "-r", "-", "-i", path]):
            check = cmd[:2] + (["--check"] if cmd[0] == "git" else ["--dry-run"]) + cmd[2:]
            try:
                res = run(check, cwd=repo_root, capture_output=True, text=True)
                if res.returncode == 0:
                    res = run(cmd, cwd=repo_root, capture_output=True, text=True)
                    if res.returncode == 0:
                        return True, ""
                errors.append(f"$ {' '.join(cmd[:2])}: {(res.stderr or res.stdout).strip()}")
            except FileNotFoundError as e:
                errors.append(f"$ {cmd[0]}: {e}")
        return False, "\n".join(errors)
    finally:
        os.unlink(path)


def reset_worktree(repo_root: Path, run: Runner = subprocess.run) -> None:
    run(["git", "checkout", "--", "."], cwd=repo_root, capture_output=True)
    run(["git", "clean", "-fdq", "-e", ".selfheal"], cwd=repo_root, capture_output=True)


def run_suites(suites: list[Suite], repo_root: Path, base_sha: str = "",
               run: Runner = subprocess.run, timeout: int = 1_200) -> tuple[bool, str]:
    out, ok = [], True
    for s in suites:
        for cmd in s.commands:
            cmd = cmd.format(base_sha=shlex.quote(base_sha or "HEAD~1"))
            try:
                res = run(cmd, shell=True, cwd=repo_root, capture_output=True,
                          text=True, timeout=timeout)
                rc, text = res.returncode, (res.stdout or "") + (res.stderr or "")
            except subprocess.TimeoutExpired:
                rc, text = 124, f"timed out after {timeout}s"
            out.append(f"$ {cmd}\n{text.rstrip()}\n[exit {rc}]")
            if rc != 0:
                ok = False
                break  # later commands in this suite depend on earlier ones
    return ok, "\n".join(out)


# ---------------------------------------------------------------------------
# Healing loop
# ---------------------------------------------------------------------------

@dataclass
class HealResult:
    passed: bool
    attempts: list[Attempt]
    before_output: str
    after_output: str
    llm_calls: int
    reproduced: bool = True


def heal(suites: list[Suite], failure: Failure, repo_root: Path, tracked: list[str],
         llm: Callable[[list[dict]], str], base_sha: str = "",
         run: Runner = subprocess.run, max_attempts: int = MAX_ATTEMPTS) -> HealResult:
    ok, before = run_suites(suites, repo_root, base_sha, run)
    if ok:
        return HealResult(True, [], before, before, 0, reproduced=False)
    context, _ = collect_context(failure, suites, repo_root, tracked)
    history: list[Attempt] = []
    after, calls = before, 0
    for n in range(1, max_attempts + 1):
        if getattr(llm, "models", True) == []:  # every model retired or out of quota
            break
        calls += 1
        try:
            reply = llm(build_messages(suites, failure, before, context, history))
        except Exception as e:  # counts as a failed attempt; never crash the run
            history.append(Attempt(n, "llm-error", output=f"LLM call failed: {e}"[:4_000]))
            continue
        diff, explanation = parse_llm_response(reply)
        diff = normalize_diff_paths(diff, tracked, [s.root for s in suites])
        attempt = Attempt(n, "no-diff", explanation, diff, patch_files(diff).files)
        history.append(attempt)
        problems = validate_patch(diff)
        if problems:
            attempt.status = "no-diff" if not diff.strip() else "rejected"
            attempt.output = "Guardrails rejected the patch:\n- " + "\n- ".join(problems)
            if not diff.strip():
                attempt.output += "\n\nRaw reply (excerpt):\n" + (reply or "(empty)")[:1_500]
            continue
        reset_worktree(repo_root, run)
        applied, err = apply_patch(diff, repo_root, run)
        if not applied:
            attempt.status, attempt.output = "apply-failed", "Patch did not apply:\n" + err
            continue
        ok, after = run_suites(suites, repo_root, base_sha, run)
        attempt.status, attempt.output = ("passed" if ok else "failed"), after
        if ok:
            return HealResult(True, history, before, after, calls)
    # Leave the last patch that applied in place for the draft PR.
    reset_worktree(repo_root, run)
    last = next((a for a in reversed(history) if a.status == "failed"), None)
    if last:
        apply_patch(last.diff, repo_root, run)
        after = last.output
    return HealResult(False, history, before, after, calls)


# ---------------------------------------------------------------------------
# PR body + publishing
# ---------------------------------------------------------------------------

def pr_title(ev: Event, suites: list[Suite], passed: bool) -> str:
    what = "repair" if passed else "attempted repair of"
    return f"fix(autofix): {what} {'/'.join(s.name for s in suites)} failure {AUTOFIX_MARKER} run {ev.run_id}]"


def pr_body(ev: Event, suites: list[Suite], failure: Failure, result: HealResult,
            model: str, cap: int = 60_000) -> str:
    final = next((a for a in reversed(result.attempts) if a.status in ("passed", "failed")), None)
    final = final or (result.attempts[-1] if result.attempts else Attempt(0, ""))
    status = ("✅ green in the sandbox" if result.passed
              else f"❌ still red after {len(result.attempts)} attempt(s)")
    lines = [
        f"Automated fix for CI run [{ev.run_id}]({ev.run_url}) on `{ev.head_branch}` "
        f"(`{ev.head_sha[:7]}`).",
        "",
        f"**Status:** {status} · **Suites:** {', '.join(s.name for s in suites)} · "
        f"**Model:** `{model}` · **LLM calls:** {result.llm_calls}",
        f"**Failing tests:** {', '.join(f'`{t}`' for t in failure.tests[:15]) or 'see output'}",
        "",
        "### Explanation",
        final.explanation or "_No explanation returned._",
        "",
        "### Attempts",
        "| # | Result | Files |",
        "|---|---|---|",
    ]
    for a in result.attempts:
        lines.append(f"| {a.number} | {a.status} | {', '.join(f'`{f}`' for f in a.files) or '—'} |")
    for a in result.attempts:
        lines += ["", f"<details><summary>Attempt {a.number}: {a.status}</summary>", "",
                  "```diff", a.diff.strip() or "(no diff)", "```", "", "```",
                  tail(a.output, 3_000), "```", "</details>"]
    lines += ["", "### Test output before", "<details><summary>failing</summary>", "", "```",
              tail(result.before_output, 6_000), "```", "</details>",
              "", "### Test output after",
              f"<details><summary>{'passing' if result.passed else 'still failing'}</summary>",
              "", "```", tail(result.after_output, 6_000), "```", "</details>", "",
              "_Opened by `tools/self_heal.py` (.github/workflows/self-heal.yml). "
              "Guardrails: no `.github/` edits, no deleted tests, "
              f"≤{MAX_FILES_PER_PATCH} files per patch._"]
    body = "\n".join(lines)
    return body if len(body) <= cap else body[: cap - 40] + "\n\n_[truncated]_"


AUTOMERGE_MUTATION = """
mutation($id: ID!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: SQUASH}) {
    pullRequest { number autoMergeRequest { mergeMethod } }
  }
}"""


def ensure_label(gh: GitHub, name: str, color: str, description: str) -> None:
    try:
        gh.post(f"repos/{gh.repo}/labels", {"name": name, "color": color, "description": description})
    except GitHubError as e:
        if e.status != 422:  # 422 = already exists
            raise


def enable_auto_merge(gh: GitHub, pr: dict) -> tuple[bool, str]:
    try:
        resp = gh.graphql(AUTOMERGE_MUTATION, {"id": pr["node_id"]})
    except GitHubError as e:
        return False, str(e)
    if resp and resp.get("errors"):
        return False, "; ".join(err.get("message", "") for err in resp["errors"])
    return True, "auto-merge (squash) enabled"


def dispatch_ci(gh: GitHub, branch: str, workflow: str = "ci.yml") -> None:
    gh.post(f"repos/{gh.repo}/actions/workflows/{workflow}/dispatches", {"ref": branch})


def wait_for_ci(gh: GitHub, branch: str, sha: str, timeout: int = 1_800,
                interval: int = 20, sleep: Callable[[float], None] = time.sleep,
                workflow: str = "ci.yml") -> str:
    """Conclusion of the CI run on branch@sha ('timeout' if it never finishes)."""
    waited = 0
    while waited <= timeout:
        runs = gh.get(f"repos/{gh.repo}/actions/workflows/{workflow}/runs?branch={branch}&per_page=10")
        for r in runs.get("workflow_runs", []):
            if r.get("head_sha") == sha and r.get("status") == "completed":
                return r.get("conclusion") or "unknown"
        sleep(interval)
        waited += interval
    return "timeout"


def git(repo_root: Path, *args: str, run: Runner = subprocess.run) -> str:
    res = run(["git", *args], cwd=repo_root, capture_output=True, text=True)
    if res.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)} failed: {res.stderr.strip()}")
    return res.stdout.strip()


def publish(gh: GitHub, ev: Event, suites: list[Suite], failure: Failure,
            result: HealResult, model: str, repo_root: Path,
            run: Runner = subprocess.run, sleep: Callable[[float], None] = time.sleep) -> dict:
    """Commit to autofix/<run-id>, push, open the PR, label it, arrange merging."""
    title = pr_title(ev, suites, result.passed)
    git(repo_root, "checkout", "-B", ev.branch, run=run)
    # Stage only what the kept patch touched (suites leave coverage files etc.).
    kept = next((a for a in reversed(result.attempts) if a.status in ("passed", "failed")), None)
    if kept and kept.files:
        git(repo_root, "add", "-A", "--", *kept.files, run=run)
    git(repo_root, "-c", "user.name=github-actions[bot]",
        "-c", "user.email=41898282+github-actions[bot]@users.noreply.github.com",
        "commit", "--allow-empty", "-m", title, "-m", f"Self-heal of {ev.run_url}", run=run)
    sha = git(repo_root, "rev-parse", "HEAD", run=run)
    git(repo_root, "push", "origin", f"HEAD:refs/heads/{ev.branch}", run=run)

    pr = gh.post(f"repos/{gh.repo}/pulls", {
        "title": title, "head": ev.branch, "base": ev.head_branch,
        "body": pr_body(ev, suites, failure, result, model), "draft": not result.passed,
    })
    label = LABEL_OK if result.passed else LABEL_FAILED
    ensure_label(gh, LABEL_OK, "1f883d", "Fix opened by the self-healing CI")
    ensure_label(gh, LABEL_FAILED, "d1242f", "Self-healing CI could not fix this")
    gh.post(f"repos/{gh.repo}/issues/{pr['number']}/labels", {"labels": [label]})
    outcome = {"pr": pr["html_url"], "number": pr["number"], "label": label, "merge": "n/a"}
    if not result.passed:
        return outcome

    # Commits pushed with GITHUB_TOKEN don't trigger CI; a dispatch does.
    try:
        dispatch_ci(gh, ev.branch)
    except GitHubError as e:
        outcome["merge"] = f"could not dispatch CI: {e}"
        return outcome
    ok, msg = enable_auto_merge(gh, pr)
    if ok:
        outcome["merge"] = msg
        return outcome
    # No required checks on the base branch ("clean status"), so GitHub won't
    # queue auto-merge. Do what auto-merge would: wait for CI, squash if green.
    conclusion = wait_for_ci(gh, ev.branch, sha, sleep=sleep)
    if conclusion == "success":
        gh.put(f"repos/{gh.repo}/pulls/{pr['number']}/merge",
               {"merge_method": "squash", "sha": sha, "commit_title": f"{title} (#{pr['number']})"})
        outcome["merge"] = f"squash-merged after CI passed (auto-merge unavailable: {msg})"
    else:
        outcome["merge"] = f"left open: CI {conclusion} (auto-merge unavailable: {msg})"
    return outcome


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def write_outputs(values: dict[str, str]) -> None:
    path = os.environ.get("GITHUB_OUTPUT")
    lines = [f"{k}={v}" for k, v in values.items()]
    print("\n".join(lines))
    if path:
        with open(path, "a", encoding="utf-8") as fh:
            fh.write("\n".join(lines) + "\n")


def write_summary(md: str) -> None:
    print(md)
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if path:
        with open(path, "a", encoding="utf-8") as fh:
            fh.write(md + "\n")


def plan(ev: Event, gh: GitHub) -> dict[str, str]:
    reason = trigger_skip_reason(ev)
    if reason is None and gh.branch_exists(ev.branch):
        reason = f"{ev.branch} already exists: this run was already healed"
    suites: list[Suite] = []
    if reason is None:
        for job in gh.failed_jobs(ev.run_id):
            s = suite_for_job(job.get("name", ""))
            if s and s not in suites and not (s.name == "test-first" and not ev.base_sha):
                suites.append(s)
        if not suites:
            reason = "no failing job maps to a healable suite"
    chains = {c for s in suites for c in s.toolchains}
    return {
        "skip": "true" if reason else "false",
        "reason": reason or "",
        "suites": ",".join(s.name for s in suites),
        "need_go": str("go" in chains).lower(),
        "need_python": str("python" in chains).lower(),
        "need_flutter": str("flutter" in chains).lower(),
    }


def fix(ev: Event, gh: GitHub, suite_names: list[str], models: str | list[str],
        repo_root: Path = REPO_ROOT, completion: Callable | None = None) -> int:
    llm = LLM([models] if isinstance(models, str) else list(models), completion)
    suites = [SUITES[n] for n in suite_names if n in SUITES]
    names = {s.name for s in suites}
    logs = [gh.job_log(j["id"]) for j in gh.failed_jobs(ev.run_id)
            if (s := suite_for_job(j.get("name", ""))) and s.name in names]
    failure = merge_failures([parse_failures(t) for t in logs])
    result = heal(suites, failure, repo_root, tracked_files(repo_root),
                  llm, ev.base_sha)
    if not result.reproduced:
        write_summary(f"### Self-heal: nothing to do\nSuites `{','.join(names)}` pass in the "
                      f"sandbox at `{ev.head_sha[:7]}`; the failure did not reproduce (flaky?). "
                      "No LLM call made.")
        return 0
    model = llm.used or ", ".join(llm.models) or "none available"
    outcome = publish(gh, ev, suites, failure, result, model, repo_root)
    write_summary(
        f"### Self-heal for run [{ev.run_id}]({ev.run_url})\n"
        f"- Result: {'green' if result.passed else 'red'} after {len(result.attempts)} attempt(s)\n"
        f"- PR: {outcome['pr']} (label `{outcome['label']}`)\n- Merge: {outcome['merge']}")
    write_outputs({"pr_url": outcome["pr"], "passed": str(result.passed).lower()})
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="HexaGo self-healing CI")
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("plan")
    p_fix = sub.add_parser("fix")
    p_fix.add_argument("--suites", required=True)
    args = ap.parse_args(argv)
    if hasattr(sys.stdout, "reconfigure"):  # emoji on Windows consoles
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")

    ev = Event.from_env()
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN", "")
    gh = GitHub(ev.repo, token)
    if args.cmd == "plan":
        out = plan(ev, gh)
        write_outputs(out)
        if out["skip"] == "true":
            write_summary(f"### Self-heal skipped\n{out['reason']}")
        return 0
    models = models_from_env()
    if not os.environ.get("GEMINI_API_KEY") and all(m.startswith("gemini/") for m in models):
        write_summary("### Self-heal skipped\nGEMINI_API_KEY secret is not set.")
        return 1
    return fix(ev, gh, [s for s in args.suites.split(",") if s], models)


if __name__ == "__main__":
    sys.exit(main())
