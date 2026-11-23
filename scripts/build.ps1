<#
.SYNOPSIS
  Builds maillens.exe into bin\ (or -OutDir), with the version of the VERSION file built in.

.DESCRIPTION
  Runs go vet and go test first (the PostgreSQL tests are skipped unless MAILLENS_TEST_PG is set), then builds.

.PARAMETER OutDir
  Folder for maillens.exe. Default: bin

.PARAMETER Version
  Version to build in. Default: the content of the VERSION file.

.PARAMETER SkipTests
  Do not run go vet and go test.

.EXAMPLE
  .\scripts\build.ps1
  .\scripts\build.ps1 -OutDir D:\release -SkipTests
#>
[CmdletBinding()]
param(
    [string] $OutDir = 'bin',
    [string] $Version = '',
    [switch] $SkipTests
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Find-Go {
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    foreach ($base in @($env:GOROOT, (Join-Path $env:ProgramFiles 'Go'), 'D:\programs\Go')) {
        if ($base) {
            $candidate = Join-Path $base 'bin\go.exe'
            if (Test-Path $candidate) { return $candidate }
        }
    }
    throw 'Go was not found. Install Go, or add its bin folder to PATH.'
}

$go = Find-Go
if (-not $Version) { $Version = (Get-Content (Join-Path $root 'VERSION') -Raw).Trim() }
if ($Version -notmatch '^\d+\.\d+\.\d+$') { throw "VERSION '$Version' is not major.minor.patch" }

$fromFile = Select-String -Path (Join-Path $root 'internal\version\version.go') -Pattern 'var Version = "([^"]+)"'
if ($fromFile -and $fromFile.Matches[0].Groups[1].Value -ne $Version) {
    Write-Warning "internal\version\version.go says $($fromFile.Matches[0].Groups[1].Value) but the build uses $Version; the value of the build wins."
}

if (-not $SkipTests) {
    Write-Host '== go vet'
    & $go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
    Write-Host '== go test'
    & $go test -count=1 ./...
    if ($LASTEXITCODE -ne 0) { throw 'go test failed' }
}

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$exe = Join-Path $OutDir 'maillens.exe'
Write-Host "== go build -> $exe (version $Version)"
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
& $go build -trimpath -ldflags "-s -w -X maillens/internal/version.Version=$Version" -o $exe ./cmd/maillens
if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
& $exe version
