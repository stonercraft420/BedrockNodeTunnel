[CmdletBinding()]
param(
    [ValidateSet("install", "update", "uninstall")]
    [string]$Action = "install",

    [string]$InstallDir = "",

    [switch]$PurgeData,

    [switch]$NoPathUpdate
)

$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Repository = "stonercraft420/BedrockNodeTunnel"
$ReleaseBase = "https://github.com/$Repository/releases/latest/download"
$ConfigDir = Join-Path $env:APPDATA "bedrocknode-tunnel"

if ([string]::IsNullOrWhiteSpace($InstallDir)) {
    $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\BedrockNodeTunnel"
}
$InstallDir = [IO.Path]::GetFullPath($InstallDir)
$BinaryPath = Join-Path $InstallDir "nethernet-client.exe"

function Remove-UserPathEntry {
    param([string]$PathToRemove)

    $current = [Environment]::GetEnvironmentVariable("Path", "User")
    $entries = @($current -split ";" | Where-Object {
        -not [string]::IsNullOrWhiteSpace($_) -and
        [string]::Compare($_.TrimEnd("\"), $PathToRemove.TrimEnd("\"), $true) -ne 0
    })
    [Environment]::SetEnvironmentVariable("Path", ($entries -join ";"), "User")
}

if ($Action -eq "uninstall") {
    if (Test-Path -LiteralPath $BinaryPath) {
        Remove-Item -LiteralPath $BinaryPath -Force
    }

    if (-not $NoPathUpdate) {
        Remove-UserPathEntry -PathToRemove $InstallDir
        $env:Path = (($env:Path -split ";" | Where-Object {
            [string]::Compare($_.TrimEnd("\"), $InstallDir.TrimEnd("\"), $true) -ne 0
        }) -join ";")
    }

    if ($PurgeData -and (Test-Path -LiteralPath $ConfigDir)) {
        Remove-Item -LiteralPath $ConfigDir -Recurse -Force
        Write-Host "Removed saved tunnel token and node configuration."
    } else {
        Write-Host "Saved tunnel data was kept at: $ConfigDir"
    }

    if (Test-Path -LiteralPath $InstallDir) {
        $remaining = Get-ChildItem -LiteralPath $InstallDir -Force
        if (-not $remaining) {
            Remove-Item -LiteralPath $InstallDir -Force
        }
    }

    Write-Host "nethernet-client has been uninstalled."
    return
}

$architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
switch ($architecture) {
    "X64"   { $GoArch = "amd64" }
    "Arm64" { $GoArch = "arm64" }
    default { throw "Unsupported Windows architecture: $architecture. Supported: x64 and ARM64." }
}

$Asset = "nethernet-client-windows-$GoArch.exe"
$TempDir = Join-Path ([IO.Path]::GetTempPath()) ("BedrockNodeTunnel-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null

try {
    Write-Host "Downloading latest Windows/$GoArch build..."
    $DownloadPath = Join-Path $TempDir $Asset
    $ChecksumsPath = Join-Path $TempDir "SHA256SUMS"

    Invoke-WebRequest -Uri "$ReleaseBase/$Asset" -OutFile $DownloadPath -UseBasicParsing
    Invoke-WebRequest -Uri "$ReleaseBase/SHA256SUMS" -OutFile $ChecksumsPath -UseBasicParsing

    $checksumLine = Get-Content -LiteralPath $ChecksumsPath | Where-Object {
        $_ -match ("^([a-fA-F0-9]{64})\s+\*?" + [regex]::Escape($Asset) + "$")
    } | Select-Object -First 1

    if (-not $checksumLine) {
        throw "No SHA-256 entry found for $Asset. Refusing to install an unverified build."
    }

    $expectedHash = (($checksumLine -split "\s+")[0]).ToLowerInvariant()
    $actualHash = (Get-FileHash -LiteralPath $DownloadPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -ne $expectedHash) {
        throw "SHA-256 verification failed for $Asset."
    }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    try {
        Move-Item -LiteralPath $DownloadPath -Destination $BinaryPath -Force
    } catch {
        throw "Could not replace nethernet-client.exe. Stop any running client and retry. $($_.Exception.Message)"
    }

    if (-not $NoPathUpdate) {
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $entries = @($userPath -split ";" | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
        $alreadyPresent = $false
        foreach ($entry in $entries) {
            if ([string]::Compare($entry.TrimEnd("\"), $InstallDir.TrimEnd("\"), $true) -eq 0) {
                $alreadyPresent = $true
                break
            }
        }
        if (-not $alreadyPresent) {
            $newPath = if ([string]::IsNullOrWhiteSpace($userPath)) { $InstallDir } else { "$userPath;$InstallDir" }
            [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
        }
        if (($env:Path -split ";" | Where-Object {
            [string]::Compare($_.TrimEnd("\"), $InstallDir.TrimEnd("\"), $true) -eq 0
        }).Count -eq 0) {
            $env:Path = "$env:Path;$InstallDir"
        }
    }

    $installedVersion = & $BinaryPath -version
    Write-Host "Installed nethernet-client $installedVersion to $BinaryPath"
    Write-Host "Saved tokens and node settings are preserved in: $ConfigDir"
    if ($Action -eq "update") {
        Write-Host "Update complete."
    }
} finally {
    if (Test-Path -LiteralPath $TempDir) {
        Remove-Item -LiteralPath $TempDir -Recurse -Force
    }
}
