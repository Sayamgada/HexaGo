import json
import sys
from pathlib import Path

import pytest

import self_heal as sh
from conftest import commit_all, git

# ---------------------------------------------------------------- fixtures

GO_LOG = """\
2026-11-17T10:00:00.1234567Z ##[group]Run go test -race ./...
2026-11-17T10:00:05.0000000Z === RUN   TestKRingMax
2026-11-17T10:00:05.0000000Z     geo_test.go:142: Rings(origin, 3) returned 4 rings, want 3
2026-11-17T10:00:05.0000000Z --- FAIL: TestKRingMax (0.00s)
2026-11-17T10:00:05.0000000Z FAIL
2026-11-17T10:00:05.0000000Z FAIL\tgithub.com/hexago/hexago/services/dispatch-go/internal/geo\t0.012s
2026-11-17T10:00:06.0000000Z ok  \tgithub.com/hexago/hexago/services/dispatch-go/internal/fleet\t1.2s
2026-11-17T10:00:07.0000000Z ##[error]Process completed with exit code 1.
"""

GO_PANIC_LOG = """\
panic: runtime error: index out of range [4] with length 4
goroutine 7 [running]:
github.com/hexago/hexago/services/dispatch-go/internal/geo.(*HexGrid).Rings(...)
\t/home/runner/work/HexaGo/HexaGo/services/dispatch-go/internal/geo/hexgrid.go:88 +0x1d
--- FAIL: TestRings (0.00s)
"""

PY_LOG = """\
tests/test_cors.py::test_allows_localhost FAILED
=================================== FAILURES ===================================
    def test_allows_localhost():
>       assert allowed("http://localhost:3000")
E       AssertionError: assert False
tests/test_cors.py:12: AssertionError
  File "/home/runner/work/HexaGo/HexaGo/services/agents-py/src/agents_py/cors.py", line 21, in allowed
=========================== short test summary info ============================
FAILED tests/test_cors.py::test_allows_localhost - AssertionError: assert False
"""

DART_LOG = """\
00:03 +12 -1: OpsCommandCenter shows metrics [E]
  Expected: exactly one matching candidate
  package:hexago_app/features/ops/widgets/metrics_strip.dart 44:7  MetricsStrip.build
  test/ops/metrics_strip_test.dart 30:5  main.<fn>
"""


class FakeGitHub(sh.GitHub):
    """In-memory GitHub API: records writes, serves canned reads."""

    def __init__(self, jobs=None, logs=None, branches=(), automerge_errors=None,
                 runs=None):
        super().__init__("acme/hexago", "token")
        self.jobs = jobs or []
        self.logs = logs or {}
        self.branches = set(branches)
        self.automerge_errors = automerge_errors
        self.runs = runs or []
        self.calls = []

    def get(self, path):
        self.calls.append(("GET", path, None))
        if "/branches/" in path:
            if path.rsplit("/branches/", 1)[1] in self.branches:
                return {"name": "x"}
            raise sh.GitHubError(404, "Branch not found")
        if path.endswith("/jobs?per_page=100&filter=latest"):
            return {"jobs": self.jobs}
        if "/runs?branch=" in path:
            return {"workflow_runs": self.runs}
        raise AssertionError(f"unexpected GET {path}")

    def get_text(self, path):
        self.calls.append(("GET_TEXT", path, None))
        return self.logs[int(path.split("/jobs/")[1].split("/")[0])]

    def post(self, path, body):
        self.calls.append(("POST", path, body))
        if path.endswith("/pulls"):
            return {"number": 7, "html_url": "https://github.com/acme/hexago/pull/7",
                    "node_id": "PR_node"}
        if path.endswith("/graphql"):
            if self.automerge_errors:
                return {"errors": [{"message": self.automerge_errors}]}
            return {"data": {"enablePullRequestAutoMerge": {"pullRequest": {"number": 7}}}}
        return {}

    def put(self, path, body):
        self.calls.append(("PUT", path, body))
        return {"merged": True}

    def posted(self, suffix):
        return [b for m, p, b in self.calls if m == "POST" and p.endswith(suffix)]


def event(**kw):
    base = dict(repo="acme/hexago", run_id="555", head_branch="demo/bug",
                head_sha="abc1234def", head_commit_message="test: bug", head_repo="acme/hexago")
    base.update(kw)
    return sh.Event(**base)


# ---------------------------------------------------------------- guardrails: trigger

@pytest.mark.parametrize("kw, fragment", [
    ({"conclusion": "success"}, "not failure"),
    ({"head_branch": "autofix/123"}, "autofix branch"),
    ({"head_commit_message": "fix(autofix): repair go failure [ai-autofix run 9] (#3)"}, "autofix merge"),
    ({"head_repo": "evil/fork"}, "fork"),
    ({"run_attempt": 2}, "re-run"),
    ({"run_id": ""}, "missing"),
])
def test_trigger_skip_reasons(kw, fragment):
    assert fragment in sh.trigger_skip_reason(event(**kw))


def test_trigger_proceeds_for_normal_failure():
    assert sh.trigger_skip_reason(event()) is None


def test_event_from_env():
    ev = sh.Event.from_env({"GITHUB_REPOSITORY": "acme/hexago", "RUN_ID": "42",
                            "RUN_ATTEMPT": "1", "HEAD_BRANCH": "main", "PR_BASE_SHA": "b"})
    assert ev.branch == "autofix/42"
    assert ev.run_url == "https://github.com/acme/hexago/actions/runs/42"
    assert ev.head_repo == "acme/hexago" and ev.base_sha == "b"


def test_suite_for_job():
    assert sh.suite_for_job("go").name == "go"
    assert sh.suite_for_job("Python (agents-py)").name == "python"
    assert sh.suite_for_job("coverage-gate").name == "coverage-gate"
    assert sh.suite_for_job("autocoding-stats") is None


# ---------------------------------------------------------------- plan

def test_plan_selects_failing_suites_and_toolchains():
    gh = FakeGitHub(jobs=[{"id": 1, "name": "go", "conclusion": "failure"},
                          {"id": 2, "name": "coverage-gate", "conclusion": "failure"},
                          {"id": 3, "name": "autocoding-stats", "conclusion": "failure"}])
    out = sh.plan(event(), gh)
    assert out["skip"] == "false"
    assert out["suites"] == "go,coverage-gate"
    assert out["need_go"] == "true" and out["need_python"] == "true"
    assert out["need_flutter"] == "false"


def test_plan_skips_when_already_healed():
    gh = FakeGitHub(branches={"autofix/555"})
    out = sh.plan(event(), gh)
    assert out["skip"] == "true" and "already" in out["reason"]


def test_plan_skips_without_healable_suite_or_pr_base():
    gh = FakeGitHub(jobs=[{"id": 1, "name": "test-first", "conclusion": "failure"}])
    out = sh.plan(event(base_sha=""), gh)
    assert out["skip"] == "true" and "healable" in out["reason"]
    assert sh.plan(event(base_sha="b"), gh)["suites"] == "test-first"


def test_plan_skips_on_guardrail_without_calling_api():
    gh = FakeGitHub()
    assert sh.plan(event(head_branch="autofix/1"), gh)["skip"] == "true"
    assert gh.calls == []


# ---------------------------------------------------------------- log parsing

def test_parse_go_failure():
    f = sh.parse_failures(GO_LOG)
    assert f.tests == ["TestKRingMax"]
    assert f.go_packages == ["github.com/hexago/hexago/services/dispatch-go/internal/geo"]
    assert ("geo_test.go", 142) in f.file_refs
    assert "--- FAIL: TestKRingMax" in f.excerpt
    assert "2026-11-17T" not in f.excerpt  # timestamps stripped


def test_parse_go_panic_stack():
    f = sh.parse_failures(GO_PANIC_LOG)
    assert f.tests == ["TestRings"]
    assert ("/home/runner/work/HexaGo/HexaGo/services/dispatch-go/internal/geo/hexgrid.go", 88) in f.file_refs


def test_parse_pytest_failure():
    f = sh.parse_failures(PY_LOG)
    assert f.tests == ["tests/test_cors.py::test_allows_localhost"]
    assert ("tests/test_cors.py", 12) in f.file_refs
    assert ("/home/runner/work/HexaGo/HexaGo/services/agents-py/src/agents_py/cors.py", 21) in f.file_refs


def test_parse_flutter_failure():
    f = sh.parse_failures(DART_LOG)
    assert f.tests == ["OpsCommandCenter shows metrics"]
    assert ("lib/features/ops/widgets/metrics_strip.dart", 44) in f.file_refs
    assert ("test/ops/metrics_strip_test.dart", 30) in f.file_refs


def test_excerpt_keeps_windows_and_caps():
    lines = [f"noise {i}" for i in range(500)] + ["--- FAIL: TestX"] + [f"tail {i}" for i in range(500)]
    text = sh.excerpt(lines, radius=2)
    assert "--- FAIL: TestX" in text and "noise 498" in text and "noise 10" not in text
    assert len(sh.excerpt(["x" * 50] * 1000, cap=500)) <= 500
    assert sh.excerpt(["--- FAIL: A"] * 2000, cap=300).startswith("...")


def test_merge_failures_dedupes():
    merged = sh.merge_failures([sh.parse_failures(GO_LOG), sh.parse_failures(GO_LOG)])
    assert merged.tests == ["TestKRingMax"]
    assert len(merged.go_packages) == 1


# ---------------------------------------------------------------- path resolution + context

TRACKED = [
    "services/dispatch-go/internal/geo/geo_test.go",
    "services/dispatch-go/internal/geo/hexgrid.go",
    "services/dispatch-go/internal/sim/geo_test.go",
    "services/agents-py/src/agents_py/cors.py",
    "services/agents-py/tests/test_cors.py",
    "apps/hexago_app/lib/features/ops/widgets/metrics_strip.dart",
]


def test_resolve_ref_absolute_and_relative():
    assert sh.resolve_ref("/home/runner/work/HexaGo/HexaGo/services/dispatch-go/internal/geo/hexgrid.go",
                          TRACKED, []) == "services/dispatch-go/internal/geo/hexgrid.go"
    assert sh.resolve_ref("tests/test_cors.py", TRACKED, []) == "services/agents-py/tests/test_cors.py"
    assert sh.resolve_ref("./lib/features/ops/widgets/metrics_strip.dart", TRACKED, []) \
        == "apps/hexago_app/lib/features/ops/widgets/metrics_strip.dart"


def test_resolve_ref_ambiguous_basename_uses_preference():
    assert sh.resolve_ref("geo_test.go", TRACKED, []) is None
    assert sh.resolve_ref("geo_test.go", TRACKED, ["services/dispatch-go/internal/geo"]) \
        == "services/dispatch-go/internal/geo/geo_test.go"
    assert sh.resolve_ref("nope.go", TRACKED, []) is None


def test_is_test_file():
    assert sh.is_test_file("a/b/x_test.go")
    assert sh.is_test_file("services/agents-py/tests/helpers.py")
    assert sh.is_test_file("apps/hexago_app/test/ops/fakes.dart")
    assert not sh.is_test_file("services/dispatch-go/internal/geo/geo.go")


def _write(root: Path, rel: str, text: str) -> None:
    p = root / rel
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(text, encoding="utf-8", newline="")


def test_collect_context_prioritises_refs_and_caps(tmp_path):
    _write(tmp_path, "services/dispatch-go/go.mod", "module github.com/hexago/hexago/services/dispatch-go\n")
    _write(tmp_path, "services/dispatch-go/internal/geo/geo_test.go", "package geo // test\n")
    _write(tmp_path, "services/dispatch-go/internal/geo/hexgrid.go", "package geo // grid\n")
    _write(tmp_path, "services/dispatch-go/internal/geo/big.go", "x" * 10_000)
    _write(tmp_path, ".github/workflows/ci.yml", "secret")
    tracked = ["services/dispatch-go/go.mod", "services/dispatch-go/internal/geo/geo_test.go",
               "services/dispatch-go/internal/geo/hexgrid.go", "services/dispatch-go/internal/geo/big.go",
               ".github/workflows/ci.yml"]
    f = sh.parse_failures(GO_LOG)
    f.file_refs.append((".github/workflows/ci.yml", 1))
    ctx, included = sh.collect_context(f, [sh.SUITES["go"]], tmp_path, tracked)
    assert included[0] == "services/dispatch-go/internal/geo/geo_test.go"
    assert ".github/workflows/ci.yml" not in included
    assert "services/dispatch-go/internal/geo/big.go" in included

    ctx, included = sh.collect_context(f, [sh.SUITES["go"]], tmp_path, tracked, cap=4_000)
    assert len(ctx) <= 4_000 + 200
    assert "big.go (truncated)" in ctx or "big.go" not in ctx


def test_collect_context_stops_when_no_room(tmp_path):
    _write(tmp_path, "a/x.py", "y" * 5_000)
    _write(tmp_path, "a/z.py", "z" * 5_000)
    f = sh.Failure(file_refs=[("a/x.py", 1), ("a/z.py", 1)])
    ctx, included = sh.collect_context(f, [], tmp_path, ["a/x.py", "a/z.py"], cap=6_000)
    assert included == ["a/x.py"]


def test_go_package_dirs_and_module(tmp_path):
    assert sh.go_module(tmp_path) == ""
    assert sh.go_package_dirs(["github.com/m/internal/geo"], "github.com/m") == ["services/dispatch-go/internal/geo"]
    assert sh.go_package_dirs(["other/x"], "github.com/m") == []


# ---------------------------------------------------------------- LLM response parsing

GOOD_DIFF = """\
--- a/calc.py
+++ b/calc.py
@@ -1,2 +1,2 @@
 def add(a, b):
-    return a - b
+    return a + b
"""


def test_parse_llm_response_fenced():
    reply = f"EXPLANATION: add() subtracted instead of adding.\n\n```diff\n{GOOD_DIFF}```\n"
    diff, why = sh.parse_llm_response(reply)
    assert diff == GOOD_DIFF
    assert why == "add() subtracted instead of adding."


def test_parse_llm_response_unfenced_and_missing():
    diff, why = sh.parse_llm_response("Here you go\n" + GOOD_DIFF.rstrip("\n"))
    assert diff.startswith("--- a/calc.py") and diff.endswith("\n")
    assert sh.parse_llm_response("I cannot help")[0] == ""


def test_parse_llm_response_skips_non_diff_fence():
    reply = "EXPLANATION: x\n```\nsome log\n```\n```diff\n" + GOOD_DIFF + "```"
    assert sh.parse_llm_response(reply)[0] == GOOD_DIFF


def test_call_llm_uses_completion_and_dict_fallback():
    class Msg:
        content = "hi"

    class Choice:
        message = Msg()

    class Resp:
        choices = [Choice()]

    seen = {}

    def completion(**kw):
        seen.update(kw)
        return Resp()

    assert sh.call_llm([{"role": "user", "content": "x"}], "gemini/x", completion) == "hi"
    assert seen["model"] == "gemini/x" and seen["temperature"] == 0
    assert sh.call_llm([], "m", lambda **kw: {"choices": [{"message": {"content": "d"}}]}) == "d"


def test_build_messages_includes_history_and_hints():
    f = sh.Failure(tests=["TestA"], excerpt="--- FAIL: TestA")
    hist = [sh.Attempt(1, "failed", "x", GOOD_DIFF, ["calc.py"], "still broken")]
    msgs = sh.build_messages([sh.SUITES["coverage-gate"]], f, "before", "=== calc.py ===", hist)
    assert msgs[0]["role"] == "system" and ".github/" in msgs[0]["content"]
    user = msgs[1]["content"]
    for needle in ("TestA", "Attempt 1 (failed)", "still broken", "ORIGINAL", "add or extend tests"):
        assert needle in user


# ---------------------------------------------------------------- patch guardrails

def _diff_for(path, minus="a", plus="b"):
    return f"--- a/{path}\n+++ b/{path}\n@@ -1 +1 @@\n-{minus}\n+{plus}\n"


def test_validate_accepts_good_patch():
    assert sh.validate_patch(GOOD_DIFF) == []


def test_validate_rejects_github_paths():
    errs = sh.validate_patch(_diff_for(".github/workflows/ci.yml"))
    assert any("protected" in e for e in errs)


def test_validate_rejects_unsafe_paths():
    assert sh.validate_patch(_diff_for("../outside.py"))
    assert sh.validate_patch("--- /etc/passwd\n+++ /etc/passwd\n@@ -1 +1 @@\n-a\n+b\n")


def test_validate_rejects_too_many_files():
    diff = "".join(_diff_for(f"f{i}.py") for i in range(6))
    assert any("6 files" in e for e in sh.validate_patch(diff))
    assert sh.validate_patch("".join(_diff_for(f"f{i}.py") for i in range(5))) == []


def test_validate_rejects_deleting_test_file():
    diff = "--- a/services/x/foo_test.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-package x\n"
    assert any("deletes test" in e for e in sh.validate_patch(diff))
    ok = "--- a/services/x/old.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-package x\n"
    assert sh.validate_patch(ok) == []


@pytest.mark.parametrize("removed", [
    "func TestKRing(t *testing.T) {",
    "def test_allows_localhost():",
    "  test('shows metrics', () {",
    "async def test_stream():",
])
def test_validate_rejects_removing_test_functions(removed):
    diff = f"--- a/t_test.go\n+++ b/t_test.go\n@@ -1,2 +1 @@\n-{removed}\n keep\n"
    assert any("removes tests" in e for e in sh.validate_patch(diff))


def test_validate_allows_editing_a_test_body():
    diff = ("--- a/x_test.go\n+++ b/x_test.go\n@@ -1,2 +1,2 @@\n"
            "-func TestA(t *testing.T) { old }\n+func TestA(t *testing.T) { new }\n")
    assert sh.validate_patch(diff) == []


@pytest.mark.parametrize("added", ["\tt.Skip(\"flaky\")", "@pytest.mark.skip", "  }, skip: true);"])
def test_validate_rejects_adding_skips(added):
    diff = f"--- a/x_test.go\n+++ b/x_test.go\n@@ -1 +1,2 @@\n keep\n+{added}\n"
    assert any("skip" in e for e in sh.validate_patch(diff))


def test_validate_rejects_empty():
    assert sh.validate_patch("") == ["no unified diff found in the reply"]
    assert "diff names no files" in sh.validate_patch("@@ -1 +1 @@\n-a\n+b\n")


def test_patch_files_tracks_created_and_deleted():
    diff = ("--- /dev/null\n+++ b/new.py\n@@ -0,0 +1 @@\n+x\n"
            "--- a/gone.py\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n")
    info = sh.patch_files(diff)
    assert info.created == ["new.py"] and info.deleted == ["gone.py"]
    assert info.files == ["new.py", "gone.py"]


# ---------------------------------------------------------------- patch application (real git)

def test_apply_patch_with_git(repo):
    (repo / "calc.py").write_text("def add(a, b):\n    return a - b\n", encoding="utf-8", newline="")
    commit_all(repo)
    ok, err = sh.apply_patch(GOOD_DIFF, repo)
    assert ok, err
    assert "a + b" in (repo / "calc.py").read_text(encoding="utf-8")


def test_apply_patch_recounts_bad_hunk_header(repo):
    (repo / "calc.py").write_text("def add(a, b):\n    return a - b\n", encoding="utf-8", newline="")
    commit_all(repo)
    bad = GOOD_DIFF.replace("@@ -1,2 +1,2 @@", "@@ -1,7 +1,9 @@")
    ok, err = sh.apply_patch(bad, repo)
    assert ok, err


def test_apply_patch_reports_mismatch(repo):
    (repo / "calc.py").write_text("something else\n", encoding="utf-8", newline="")
    commit_all(repo)
    ok, err = sh.apply_patch(GOOD_DIFF, repo)
    assert not ok and "git apply" in err


def test_apply_patch_missing_binary(tmp_path):
    def run(*a, **kw):
        raise FileNotFoundError("git not found")
    ok, err = sh.apply_patch(GOOD_DIFF, tmp_path, run=run)
    assert not ok and "not found" in err


# ---------------------------------------------------------------- healing loop (real git + suite, mocked LLM)

CHECK = "import sys, calc\nsys.exit(0 if calc.add(2, 3) == 5 else 1)\n"
DEMO_SUITE = sh.Suite("demo", ".", (f'"{sys.executable}" check.py',), ())


@pytest.fixture
def broken_repo(repo):
    (repo / "calc.py").write_text("def add(a, b):\n    return a - b\n", encoding="utf-8", newline="")
    (repo / "check.py").write_text(CHECK, encoding="utf-8", newline="")
    commit_all(repo)
    return repo


def scripted_llm(*replies):
    calls = []

    def llm(messages):
        calls.append(messages)
        return replies[len(calls) - 1]
    llm.calls = calls
    return llm


def test_heal_succeeds_on_second_attempt_with_feedback(broken_repo):
    wrong = GOOD_DIFF.replace("a + b", "a * b")
    llm = scripted_llm(f"EXPLANATION: try multiply\n```diff\n{wrong}```",
                       f"EXPLANATION: use plus\n```diff\n{GOOD_DIFF}```")
    failure = sh.Failure(tests=["check"], file_refs=[("calc.py", 2)], excerpt="exit 1")
    res = sh.heal([DEMO_SUITE], failure, broken_repo, ["calc.py", "check.py"], llm)
    assert res.passed and res.llm_calls == 2
    assert [a.status for a in res.attempts] == ["failed", "passed"]
    assert "Attempt 1 (failed)" in llm.calls[1][1]["content"]   # failure fed back
    assert "=== calc.py ===" in llm.calls[0][1]["content"]       # context sent
    assert "a + b" in (broken_repo / "calc.py").read_text(encoding="utf-8")
    assert "[exit 1]" in res.before_output and "[exit 0]" in res.after_output


def test_heal_gives_up_after_three_and_keeps_last_applied_patch(broken_repo):
    wrong = GOOD_DIFF.replace("a + b", "a * b")
    llm = scripted_llm(f"```diff\n{wrong}```",
                       "```diff\n" + _diff_for(".github/workflows/ci.yml") + "```",
                       "no diff here")
    res = sh.heal([DEMO_SUITE], sh.Failure(), broken_repo, ["calc.py"], llm)
    assert not res.passed and res.llm_calls == 3
    assert [a.status for a in res.attempts] == ["failed", "rejected", "no-diff"]
    assert "a * b" in (broken_repo / "calc.py").read_text(encoding="utf-8")


def test_heal_records_apply_failure(broken_repo):
    bad = GOOD_DIFF.replace("return a - b", "return nothing")
    llm = scripted_llm(f"```diff\n{bad}```", f"```diff\n{GOOD_DIFF}```")
    res = sh.heal([DEMO_SUITE], sh.Failure(), broken_repo, ["calc.py"], llm)
    assert [a.status for a in res.attempts] == ["apply-failed", "passed"]
    assert "did not apply" in res.attempts[0].output


def test_heal_skips_llm_when_failure_does_not_reproduce(broken_repo):
    (broken_repo / "calc.py").write_text("def add(a, b):\n    return a + b\n", encoding="utf-8", newline="")
    llm = scripted_llm()
    res = sh.heal([DEMO_SUITE], sh.Failure(), broken_repo, [], llm)
    assert res.passed and not res.reproduced and res.llm_calls == 0


def test_run_suites_formats_base_and_handles_timeout(tmp_path):
    seen = []

    def run(cmd, **kw):
        seen.append(cmd)
        raise sh.subprocess.TimeoutExpired(cmd, 1)
    ok, out = sh.run_suites([sh.SUITES["test-first"]], tmp_path, "deadbeef", run=run)
    assert not ok and "timed out" in out and "--base deadbeef" in seen[0]


# ---------------------------------------------------------------- publishing (real git, fake GitHub)

def _result(passed=True):
    att = sh.Attempt(1, "passed" if passed else "failed", "Fixed add().", GOOD_DIFF, ["calc.py"], "ok")
    return sh.HealResult(passed, [att], "before: FAIL", "after: ok", 1)


def test_publish_green_opens_labelled_pr_with_auto_merge(broken_repo):
    sh.apply_patch(GOOD_DIFF, broken_repo)
    (broken_repo / "coverage.xml").write_text("<junk/>", encoding="utf-8", newline="")  # must not be committed
    gh = FakeGitHub()
    ev = event(run_id="555", head_branch="main")
    out = sh.publish(gh, ev, [DEMO_SUITE], sh.Failure(tests=["TestA"]), _result(), "gemini/x", broken_repo)

    assert out == {"pr": "https://github.com/acme/hexago/pull/7", "number": 7,
                   "label": "ai-autofix", "merge": "auto-merge (squash) enabled"}
    pr = gh.posted("/pulls")[0]
    assert pr["head"] == "autofix/555" and pr["base"] == "main" and pr["draft"] is False
    assert "[ai-autofix run 555]" in pr["title"]
    for needle in ("Fixed add().", "### Attempts", "Test output before", "before: FAIL", "after: ok"):
        assert needle in pr["body"]
    assert gh.posted("/issues/7/labels") == [{"labels": ["ai-autofix"]}]
    assert gh.posted("/actions/workflows/ci.yml/dispatches") == [{"ref": "autofix/555"}]
    files = git(broken_repo, "show", "--name-only", "--format=", "origin/autofix/555").split()
    assert files == ["calc.py"]


def test_publish_red_opens_draft_failed_pr(broken_repo):
    gh = FakeGitHub()
    out = sh.publish(gh, event(), [DEMO_SUITE], sh.Failure(), _result(passed=False), "m", broken_repo)
    assert out["label"] == "ai-autofix-failed"
    assert gh.posted("/pulls")[0]["draft"] is True
    assert gh.posted("/pulls")[0]["base"] == "demo/bug"
    assert gh.posted("/dispatches") == [] and gh.posted("/graphql") == []


def test_publish_falls_back_to_merge_after_ci_when_no_branch_protection(broken_repo):
    sh.apply_patch(GOOD_DIFF, broken_repo)
    gh = FakeGitHub(automerge_errors="Pull request is in clean status")
    sha_holder = {}

    def sleep(_):
        sha = git(broken_repo, "rev-parse", "HEAD")
        sha_holder["sha"] = sha
        gh.runs = [{"head_sha": sha, "status": "completed", "conclusion": "success"}]
    out = sh.publish(gh, event(), [DEMO_SUITE], sh.Failure(), _result(), "m", broken_repo, sleep=sleep)
    assert out["merge"].startswith("squash-merged after CI passed")
    merges = [b for m, p, b in gh.calls if m == "PUT" and p.endswith("/pulls/7/merge")]
    assert merges[0]["merge_method"] == "squash" and merges[0]["sha"] == sha_holder["sha"]


def test_publish_leaves_pr_open_when_ci_fails(broken_repo):
    sh.apply_patch(GOOD_DIFF, broken_repo)
    gh = FakeGitHub(automerge_errors="clean status")

    def sleep(_):
        gh.runs = [{"head_sha": git(broken_repo, "rev-parse", "HEAD"),
                    "status": "completed", "conclusion": "failure"}]
    out = sh.publish(gh, event(), [DEMO_SUITE], sh.Failure(), _result(), "m", broken_repo, sleep=sleep)
    assert out["merge"].startswith("left open: CI failure")
    assert not [c for c in gh.calls if c[0] == "PUT"]


def test_wait_for_ci_times_out():
    gh = FakeGitHub()
    assert sh.wait_for_ci(gh, "b", "sha", timeout=40, interval=20, sleep=lambda _: None) == "timeout"


def test_ensure_label_ignores_existing_but_raises_others():
    class G(FakeGitHub):
        def __init__(self, status):
            super().__init__()
            self.status = status

        def post(self, path, body):
            raise sh.GitHubError(self.status, "x")
    sh.ensure_label(G(422), "a", "fff", "d")
    with pytest.raises(sh.GitHubError):
        sh.ensure_label(G(500), "a", "fff", "d")


def test_enable_auto_merge_reports_api_error():
    class G(FakeGitHub):
        def graphql(self, q, v):
            raise sh.GitHubError(403, "forbidden")
    ok, msg = sh.enable_auto_merge(G(), {"node_id": "x"})
    assert not ok and "403" in msg


def test_pr_body_is_capped():
    res = _result()
    res.before_output = "x" * 100_000
    body = sh.pr_body(event(), [DEMO_SUITE], sh.Failure(), res, "m", cap=5_000)
    assert len(body) <= 5_000 and body.endswith("_[truncated]_")


# ---------------------------------------------------------------- fix() end to end + CLI

def test_fix_end_to_end_with_mocks(broken_repo, monkeypatch):
    monkeypatch.setattr(sh, "SUITES", {**sh.SUITES, "demo": DEMO_SUITE})
    gh = FakeGitHub(jobs=[{"id": 9, "name": "demo", "conclusion": "failure"}],
                    logs={9: "FAILED check.py - assert\n  File \"calc.py\", line 2, in add\n"})

    class Msg:
        content = f"EXPLANATION: plus\n```diff\n{GOOD_DIFF}```"

    class Resp:
        choices = [type("C", (), {"message": Msg()})()]

    rc = sh.fix(event(), gh, ["demo"], "gemini/x", repo_root=broken_repo,
                completion=lambda **kw: Resp())
    assert rc == 0
    assert gh.posted("/pulls")[0]["draft"] is False
    assert ("GET_TEXT", "repos/acme/hexago/actions/jobs/9/logs", None) in gh.calls


def test_fix_not_reproduced_opens_nothing(broken_repo, monkeypatch):
    (broken_repo / "calc.py").write_text("def add(a, b):\n    return a + b\n", encoding="utf-8", newline="")
    commit_all(broken_repo, "fixed")
    monkeypatch.setattr(sh, "SUITES", {**sh.SUITES, "demo": DEMO_SUITE})
    gh = FakeGitHub(jobs=[{"id": 9, "name": "demo", "conclusion": "failure"}], logs={9: "flake"})
    assert sh.fix(event(), gh, ["demo"], "m", repo_root=broken_repo, completion=None) == 0
    assert gh.posted("/pulls") == []


def test_main_plan_writes_outputs(tmp_path, monkeypatch):
    out = tmp_path / "out.txt"
    summary = tmp_path / "summary.md"
    monkeypatch.setenv("GITHUB_OUTPUT", str(out))
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))
    monkeypatch.setenv("GITHUB_REPOSITORY", "acme/hexago")
    monkeypatch.setenv("RUN_ID", "1")
    monkeypatch.setenv("HEAD_BRANCH", "autofix/0")
    assert sh.main(["plan"]) == 0
    assert "skip=true" in out.read_text(encoding="utf-8")
    assert "autofix branch" in summary.read_text(encoding="utf-8")


def test_main_fix_requires_gemini_key(monkeypatch, tmp_path):
    monkeypatch.delenv("GEMINI_API_KEY", raising=False)
    monkeypatch.delenv("LLM_MODEL", raising=False)
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(tmp_path / "s.md"))
    assert sh.main(["fix", "--suites", "go"]) == 1


def test_github_client_get_text_follows_redirect_without_auth(monkeypatch):
    gh = sh.GitHub("acme/hexago", "tok")
    monkeypatch.setattr(gh, "_request", lambda *a, **kw: (302, b"", {"Location": "https://blob/x"}))
    opened = {}

    class Resp:
        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def read(self):
            return b"log text"

    def urlopen(url, timeout):
        opened["url"] = url
        return Resp()
    monkeypatch.setattr(sh.urllib.request, "urlopen", urlopen)
    assert gh.get_text("repos/acme/hexago/actions/jobs/1/logs") == "log text"
    assert opened["url"] == "https://blob/x"


def test_github_client_json_helpers(monkeypatch):
    gh = sh.GitHub("acme/hexago", "tok")
    sent = []

    def fake(method, url, body=None, follow=True):
        sent.append((method, url, body))
        if "branches/missing" in url:
            raise sh.GitHubError(404, "nf")
        if "branches/boom" in url:
            raise sh.GitHubError(500, "boom")
        return 200, json.dumps({"ok": True, "jobs": [{"conclusion": "failure", "id": 1},
                                                     {"conclusion": "success", "id": 2}]}).encode(), {}
    monkeypatch.setattr(gh, "_request", fake)
    assert gh.get("repos/x")["ok"]
    assert gh.post("repos/x", {"a": 1})["ok"] and gh.put("repos/x", {})["ok"]
    assert gh.graphql("q", {})["ok"]
    assert sent[3][1] == "https://api.github.com/graphql"
    assert gh.branch_exists("present") and not gh.branch_exists("missing")
    with pytest.raises(sh.GitHubError):
        gh.branch_exists("boom")
    assert [j["id"] for j in gh.failed_jobs("1")] == [1]
    sh.dispatch_ci(gh, "autofix/1")
    assert sent[-1] == ("POST", "https://api.github.com/repos/acme/hexago/actions/workflows/ci.yml/dispatches",
                        {"ref": "autofix/1"})


# ---------------------------------------------------------------- model fallback + LLM errors

class NotFoundError(Exception):
    pass


def _reply(text):
    return {"choices": [{"message": {"content": text}}]}


def test_llm_falls_back_and_drops_retired_model():
    seen = []

    def completion(model, **kw):
        seen.append(model)
        if model == "gemini/old":
            raise NotFoundError("This model models/old is no longer available to new users")
        return _reply("ok from " + model)

    llm = sh.LLM(["gemini/old", "gemini/new", "gemini/new"], completion)
    assert llm([]) == "ok from gemini/new" and llm.used == "gemini/new"
    assert llm([]) == "ok from gemini/new"
    assert seen == ["gemini/old", "gemini/new", "gemini/new"]  # retired model not retried
    assert llm.models == ["gemini/new"] and "no longer available" in llm.errors[0]


def test_llm_keeps_model_after_transient_error_and_raises_when_all_fail():
    def completion(model, **kw):
        raise TimeoutError("read timed out")

    llm = sh.LLM(["a", "b"], completion, sleep=lambda s: None, delays=(1,))
    with pytest.raises(RuntimeError, match="every model failed: a: TimeoutError"):
        llm([])
    assert llm.models == ["a", "b"]
    with pytest.raises(RuntimeError, match="no models configured"):
        sh.LLM([], completion)([])


def test_models_from_env():
    assert sh.models_from_env({}) == sh._dedupe([sh.DEFAULT_MODEL, *sh.DEFAULT_FALLBACK_MODELS])
    assert sh.models_from_env({"LLM_MODEL": "gemini/a", "LLM_FALLBACK_MODELS": " gemini/b, ,gemini/a "})         == ["gemini/a", "gemini/b"]
    assert sh.models_from_env({"LLM_MODEL": "gemini/a"}) == ["gemini/a", *sh.DEFAULT_FALLBACK_MODELS]


def test_heal_counts_llm_errors_as_attempts(broken_repo):
    replies = iter([RuntimeError("every model failed: 404"), f"```diff\n{GOOD_DIFF}```"])

    def llm(messages):
        r = next(replies)
        if isinstance(r, Exception):
            raise r
        return r
    res = sh.heal([DEMO_SUITE], sh.Failure(), broken_repo, ["calc.py"], llm)
    assert res.passed and res.llm_calls == 2
    assert [a.status for a in res.attempts] == ["llm-error", "passed"]
    assert "404" in res.attempts[0].output


def test_fix_reports_model_that_answered(broken_repo, monkeypatch):
    monkeypatch.setattr(sh, "SUITES", {**sh.SUITES, "demo": DEMO_SUITE})
    gh = FakeGitHub(jobs=[{"id": 9, "name": "demo", "conclusion": "failure"}], logs={9: "FAILED"})

    def completion(model, **kw):
        if model == "gemini/retired":
            raise NotFoundError("404 NOT_FOUND")
        return _reply(f"EXPLANATION: plus\n```diff\n{GOOD_DIFF}```")
    assert sh.fix(event(), gh, ["demo"], ["gemini/retired", "gemini/live"], repo_root=broken_repo,
                  completion=completion) == 0
    assert "**Model:** `gemini/live`" in gh.posted("/pulls")[0]["body"]


def test_repo_root_env_override(tmp_path, monkeypatch):
    import importlib
    monkeypatch.setenv("HEXAGO_REPO_ROOT", str(tmp_path))
    try:
        assert importlib.reload(sh).REPO_ROOT == tmp_path.resolve()
    finally:
        monkeypatch.delenv("HEXAGO_REPO_ROOT")
        importlib.reload(sh)


class UnsupportedParamsError(Exception):
    pass


def test_call_llm_drops_params_and_retries_without_temperature():
    calls = []

    def completion(**kw):
        calls.append(kw)
        if "temperature" in kw:
            raise UnsupportedParamsError("gemini does not support parameters: ['temperature']")
        return _reply("ok")
    assert sh.call_llm([], "gemini/gemini-3.8-flash", completion) == "ok"
    assert calls[0]["drop_params"] is True and calls[0]["temperature"] == 0
    assert "temperature" not in calls[1] and calls[1]["drop_params"] is True


def test_call_llm_reraises_other_errors_without_retry():
    calls = []

    def completion(**kw):
        calls.append(kw)
        raise TimeoutError("slow")
    with pytest.raises(TimeoutError):
        sh.call_llm([], "m", completion)
    assert len(calls) == 1


class ServiceUnavailableError(Exception):
    pass


def test_llm_retries_transient_errors_with_backoff():
    waits, calls = [], []

    def completion(model, **kw):
        calls.append(model)
        if len(calls) < 3:
            raise ServiceUnavailableError('{"code": 503, "message": "high demand", "status": "UNAVAILABLE"}')
        return _reply("ok")
    llm = sh.LLM(["gemini/x"], completion, sleep=waits.append, delays=(15, 30, 60))
    assert llm([]) == "ok" and waits == [15, 30] and llm.models == ["gemini/x"]


def test_llm_gives_up_after_retries_then_tries_next_model():
    waits = []

    def completion(model, **kw):
        if model == "busy":
            raise ServiceUnavailableError("503 UNAVAILABLE")
        return _reply("from " + model)
    llm = sh.LLM(["busy", "spare"], completion, sleep=waits.append, delays=(1, 2))
    assert llm([]) == "from spare" and waits == [1, 2]
    assert llm.models == ["busy", "spare"]  # busy is not retired


class RateLimitError(Exception):
    pass


def test_llm_drops_model_on_spent_quota_without_backoff(broken_repo):
    waits, calls = [], []

    def completion(model, **kw):
        calls.append(model)
        raise RateLimitError('{"code": 429, "message": "You exceeded your current quota, please check your plan"}')
    llm = sh.LLM(["gemini/x"], completion, sleep=waits.append, delays=(15, 30))
    with pytest.raises(RuntimeError, match="exceeded your current quota"):
        llm([])
    assert waits == [] and llm.models == [] and calls == ["gemini/x"]
    # The healing loop stops instead of burning the remaining attempts.
    res = sh.heal([DEMO_SUITE], sh.Failure(), broken_repo, ["calc.py"], llm)
    assert not res.passed and res.attempts == [] and calls == ["gemini/x"]


def test_plain_rate_limit_is_still_retried():
    waits = []
    replies = iter([RateLimitError("429 Too Many Requests, retry later"), None])

    def completion(model, **kw):
        r = next(replies)
        if r:
            raise r
        return _reply("ok")
    assert sh.LLM(["m"], completion, sleep=waits.append, delays=(5,))([]) == "ok" and waits == [5]
