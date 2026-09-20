param()

$ErrorActionPreference = "Stop"
$buildDirectory = Split-Path -Parent $PSCommandPath
$root = [IO.Path]::GetFullPath((Join-Path $buildDirectory "..\..\.."))
$backend = Join-Path $root "code\backend"
$metadataPath = Join-Path $buildDirectory "inno-setup-metadata.json"

Push-Location $backend
try {
    & go run -mod=readonly ./tools/targetverify --target windows_amd64
    if ($LASTEXITCODE -ne 0) { throw "Selected Windows component verification failed before Inno Setup acquisition." }
} finally {
    Pop-Location
}

$metadata = Get-Content -LiteralPath $metadataPath -Raw | ConvertFrom-Json
if ($metadata.schema -ne "replicaro-inno-setup-v3" -or
    $metadata.version -ne "7.1.0" -or
    $metadata.architecture -ne "x64" -or
    $metadata.compiler -ne "ISCC.exe" -or
    $metadata.repository -ne "https://github.com/jrsoftware/issrc" -or
    $metadata.release -ne "https://github.com/jrsoftware/issrc/releases/tag/is-7_1_0" -or
    $metadata.installer.url -ne "https://github.com/jrsoftware/issrc/releases/download/is-7_1_0/innosetup-7.1.0-x64.exe" -or
    $metadata.installer.sha256 -ne "0362a383ed217d4c4239b5933866dd96d3eb2102737da92f80f6057a4b40df2f" -or
    [int64]$metadata.installer.size -ne 14304168 -or
    $metadata.installer.redirectHost -ne "release-assets.githubusercontent.com" -or
    $metadata.license -ne "https://github.com/jrsoftware/issrc/blob/is-7_1_0/license.txt") {
    throw "The pinned Inno Setup acquisition metadata is incomplete or changed."
}

function Test-X64PEImage {
    param([Parameter(Mandatory = $true)][string]$Path)

    $stream = $null
    $reader = $null
    try {
        $stream = [IO.File]::Open($Path, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
        $reader = [IO.BinaryReader]::new($stream)
        if ($stream.Length -lt 64 -or $reader.ReadUInt16() -ne 0x5a4d) { return $false }
        $stream.Position = 0x3c
        $peOffset = $reader.ReadInt32()
        if ($peOffset -lt 0 -or $peOffset + 6 -gt $stream.Length) { return $false }
        $stream.Position = $peOffset
        return $reader.ReadUInt32() -eq 0x00004550 -and $reader.ReadUInt16() -eq 0x8664
    } catch {
        return $false
    } finally {
        if ($null -ne $reader) {
            $reader.Dispose()
        } elseif ($null -ne $stream) {
            $stream.Dispose()
        }
    }
}

if ([string]::IsNullOrWhiteSpace($env:RUNNER_TEMP) -or -not [IO.Path]::IsPathRooted($env:RUNNER_TEMP)) {
    throw "RUNNER_TEMP must identify an absolute workflow-owned temporary directory."
}
$runnerTemp = [IO.Path]::GetFullPath($env:RUNNER_TEMP)
if (!(Test-Path -LiteralPath $runnerTemp -PathType Container)) {
    throw "RUNNER_TEMP is not an available directory."
}
$stage = Join-Path $runnerTemp ("replicaro-inno-setup-" + [Guid]::NewGuid().ToString("N"))
if (Test-Path -LiteralPath $stage) { throw "Refusing to reuse Inno Setup acquisition staging." }
$downloadDirectory = Join-Path $stage "download"
$installDirectory = Join-Path $stage "installed\Inno Setup 7"
$installerPath = Join-Path $downloadDirectory "innosetup-7.1.0-x64.exe"
$succeeded = $false

New-Item -ItemType Directory -Path $downloadDirectory | Out-Null
try {
    Add-Type -AssemblyName System.Net.Http
    $handler = [Net.Http.HttpClientHandler]::new()
    $handler.AllowAutoRedirect = $false
    $client = [Net.Http.HttpClient]::new($handler)
    $client.DefaultRequestHeaders.UserAgent.ParseAdd("Replicaro-build/1.0")
    $initialResponse = $null
    $assetResponse = $null
    $assetStream = $null
    $outputStream = $null
    try {
        $initialResponse = $client.GetAsync([string]$metadata.installer.url).GetAwaiter().GetResult()
        if ([int]$initialResponse.StatusCode -notin @(301, 302, 303, 307, 308)) {
            throw "The official Inno Setup release URL did not return one required GitHub asset redirect."
        }
        $redirect = $initialResponse.Headers.Location
        if ($null -eq $redirect -or !$redirect.IsAbsoluteUri -or
            $redirect.Scheme -ne "https" -or
            $redirect.Host -ne [string]$metadata.installer.redirectHost) {
            throw "The official Inno Setup release URL redirected outside GitHub's release-asset host."
        }
        $assetResponse = $client.GetAsync($redirect).GetAwaiter().GetResult()
        if (!$assetResponse.IsSuccessStatusCode -or $null -ne $assetResponse.Headers.Location) {
            throw "The GitHub Inno Setup release asset did not return one final successful response."
        }
        $assetStream = $assetResponse.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
        $outputStream = [IO.File]::Open($installerPath, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
        $assetStream.CopyTo($outputStream)
    } finally {
        if ($null -ne $outputStream) { $outputStream.Dispose() }
        if ($null -ne $assetStream) { $assetStream.Dispose() }
        if ($null -ne $assetResponse) { $assetResponse.Dispose() }
        if ($null -ne $initialResponse) { $initialResponse.Dispose() }
        if ($null -ne $client) { $client.Dispose() }
        if ($null -ne $handler) { $handler.Dispose() }
    }

    $installer = Get-Item -LiteralPath $installerPath
    if ($installer.Length -ne [int64]$metadata.installer.size) {
        throw "The downloaded Inno Setup installer size differs from the pinned official release asset."
    }
    $actualHash = (Get-FileHash -LiteralPath $installerPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -ne [string]$metadata.installer.sha256) {
        throw "The downloaded Inno Setup installer hash differs from the pinned official release asset."
    }

    $arguments = @(
        "/VERYSILENT",
        "/SUPPRESSMSGBOXES",
        "/NORESTART",
        "/NOICONS",
        "/CURRENTUSER",
        "/DIR=`"$installDirectory`""
    )
    $installProcess = Start-Process -FilePath $installerPath -ArgumentList $arguments -Wait -PassThru
    if ($installProcess.ExitCode -ne 0) {
        throw "Official Inno Setup installation failed with exit code $($installProcess.ExitCode)."
    }
    $compiler = Join-Path $installDirectory "ISCC.exe"
    if (!(Test-Path -LiteralPath $compiler -PathType Leaf) -or !(Test-X64PEImage -Path $compiler)) {
        throw "The installed Inno Setup compiler is missing or is not a native x64 PE image."
    }

    $probeDirectory = Join-Path $stage "probe"
    New-Item -ItemType Directory -Path $probeDirectory | Out-Null
    $probeDefinition = Join-Path $probeDirectory "probe.iss"
    $probeSource = @'
#if Ver != 117506048
  #error "Exact Inno Setup 7.1.0.0 is required"
#endif
[Setup]
AppName=Replicaro Inno Setup Probe
AppVersion=1.0.0
DefaultDirName={tmp}\ReplicaroInnoSetupProbe
Uninstallable=no
SetupArchitecture=x64
OutputDir=.
OutputBaseFilename=probe
'@
    [IO.File]::WriteAllText($probeDefinition, $probeSource, [Text.UTF8Encoding]::new($false))
    & $compiler "/Qp" $probeDefinition
    if ($LASTEXITCODE -ne 0) { throw "Pinned native x64 Inno Setup compiler probe failed." }
    $probeOutput = Join-Path $probeDirectory "probe.exe"
    if (!(Test-Path -LiteralPath $probeOutput -PathType Leaf) -or !(Test-X64PEImage -Path $probeOutput)) {
        throw "Pinned Inno Setup did not produce a native x64 Setup probe."
    }

    Push-Location $backend
    try {
        & go run -mod=readonly ./tools/targetverify --target windows_amd64
        if ($LASTEXITCODE -ne 0) { throw "Selected Windows component verification failed after Inno Setup acquisition." }
    } finally {
        Pop-Location
    }
    Remove-Item -LiteralPath $probeDirectory -Recurse -Force

    if ([string]::IsNullOrWhiteSpace($env:GITHUB_ENV) -or -not [IO.Path]::IsPathRooted($env:GITHUB_ENV)) {
        throw "GITHUB_ENV must identify the workflow environment file."
    }
    "REPLICARO_INNO_COMPILER=$compiler" | Out-File -FilePath $env:GITHUB_ENV -Encoding utf8 -Append
    $succeeded = $true
} finally {
    if (Test-Path -LiteralPath $downloadDirectory) {
        Remove-Item -LiteralPath $downloadDirectory -Recurse -Force
    }
    if (!$succeeded -and (Test-Path -LiteralPath $stage)) {
        Remove-Item -LiteralPath $stage -Recurse -Force
    }
}

Write-Host "Pinned official Inno Setup 7.1.0 native x64 compiler is ready."
