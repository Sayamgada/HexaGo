import json
from pathlib import Path

import coverage_gate as cg

MOD = "github.com/hexago/hexago/services/dispatch-go"

# geo: 10 stmts, all hit (the miss in binary 1 is hit in binary 2).
# fleet: 100 stmts, 96 hit. matching: 100 stmts, 95 hit. sim: ignored.
PROFILE = f"""mode: atomic
{MOD}/internal/geo/geo.go:1.1,2.1 10 0
{MOD}/internal/geo/geo.go:1.1,2.1 10 3
{MOD}/internal/fleet/fleet.go:1.1,2.1 96 1
{MOD}/internal/fleet/fleet.go:3.1,4.1 4 0
{MOD}/internal/matching/matching.go:1.1,2.1 95 2
{MOD}/internal/matching/matching.go:3.1,4.1 5 0
{MOD}/internal/sim/sim.go:1.1,2.1 50 0
{MOD}/internal/geopolitics/x.go:1.1,2.1 7 0
garbage line
"""

COBERTURA = """<?xml version="1.0" ?>
<coverage version="7.6">
  <sources><source>/home/runner/work/HexaGo/HexaGo/services/agents-py/src</source></sources>
  <packages><package name="agents_py.core"><classes>
    <class filename="agents_py/core/llm.py" name="llm.py"><lines>
      <line number="1" hits="1"/><line number="2" hits="1"/><line number="3" hits="0"/><line number="4" hits="1"/>
    </lines></class>
    <class filename="agents_py/main.py" name="main.py"><lines>
      <line number="1" hits="0"/>
    </lines></class>
  </classes></package></packages>
</coverage>
"""


def test_go_coverage_per_package_merges_binaries():
    res = {r.package: r for r in cg.go_coverage(PROFILE, MOD, ["internal/geo", "internal/fleet",
                                                               "internal/matching", "internal/ledger"], 96)}
    assert (res["internal/geo"].statements, res["internal/geo"].covered) == (10, 10)
    assert res["internal/fleet"].percent == 96.0 and res["internal/fleet"].status == "pass"
    assert res["internal/matching"].status == "fail"
    assert res["internal/ledger"].status == "pending" and res["internal/ledger"].percent is None


def test_go_package_prefix_does_not_match_sibling():
    (geo,) = cg.go_coverage(PROFILE, MOD, ["internal/geo"], 96)
    assert geo.statements == 10  # internal/geopolitics excluded


def test_python_coverage_matches_via_source_path():
    (core,) = cg.python_coverage(COBERTURA, ["src/agents_py/core"], 96)
    assert (core.statements, core.covered) == (4, 3)
    assert core.status == "fail"
    (missing,) = cg.python_coverage(COBERTURA, ["src/agents_py/ledger"], 96)
    assert missing.status == "pending"


def test_status_rounds_to_two_decimals():
    assert cg.PackageCoverage("go", "p", 100_000, 95_999, 96).status == "fail"
    assert cg.PackageCoverage("go", "p", 100, 96, 96).status == "pass"


def test_render_markdown():
    rows = cg.go_coverage(PROFILE, MOD, ["internal/geo", "internal/matching"], 96)
    md = cg.render_markdown(rows, 96)
    assert "| go | `internal/geo` | 10 | 10 | 100.00% | ✅ pass |" in md
    assert "❌ fail" in md and "Gate failed" in md
    assert "Gate passed" in cg.render_markdown(rows[:1], 96)


def test_overall():
    assert cg.overall([]) is None
    rows = cg.go_coverage(PROFILE, MOD, ["internal/geo", "internal/fleet"], 96)
    assert cg.overall(rows) == round(100 * 106 / 110, 2)


def _setup(tmp_path: Path, profile: str | None = PROFILE, xml: str | None = COBERTURA,
           go_pkgs=("internal/geo", "internal/fleet"), py_pkgs=("src/agents_py/core",),
           threshold=96) -> Path:
    if profile is not None:
        (tmp_path / "cov.out").write_text(profile, encoding="utf-8")
    if xml is not None:
        (tmp_path / "cov.xml").write_text(xml, encoding="utf-8")
    cfg = tmp_path / "targets.yml"
    cfg.write_text(
        f"threshold: {threshold}\n"
        f"go:\n  profile: cov.out\n  module: {MOD}\n  packages: {list(go_pkgs)}\n"
        f"python:\n  report: cov.xml\n  packages: {list(py_pkgs)}\n", encoding="utf-8")
    return cfg


def test_run_passes_and_writes_summary_and_json(tmp_path):
    cfg = _setup(tmp_path, threshold=70)
    summary = tmp_path / "summary.md"
    out = tmp_path / "s.json"
    assert cg.run(cfg, tmp_path, out, str(summary)) == 0
    assert "Core coverage gate (threshold 70%)" in summary.read_text(encoding="utf-8")
    data = json.loads(out.read_text(encoding="utf-8"))
    assert data["threshold"] == 70 and len(data["packages"]) == 3
    assert data["packages"][2]["percent"] == 75.0


def test_run_fails_below_threshold(tmp_path):
    assert cg.run(_setup(tmp_path), tmp_path, None, None) == 1


def test_run_fails_when_report_missing(tmp_path):
    cfg = _setup(tmp_path, profile=None, py_pkgs=("src/agents_py/nothing",))
    out = tmp_path / "s.json"
    assert cg.run(cfg, tmp_path, out, None) == 1
    assert "coverprofile not found" in json.loads(out.read_text(encoding="utf-8"))["errors"][0]
    cfg = _setup(tmp_path, xml=None)
    (tmp_path / "cov.xml").unlink()
    assert cg.run(cfg, tmp_path, None, None) == 1


def test_main_uses_repo_config(tmp_path, monkeypatch):
    cfg = _setup(tmp_path, go_pkgs=("internal/geo",), py_pkgs=("src/agents_py/pending",))
    monkeypatch.delenv("GITHUB_STEP_SUMMARY", raising=False)
    assert cg.main(["--config", str(cfg), "--root", str(tmp_path)]) == 0


def test_repo_targets_file_lists_core_packages():
    import yaml
    cfg = yaml.safe_load((cg.REPO_ROOT / "tools" / "coverage_targets.yml").read_text(encoding="utf-8"))
    assert cfg["threshold"] == 96.0
    assert {"internal/geo", "internal/fleet", "internal/matching", "internal/safety"} <= set(cfg["go"]["packages"])
    assert {"src/agents_py/core", "src/agents_py/agents/safety"} <= set(cfg["python"]["packages"])
