param(
    [string]$TargetOut = $env:REPLICARO_TARGET_OUT
)

$ErrorActionPreference = "Stop"
$buildDirectory = Split-Path -Parent $PSCommandPath
$root = [IO.Path]::GetFullPath((Join-Path $buildDirectory "..\..\.."))
$backend = Join-Path $root "code\backend"
$frontendPackage = Join-Path $root "code\frontend\package.json"
$metadataPath = Join-Path $buildDirectory "inno-setup-metadata.json"
$definitionPath = Join-Path $buildDirectory "replicaro.iss"
$noticeWriter = Join-Path $buildDirectory "write-licenses.py"
$buildTimestampText = [string]$env:REPLICARO_BUILD_TIMESTAMP
$buildTimestamp = [DateTimeOffset]::MinValue
if (![DateTimeOffset]::TryParseExact($buildTimestampText, "yyyy-MM-dd'T'HH:mm:ss'Z'", [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::AssumeUniversal -bor [Globalization.DateTimeStyles]::AdjustToUniversal, [ref]$buildTimestamp) -or
    $buildTimestamp.ToString("yyyy-MM-dd'T'HH:mm:ss'Z'") -ne $buildTimestampText) {
    throw "REPLICARO_BUILD_TIMESTAMP must be canonical UTC YYYY-MM-DDTHH:MM:SSZ."
}

if ([string]::IsNullOrWhiteSpace($TargetOut)) {
    $TargetOut = Join-Path $buildDirectory "out\windows_amd64"
}
$TargetOut = [IO.Path]::GetFullPath($TargetOut)
$binary = Join-Path $TargetOut "replicaro.exe"
$provenance = Join-Path $TargetOut "product-provenance.json"
if (!(Test-Path -LiteralPath $binary -PathType Leaf)) {
    throw "The Windows product executable is missing: $binary"
}
if (!(Test-Path -LiteralPath $provenance -PathType Leaf)) {
    throw "The Windows product provenance is missing: $provenance"
}

Push-Location $backend
try {
    & go run -mod=readonly ./tools/targetverify --target windows_amd64
    if ($LASTEXITCODE -ne 0) { throw "Selected Windows component verification failed before packaging." }
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
    throw "The pinned Inno Setup metadata is incomplete or changed."
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

if ([string]::IsNullOrWhiteSpace($env:REPLICARO_INNO_COMPILER)) {
    $nativeProgramFiles = @($env:ProgramW6432, $env:ProgramFiles) |
        Where-Object { ![string]::IsNullOrWhiteSpace($_) } |
        Select-Object -Unique
    $compilerCandidates = @($nativeProgramFiles | ForEach-Object { Join-Path $_ "Inno Setup 7\ISCC.exe" })
} else {
    $compilerCandidates = @($env:REPLICARO_INNO_COMPILER)
}
$compiler = $compilerCandidates |
    Where-Object { ![string]::IsNullOrWhiteSpace($_) } |
    Where-Object { (Test-Path -LiteralPath $_ -PathType Leaf) -and (Test-X64PEImage -Path $_) } |
    Select-Object -First 1
if ([string]::IsNullOrWhiteSpace($compiler)) {
    throw "Pinned native x64 Inno Setup 7.1.0 was not found in an approved installation location."
}
$package = Get-Content -LiteralPath $frontendPackage -Raw | ConvertFrom-Json
$version = [string]$package.version
if ($version -notmatch '^[0-9]+[.][0-9]+[.][0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$') {
    throw "Canonical application version is missing or invalid."
}

$setupName = "Replicaro-$version-windows_amd64-setup"
$portableName = "Replicaro-$version-windows_amd64-portable.zip"
$setupOutput = Join-Path $TargetOut "$setupName.exe"
$portableOutput = Join-Path $TargetOut $portableName
foreach ($path in @($setupOutput, "$setupOutput.sha256", $portableOutput, "$portableOutput.sha256")) {
    if (Test-Path -LiteralPath $path) { throw "Refusing to replace an existing Windows distribution output: $path" }
}

$stage = Join-Path $TargetOut ("windows-packaging-stage-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $stage | Out-Null
try {
    Add-Type -AssemblyName System.IO.Compression
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $archiveTimestamp = $buildTimestamp
    function Add-DeterministicZipEntry {
        param(
            [Parameter(Mandatory = $true)][IO.Compression.ZipArchive]$Archive,
            [Parameter(Mandatory = $true)][string]$Source,
            [Parameter(Mandatory = $true)][string]$Name
        )
        $sourceStream = [IO.File]::OpenRead($Source)
        try {
            $entry = $Archive.CreateEntry($Name, [IO.Compression.CompressionLevel]::Optimal)
            # Create mode locks entry metadata as soon as its stream is opened.
            $entry.LastWriteTime = $archiveTimestamp
            $entryStream = $entry.Open()
            try {
                $sourceStream.CopyTo($entryStream)
            } finally {
                $entryStream.Dispose()
            }
        } finally {
            $sourceStream.Dispose()
        }
    }
    $stagedPortable = Join-Path $stage $portableName
    $stagedLicense = Join-Path $stage "LICENSES.txt"
    & python3 $noticeWriter --platform windows --output $stagedLicense
    if ($LASTEXITCODE -ne 0 -or !(Test-Path -LiteralPath $stagedLicense -PathType Leaf)) {
        throw "Could not assemble Windows third-party notices."
    }
    $portableArchive = [IO.Compression.ZipFile]::Open($stagedPortable, [IO.Compression.ZipArchiveMode]::Create)
    try {
        Add-DeterministicZipEntry -Archive $portableArchive -Source $binary -Name "replicaro.exe"
        Add-DeterministicZipEntry -Archive $portableArchive -Source $stagedLicense -Name "LICENSES.txt"
    } finally {
        $portableArchive.Dispose()
    }

    $fixedTime = $buildTimestamp.UtcDateTime
    foreach ($path in @($binary, $stagedLicense)) {
        [IO.File]::SetLastWriteTimeUtc($path, $fixedTime)
    }
    $env:REPLICARO_INNO_APP_VERSION = $version
    $env:REPLICARO_INNO_SOURCE_EXE = $binary
    $env:REPLICARO_INNO_SOURCE_LICENSES = $stagedLicense
    $env:REPLICARO_INNO_OUTPUT_DIR = $stage
    $env:REPLICARO_INNO_OUTPUT_NAME = $setupName
    try {
        & $compiler "/Qp" $definitionPath
        if ($LASTEXITCODE -ne 0) { throw "Inno Setup compilation failed." }
    } finally {
        Remove-Item Env:REPLICARO_INNO_APP_VERSION -ErrorAction SilentlyContinue
        Remove-Item Env:REPLICARO_INNO_SOURCE_EXE -ErrorAction SilentlyContinue
        Remove-Item Env:REPLICARO_INNO_SOURCE_LICENSES -ErrorAction SilentlyContinue
        Remove-Item Env:REPLICARO_INNO_OUTPUT_DIR -ErrorAction SilentlyContinue
        Remove-Item Env:REPLICARO_INNO_OUTPUT_NAME -ErrorAction SilentlyContinue
    }

    $stagedSetup = Join-Path $stage "$setupName.exe"
    if (!(Test-Path -LiteralPath $stagedSetup -PathType Leaf) -or
        !(Test-Path -LiteralPath $stagedPortable -PathType Leaf)) {
        throw "Windows distribution packaging did not produce both required payloads."
    }
    if (!(Test-X64PEImage -Path $stagedSetup)) {
        throw "Inno Setup did not produce a native x64 Windows installer."
    }

    Move-Item -LiteralPath $stagedSetup -Destination $setupOutput
    Move-Item -LiteralPath $stagedPortable -Destination $portableOutput
    foreach ($artifact in @($setupOutput, $portableOutput)) {
        $hash = (Get-FileHash -LiteralPath $artifact -Algorithm SHA256).Hash.ToLowerInvariant()
        $line = "$hash  $([IO.Path]::GetFileName($artifact))`n"
        [IO.File]::WriteAllText("$artifact.sha256", $line, [Text.UTF8Encoding]::new($false))
    }
} finally {
    if (Test-Path -LiteralPath $stage) {
        Remove-Item -LiteralPath $stage -Recurse -Force
    }
}

Write-Host "Windows distributions created: $([IO.Path]::GetFileName($setupOutput)), $portableName"
