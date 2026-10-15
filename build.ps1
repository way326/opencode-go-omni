param(
    [string]$GoExe = "go",
    [string]$CCompiler = "gcc",
    [switch]$SkipTests
)

$ErrorActionPreference = "Stop"
$sourceRoot = $PSScriptRoot
$savedCGO = $env:CGO_ENABLED
$savedCC = $env:CC
$savedPath = $env:PATH
try {
    $resolvedGo = (Get-Command $GoExe -ErrorAction Stop).Source
    $resolvedCC = (Get-Command $CCompiler -ErrorAction Stop).Source
    $env:CC = $resolvedCC
    $env:PATH = (Split-Path $resolvedCC) + ";" + $savedPath
    $env:CGO_ENABLED = "1"
    if (-not $SkipTests) {
        & $resolvedGo -C $sourceRoot test ./...
        if ($LASTEXITCODE -ne 0) { throw "Offline tests failed." }
        & $resolvedGo -C $sourceRoot vet ./...
        if ($LASTEXITCODE -ne 0) { throw "go vet failed." }
    }
    $dist = Join-Path $sourceRoot "dist"
    New-Item -ItemType Directory -Path $dist -Force | Out-Null
    $library = Join-Path $dist "opencode-go-unified.dll"
    & $resolvedGo -C $sourceRoot build -trimpath -buildmode=c-shared -ldflags "-s -w" -o $library .
    if ($LASTEXITCODE -ne 0) { throw "Plugin shared-library build failed." }
    $archive = Join-Path $dist "opencode-go-unified_0.2.1-dev_windows_amd64.zip"
    & $resolvedGo -C $sourceRoot run ./.github/scripts/package-release.go -library $library -archive $archive -checksum "$archive.sha256"
    if ($LASTEXITCODE -ne 0) { throw "Packaging failed." }
    Write-Output "Built plugin only: $library"
    Get-FileHash -LiteralPath $library -Algorithm SHA256
} finally {
    $env:CGO_ENABLED = $savedCGO
    $env:CC = $savedCC
    $env:PATH = $savedPath
}
