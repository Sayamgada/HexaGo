import json
import subprocess

import autocoding_stats as st
from conftest import commit_all

LOG = """# Autonomous AI Coding Log

| Entry # | Date (UTC) | Prompt Summary | Agent / Tool | Files | Tests | Result |
|---|---|---|---|---|---|---|
| 1 | 2026-11-12 | scaffold | Antigravity | a | b | ok |
| 2 | 2026-11-13 | more | Claude Code | a | b | ok |
"""


def _w(repo, rel, text):
    p = repo / rel
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(text, encoding="utf-8", newline="")


def test_count_log_entries():
    assert st.count_log_entries(LOG) == 2
    assert st.count_log_entries("no table") == 0


def test_commit_counts_excludes_stats_commits(repo):
    _w(repo, "a.txt", "1")
    commit_all(repo, "feat: a")                            # not logged
    _w(repo, "docs/AUTOCODING_LOG.md", LOG)
    commit_all(repo, "feat: b")                            # logged
    _w(repo, "docs/AUTOCODING_STATS.md", "x")
    commit_all(repo, "chore(stats): update [skip ci]")     # excluded
    assert st.commit_counts(root=repo) == (2, 1)


def test_search_count_parses_and_tolerates_failure():
    ok = lambda args, **kw: subprocess.CompletedProcess(args, 0, "3\n", "")  # noqa: E731
    bad = lambda args, **kw: subprocess.CompletedProcess(args, 1, "", "HTTP 401")  # noqa: E731

    def missing(args, **kw):
        raise FileNotFoundError("gh")
    assert st.search_count("q", ok) == 3
    assert st.search_count("q", bad) is None
    assert st.search_count("q", missing) is None


def test_collect_and_render(repo, tmp_path):
    _w(repo, "docs/AUTOCODING_LOG.md", LOG)
    commit_all(repo, "feat: logged")
    cov = tmp_path / "cov.json"
    cov.write_text(json.dumps({"threshold": 96.0, "overall_percent": 98.5, "packages": [
        {"language": "go", "package": "internal/geo", "percent": 100.0, "status": "pass"},
        {"language": "python", "package": "src/agents_py/core", "percent": None, "status": "pending"},
    ]}), encoding="utf-8")

    real_run = subprocess.run
    queries = []

    def run(args, **kw):
        if args[0] == "gh":
            queries.append(args[args.index("-f") + 1])
            return subprocess.CompletedProcess(args, 0, "2\n", "")
        return real_run(args, **kw)

    s = st.collect(cov, run=run, root=repo, repo="acme/hexago")
    assert (s.total_commits, s.logged_commits, s.log_entries) == (1, 1, 2)
    assert s.autofix_prs == 2 and s.logged_share == 100.0
    assert "q=repo:acme/hexago is:pr label:ai-autofix" in queries
    md = st.render(s)
    assert "| Commits logged in [AUTOCODING_LOG.md](AUTOCODING_LOG.md) | 1 (100.0%) |" in md
    assert "| Core coverage (gate ≥ 96%) | 98.50% |" in md
    assert "| python | `src/agents_py/core` | — | pending |" in md


def test_render_without_coverage_or_gh():
    s = st.Stats("abc", 0, 0, 0, None, None, None, None)
    md = st.render(s)
    assert "| `ai-autofix` PRs opened | n/a |" in md and "| Core coverage | n/a |" in md
    assert s.logged_share == 0.0


def test_main_writes_file(repo, tmp_path, monkeypatch):
    _w(repo, "docs/AUTOCODING_LOG.md", LOG)
    commit_all(repo)
    monkeypatch.setattr(st, "REPO_ROOT", repo)
    monkeypatch.setattr(st, "search_count", lambda q, run=None: None)
    out = tmp_path / "STATS.md"
    collect = st.collect
    monkeypatch.setattr(st, "collect", lambda cov: collect(cov, root=repo, repo="acme/hexago"))
    assert st.main(["--out", str(out)]) == 0
    assert out.read_text(encoding="utf-8").startswith("# Autocoding Stats")


def test_run_raises_on_git_error(tmp_path):
    import pytest
    with pytest.raises(RuntimeError):
        st._run(["git", "rev-parse", "HEAD"], subprocess.run, tmp_path)


def test_origin_repo_parsing(repo):
    assert st.origin_repo(root=repo) == ""  # local bare origin isn't GitHub
    for url in ("https://github.com/acme/hexago.git", "git@github.com:acme/hexago.git",
                "https://github.com/acme/hexago"):
        fake = lambda args, u=url, **kw: subprocess.CompletedProcess(args, 0, u + "\n", "")  # noqa: E731
        assert st.origin_repo(fake, repo) == "acme/hexago"
    fail = lambda args, **kw: subprocess.CompletedProcess(args, 2, "", "no remote")  # noqa: E731
    assert st.origin_repo(fail, repo) == ""


def test_collect_without_repo_skips_pr_search(repo, monkeypatch):
    _w(repo, "x.txt", "1")
    commit_all(repo)
    monkeypatch.delenv("GITHUB_REPOSITORY", raising=False)
    s = st.collect(None, root=repo)
    assert s.autofix_prs is None and s.coverage is None
