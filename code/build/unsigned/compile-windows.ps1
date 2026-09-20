param([ValidateSet('helper','application')][string]$Stage)
# Dot-sourced compilation stages. The caller supplies validated source/output paths.
if ($Stage -eq 'helper') {
$helperDirectory = Join-Path $work "embed\storagehelper\assets\windows_amd64"
New-Item -ItemType Directory -Path $work, $release | Out-Null
$helper = Join-Path $helperDirectory "storage-helper.exe"
New-Item -ItemType Directory -Path $helperDirectory | Out-Null
Push-Location $backend
try {
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"
    & go build -mod=readonly -trimpath -buildvcs=false '-ldflags=-s -w' -o $helper ./cmd/storage-helper
    if ($LASTEXITCODE -ne 0) { throw "Storage helper build failed." }
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
    Pop-Location
}
} else {
$helperHash = (Get-FileHash -LiteralPath $helper -Algorithm SHA256).Hash.ToLowerInvariant()
[IO.File]::WriteAllText("$helper.sha256", $helperHash, [Text.Encoding]::ASCII)

$helperSource = Join-Path $backend "storagehelper\assets\windows_amd64\storage-helper.exe"
$helperHashSource = "$helperSource.sha256"
$helperOverlay = Join-Path $work "storage-helper-overlay.json"
$productOverlay = Join-Path $work "product-overlay.json"
$replace = [ordered]@{}
$replace[[IO.Path]::GetFullPath($helperSource)] = [IO.Path]::GetFullPath($helper)
$replace[[IO.Path]::GetFullPath($helperHashSource)] = [IO.Path]::GetFullPath("$helper.sha256")
[IO.File]::WriteAllText($helperOverlay, ([ordered]@{ Replace = $replace } | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))

Push-Location $frontend
try {
    & npm ci
    if ($LASTEXITCODE -ne 0) { throw "Frontend dependency installation failed." }
    & npm run build -- --outDir $frontendOutput --emptyOutDir
    if ($LASTEXITCODE -ne 0) { throw "Frontend build failed." }
} finally {
    Pop-Location
}

Push-Location $backend
try {
    & go run -mod=readonly ./tools/webuiembed -dist $frontendOutput -output-root $work -virtual-root (Join-Path $backend "api\webui_dist") -base-overlay $helperOverlay -helper-source $helperSource -helper-built $helper -output $productOverlay
    if ($LASTEXITCODE -ne 0) { throw "Embedded frontend validation failed." }
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"
    $binary = Join-Path $work "replicaro.exe"
    $linkerFlags = "-H=windowsgui"
    & go build -mod=readonly -tags replicaro_embedded_ui -trimpath -buildvcs=false "-ldflags=$linkerFlags" -overlay $productOverlay -o $binary .
    if ($LASTEXITCODE -ne 0) { throw "Windows application build failed." }
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
    Pop-Location
}
}
