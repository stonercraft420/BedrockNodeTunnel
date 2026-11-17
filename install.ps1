$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Repo = "stonercraft420/BedrockNodeTunnel"
$Arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$BinaryName = "nethernet-client-windows-$Arch.exe"

Write-Host "Fetching latest release from $Repo..." -ForegroundColor Cyan
$Release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ "User-Agent" = "BedrockNode-Installer" }

$Asset = $Release.assets | Where-Object { $_.name -eq $BinaryName } | Select-Object -First 1
if (-not $Asset) {
    Write-Error "Could not find asset '$BinaryName' in the latest release."
    return
}

$InstallDir = Join-Path $env:LOCALAPPDATA "BedrockNodeTunnel"
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}

$OutFile = Join-Path $InstallDir "nethernet-client.exe"
Write-Host "Downloading $($Asset.name)..." -ForegroundColor Cyan
Invoke-WebRequest -Uri $Asset.browser_download_url -OutFile $OutFile -UseBasicParsing

$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ([string]::IsNullOrEmpty($UserPath)) {
    $NewPath = $InstallDir
    [Environment]::SetEnvironmentVariable("Path", $NewPath, "User")
    $env:Path += ";$InstallDir"
    Write-Host "Added $InstallDir to your user PATH." -ForegroundColor Yellow
} elseif ($UserPath -notlike "*$InstallDir*") {
    $NewPath = $UserPath.TrimEnd(';') + ";" + $InstallDir
    [Environment]::SetEnvironmentVariable("Path", $NewPath, "User")
    $env:Path += ";$InstallDir"
    Write-Host "Added $InstallDir to your user PATH." -ForegroundColor Yellow
}

Write-Host "Installed successfully to $OutFile!" -ForegroundColor Green
Write-Host "Run 'nethernet-client' to get started." -ForegroundColor Cyan
