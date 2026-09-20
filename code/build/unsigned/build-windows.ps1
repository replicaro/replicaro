param(
    [string]$TargetOut = $env:REPLICARO_UNSIGNED_OUT
)

$ErrorActionPreference = "Stop"
$env:GOENV = "off"
$env:GOFLAGS = ""

$buildDirectory = Split-Path -Parent $PSCommandPath
$repo = [IO.Path]::GetFullPath((Join-Path $buildDirectory "..\..\.."))
$code = Join-Path $repo "code"
$backend = Join-Path $code "backend"
$frontend = Join-Path $code "frontend"
$inputsPath = Join-Path $buildDirectory "inputs.json"
$manifestPath = Join-Path $buildDirectory "public-source-v1.json"

if ([string]::IsNullOrWhiteSpace($TargetOut)) {
    $TargetOut = Join-Path $buildDirectory "out\windows_amd64"
}
$TargetOut = [IO.Path]::GetFullPath($TargetOut)
if (Test-Path -LiteralPath $TargetOut) {
    throw "Refusing to reuse unsigned output root: $TargetOut"
}
$work = Join-Path $TargetOut "work"
$release = Join-Path $TargetOut "release"

if ($env:REPLICARO_SIGNED_BUILD -and $env:REPLICARO_SIGNED_BUILD -ne "0") { throw "This public builder only creates unsigned outputs." }
$buildTimestamp = [string]$env:REPLICARO_BUILD_TIMESTAMP
Push-Location $buildDirectory
try {
    $sourceDateEpoch = (& go run ./cmd/contract timestamp --value $buildTimestamp | Select-Object -Last 1).Trim()
    if ($LASTEXITCODE -ne 0 -or $sourceDateEpoch -notmatch '^[1-9][0-9]*$') { throw "A canonical UTC build timestamp is required." }
} finally { Pop-Location }

Push-Location $backend
try {
    & go run -mod=readonly ./tools/targetverify --target windows_amd64
    if ($LASTEXITCODE -ne 0) { throw "Selected Windows component verification failed." }
} finally {
    Pop-Location
}

Push-Location $buildDirectory
try {
    $sourceIdentity = (& go run ./cmd/contract source --root $repo --manifest $manifestPath | Select-Object -Last 1).Trim()
    if ($LASTEXITCODE -ne 0 -or $sourceIdentity -notmatch '^sha256:[0-9a-f]{64}$') {
        throw "Could not compute the canonical public source identity."
    }
} finally {
    Pop-Location
}

$inputs = Get-Content -LiteralPath $inputsPath -Raw | ConvertFrom-Json
$package = Get-Content -LiteralPath (Join-Path $frontend "package.json") -Raw | ConvertFrom-Json
$version = [string]$package.version
if ($inputs.schema -ne "replicaro-unsigned-build-inputs-v1" -or $version -ne [string]$inputs.version) {
    throw "Frontend version differs from unsigned build inputs."
}
if ((& go env GOVERSION).Trim() -ne "go$([string]$inputs.toolchains.go)" -or
    (& node --version).Trim() -ne "v$([string]$inputs.toolchains.node)" -or
    (& npm --version).Trim() -ne [string]$inputs.toolchains.npm) {
    throw "Windows unsigned build toolchains differ from inputs.json."
}
if ($env:PROCESSOR_ARCHITECTURE -ne "AMD64") {
    throw "windows_amd64 requires a native AMD64 runner."
}

$frontendOutput = Join-Path $work "frontend\dist"
Push-Location $backend
try {
    & go run -mod=readonly ./tools/webuiembed -preflight-root $work -frontend-output $frontendOutput
    if ($LASTEXITCODE -ne 0) { throw "Frontend output path failed safety preflight." }
} finally {
    Pop-Location
}

. (Join-Path $buildDirectory 'compile-windows.ps1') -Stage helper
. (Join-Path $buildDirectory 'compile-windows.ps1') -Stage application
if ((Get-Item -LiteralPath $binary).Length -gt 750MB) {
    throw "Windows executable exceeds target-only release ceiling."
}

$binaryHash = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash.ToLowerInvariant()
$provenance = [ordered]@{
    schema = "replicaro-product-provenance-v2"
    target = "windows_amd64"
    version = $version
    buildTimestamp = $buildTimestamp
    source = "https://github.com/replicaro/replicaro"
    sourceIdentity = $sourceIdentity
    executableSha256 = "sha256:$binaryHash"
    signed = $false
}
[IO.File]::WriteAllText((Join-Path $work "product-provenance.json"), (($provenance | ConvertTo-Json -Compress) + "`n"), [Text.UTF8Encoding]::new($false))

& (Join-Path $buildDirectory "package-windows.ps1") -TargetOut $work
if ($LASTEXITCODE -ne 0) { throw "Windows packaging failed." }
$setup = Join-Path $work "Replicaro-$version-windows_amd64-setup.exe"

Copy-Item -LiteralPath (Join-Path $work "Replicaro-$version-windows_amd64-portable.zip") -Destination $release
Copy-Item -LiteralPath $setup -Destination $release

Push-Location $buildDirectory
try {
    & go run ./cmd/contract write --root $release --inputs $inputsPath --target windows_amd64 --source-identity $sourceIdentity --build-timestamp $buildTimestamp
    if ($LASTEXITCODE -ne 0) { throw "Could not write the unsigned artifact contract." }
} finally {
    Pop-Location
}
Write-Host "Unsigned Windows outputs created in $release."
