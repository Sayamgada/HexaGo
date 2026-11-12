<#
.SYNOPSIS
  HexaGo Local Development & Test Automation Harness
.DESCRIPTION
  Provides standardized developer commands for local development, multi-service orchestration,
  and comprehensive test execution across Go, Python, and Flutter.
.EXAMPLE
  .\dev.ps1 up
  .\dev.ps1 test-go
  .\dev.ps1 test-py
  .\dev.ps1 test-flutter
  .\dev.ps1 web
#>

[CmdletBinding()]
param (
    [Parameter(Position = 0, Mandatory = $false)]
    [ValidateSet("up", "test-go", "test-py", "test-flutter", "test-all", "web", "clean", "help")]
    [string]$Command = "help"
)

$ErrorActionPreference = "Stop"
$RepoRoot = $PSScriptRoot

function Write-Header([string]$text) {
    Write-Host "`n========================================================" -ForegroundColor Cyan
    Write-Host "  $text" -ForegroundColor Cyan
    Write-Host "========================================================`n" -ForegroundColor Cyan
}

function Run-Up {
    Write-Header "Starting HexaGo Local Infrastructure & Services"
    
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
        Write-Error "Docker is required to run HexaGo infrastructure. Please start Docker Desktop."
        return
    }

    Write-Host "Starting PostgreSQL (pgvector) and Redis via docker compose..." -ForegroundColor Yellow
    docker compose up -d postgres redis
    
    Write-Host "Waiting for database and cache to report healthy..." -ForegroundColor Yellow
    Start-Sleep -Seconds 3

    Write-Host "`nPostgres and Redis are up! You can now run the services in separate terminals:" -ForegroundColor Green
    Write-Host "  1. Python Agents Service:" -ForegroundColor White
    Write-Host "     cd services\agents-py" -ForegroundColor Gray
    Write-Host "     .\.venv\Scripts\uvicorn src.agents_py.main:app --port 8000 --reload" -ForegroundColor Gray
    Write-Host "`n  2. Go Dispatch Gateway:" -ForegroundColor White
    Write-Host "     cd services\dispatch-go" -ForegroundColor Gray
    Write-Host "     go run .\cmd\server" -ForegroundColor Gray
    Write-Host "`n  3. Flutter Web Client:" -ForegroundColor White
    Write-Host "     .\dev.ps1 web" -ForegroundColor Gray
    Write-Host "`nAlternatively, build and run all 4 containers together via: docker compose up --build" -ForegroundColor Cyan
}

function Run-TestGo {
    Write-Header "Running Go Tests (services/dispatch-go)"
    Push-Location "$RepoRoot\services\dispatch-go"
    try {
        Write-Host "Executing: go test -v -cover ./..." -ForegroundColor Yellow
        go test -v -cover ./...
        if ($LASTEXITCODE -ne 0) {
            throw "Go tests failed with exit code $LASTEXITCODE"
        }
        Write-Host "`nGo tests passed successfully!" -ForegroundColor Green
    }
    finally {
        Pop-Location
    }
}

function Run-TestPy {
    Write-Header "Running Python Tests (services/agents-py)"
    $venvPytest = "$RepoRoot\services\agents-py\.venv\Scripts\pytest.exe"
    $venvPython = "$RepoRoot\services\agents-py\.venv\Scripts\python.exe"

    if (-not (Test-Path $venvPytest)) {
        Write-Host "Virtualenv not found at services/agents-py/.venv. Creating..." -ForegroundColor Yellow
        Push-Location "$RepoRoot\services\agents-py"
        try {
            py -3.12 -m venv .venv
            & .\.venv\Scripts\pip.exe install --upgrade pip
            & .\.venv\Scripts\pip.exe install -r requirements.txt -r requirements-dev.txt
        }
        finally {
            Pop-Location
        }
    }

    Push-Location "$RepoRoot\services\agents-py"
    try {
        Write-Host "Executing pytest with coverage using .venv..." -ForegroundColor Yellow
        & $venvPytest -v --cov=src --cov-report=term-missing
        if ($LASTEXITCODE -ne 0) {
            throw "Python tests failed with exit code $LASTEXITCODE"
        }
        Write-Host "`nPython tests passed successfully!" -ForegroundColor Green
    }
    finally {
        Pop-Location
    }
}

function Run-TestFlutter {
    Write-Header "Running Flutter Analyze & Tests (apps/hexago_app)"
    Push-Location "$RepoRoot\apps\hexago_app"
    try {
        Write-Host "Executing: flutter analyze" -ForegroundColor Yellow
        flutter analyze
        if ($LASTEXITCODE -ne 0) {
            throw "flutter analyze failed with exit code $LASTEXITCODE"
        }

        Write-Host "`nExecuting: flutter test --coverage" -ForegroundColor Yellow
        flutter test --coverage
        if ($LASTEXITCODE -ne 0) {
            throw "flutter test failed with exit code $LASTEXITCODE"
        }
        Write-Host "`nFlutter tests and analysis passed successfully!" -ForegroundColor Green
    }
    finally {
        Pop-Location
    }
}

function Run-Web {
    Write-Header "Launching Flutter Web with Local API URLs"
    Push-Location "$RepoRoot\apps\hexago_app"
    try {
        Write-Host "Launching Flutter Web with: API_BASE_URL=http://localhost:8080, WS_BASE_URL=ws://localhost:8080" -ForegroundColor Yellow
        flutter run -d chrome `
            --dart-define=API_BASE_URL=http://localhost:8080 `
            --dart-define=WS_BASE_URL=ws://localhost:8080
    }
    finally {
        Pop-Location
    }
}

switch ($Command) {
    "up" {
        Run-Up
    }
    "test-go" {
        Run-TestGo
    }
    "test-py" {
        Run-TestPy
    }
    "test-flutter" {
        Run-TestFlutter
    }
    "test-all" {
        Run-TestGo
        Run-TestPy
        Run-TestFlutter
        Write-Header "ALL TESTS PASSED ACROSS MONOREPO!"
    }
    "web" {
        Run-Web
    }
    "clean" {
        Write-Header "Cleaning Build Artifacts"
        docker compose down -v
        Write-Host "Cleaned containers and volumes." -ForegroundColor Green
    }
    default {
        Write-Header "HexaGo Developer CLI (dev.ps1)"
        Write-Host "Usage: .\dev.ps1 <command>`n" -ForegroundColor Yellow
        Write-Host "Commands:" -ForegroundColor White
        Write-Host "  up           - Start Postgres+Redis via docker compose and show service runner commands"
        Write-Host "  test-go      - Run Go tests with coverage (services/dispatch-go)"
        Write-Host "  test-py      - Run pytest with coverage using .venv (services/agents-py)"
        Write-Host "  test-flutter - Run flutter analyze and flutter test (apps/hexago_app)"
        Write-Host "  test-all     - Run all three test suites (Go, Python, Flutter)"
        Write-Host "  web          - Launch Flutter web in Chrome with local API & WS URLs"
        Write-Host "  clean        - Stop docker containers and remove volumes"
        Write-Host "  help         - Show this help message`n"
    }
}
