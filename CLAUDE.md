# CLAUDE.md

@PROJECT_CONTEXT.md

## Working agreement
- Started in Google Antigravity; Claude Code is now the AI engineer. The owner writes no code by hand, so Claude writes all code, tests, and docs.

## Environment
- Windows 11, PowerShell. Use Windows paths and PowerShell syntax for commands.
- Go 1.27 and Flutter 3.50 are installed locally.
- Postgres (pgvector) and Redis run in Docker: `.\dev.ps1 up`.
- Python: use only `services/agents-py/.venv` (Python 3.12). Never use the system Python (3.15). Create a venv with `py -3.12`.

## Testing
- Always test through `.\dev.ps1`: `test-go`, `test-py`, `test-flutter`, or `test-all`.
- CI (`.github/workflows/ci.yml`) also runs Go tests with `-race` and CGO enabled, which `dev.ps1 test-go` does not.

## Definition of done (every task)
1. Tests pass (`.\dev.ps1 test-all`, or the suites you touched).
2. Add a new row to the table in `docs/AUTOCODING_LOG.md` (next entry #, UTC date, prompt summary, Agent / Tool = `Claude Code`, files, tests, result).
3. Commit with a conventional message (`feat(...)`, `fix(...)`, `chore: ...`).
4. Push to `origin main`.
5. Check CI with the gh CLI (`gh run list`, `gh run watch`, `gh run view --log-failed`) and fix any failures.
