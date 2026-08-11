<#
.SYNOPSIS
    Installer for sortie - spec-first agent orchestrator.

.DESCRIPTION
    Downloads the latest sortie release for Windows, verifies its checksum,
    installs sortie.exe, and adds the install directory to the user PATH.

    Usage:
        irm 'https://get.sortie-ai.com/install.ps1' | iex

    Environment:
        SORTIE_VERSION      Pin a specific release, with or without the
                            leading "v" (e.g. 1.18.0 or v1.18.0).
        SORTIE_INSTALL_DIR  Override install directory
                            (default: %LOCALAPPDATA%\Programs\sortie).
        SORTIE_NO_VERIFY    Set to 1 to skip checksum verification.

    Compatible with Windows PowerShell 5.1 and PowerShell 7+.
#>

#Requires -Version 5.1

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Repo = 'sortie-ai/sortie'
$Bin  = 'sortie'

# ── Formatting ──────────────────────────────────────────────────────────────────

# Mirror install.sh: info/ok use a ":: " prefix, err uses "error: ".
function Write-Info { param([string]$Message) Write-Host ':: ' -ForegroundColor Cyan  -NoNewline; Write-Host $Message }
function Write-Ok   { param([string]$Message) Write-Host ':: ' -ForegroundColor Green -NoNewline; Write-Host $Message }
function Write-Err  { param([string]$Message) [Console]::Error.WriteLine("error: $Message") }

# True when the console can render UTF-8 glyphs (ASCII art, spade).
function Test-Utf8Console {
    try {
        $enc  = [Console]::OutputEncoding
        $char = [char]0x2588  # █
        return ($enc.GetString($enc.GetBytes($char)) -eq $char)
    }
    catch { return $false }
}

# ── Platform detection ──────────────────────────────────────────────────────────

# Map the host processor architecture to a release arch token (amd64|arm64).
function Get-Architecture {
    # Prefer the .NET runtime view. Windows PowerShell 5.1 can hide members on
    # RuntimeInformation, so any failure falls back to environment variables.
    try {
        $osArch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
        switch ($osArch) {
            'X64'   { return 'amd64' }
            'Arm64' { return 'arm64' }
        }
    }
    catch {
        # Fall through to the environment-variable probe below.
    }

    # PROCESSOR_ARCHITEW6432 is set when a 32-bit process runs on 64-bit Windows;
    # it reports the true OS architecture. Otherwise PROCESSOR_ARCHITECTURE does.
    $envArch = $env:PROCESSOR_ARCHITEW6432
    if ([string]::IsNullOrEmpty($envArch)) { $envArch = $env:PROCESSOR_ARCHITECTURE }
    switch ($envArch) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
    }

    throw "unsupported architecture: $envArch"
}

# ── HTTP helpers ────────────────────────────────────────────────────────────────

# Download a URL to a file. -UseBasicParsing is required on 5.1 and harmless on 7.
function Invoke-Download {
    param(
        [Parameter(Mandatory)][string]$Url,
        [Parameter(Mandatory)][string]$OutFile
    )
    Invoke-WebRequest -Uri $Url -OutFile $OutFile -UseBasicParsing
}

# ── Version resolution ──────────────────────────────────────────────────────────

# Use $env:SORTIE_VERSION when set, otherwise read the latest release tag from
# the GitHub API. GitHub requires a User-Agent header.
function Resolve-Tag {
    if (-not [string]::IsNullOrEmpty($env:SORTIE_VERSION)) {
        return $env:SORTIE_VERSION
    }

    $url = "https://api.github.com/repos/$Repo/releases/latest"
    try {
        $release = Invoke-RestMethod -Uri $url -UseBasicParsing `
            -Headers @{ 'User-Agent' = 'sortie-installer' }
    }
    catch {
        throw 'GitHub API request failed (rate-limited? set SORTIE_VERSION to skip)'
    }

    # StrictMode throws on a missing property, so probe before reading.
    $tagProp = $release.PSObject.Properties['tag_name']
    if ($null -eq $tagProp -or [string]::IsNullOrEmpty($tagProp.Value)) {
        throw 'could not determine latest release'
    }
    return $tagProp.Value
}

# ── Checksum verification ────────────────────────────────────────────────────────

# Verify the SHA-256 of $FilePath against the entry for its file name in
# checksums.txt (each line: "<hex-sha256>  <filename>").
function Test-Checksum {
    param(
        [Parameter(Mandatory)][string]$FilePath,
        [Parameter(Mandatory)][string]$SumsPath
    )
    $name = Split-Path -Leaf $FilePath

    $want = $null
    foreach ($line in Get-Content -LiteralPath $SumsPath) {
        # Split on whitespace; goreleaser uses two spaces between hash and name.
        $fields = $line -split '\s+', 2
        if ($fields.Count -eq 2 -and $fields[1].Trim() -eq $name) {
            $want = $fields[0].Trim()
            break
        }
    }
    if ([string]::IsNullOrEmpty($want)) {
        throw "no checksum entry for $name"
    }

    $got = (Get-FileHash -LiteralPath $FilePath -Algorithm SHA256).Hash
    if ($want -ne $got) {
        throw "checksum mismatch (expected $want, got $got)"
    }
}

# ── Install directory resolution ──────────────────────────────────────────────────

# $env:SORTIE_INSTALL_DIR when set, otherwise %LOCALAPPDATA%\Programs\sortie.
# Never %ProgramFiles% by default - that would require elevation.
function Resolve-InstallDir {
    if (-not [string]::IsNullOrEmpty($env:SORTIE_INSTALL_DIR)) {
        return $env:SORTIE_INSTALL_DIR
    }
    return (Join-Path $env:LOCALAPPDATA 'Programs\sortie')
}

# ── PATH update ──────────────────────────────────────────────────────────────────

# Append $Dir to the User-scope PATH if absent (registry-backed, not the merged
# process PATH). Returns $true when the variable was modified.
function Add-ToUserPath {
    param([Parameter(Mandatory)][string]$Dir)

    $current = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ($null -eq $current) { $current = '' }

    # Case-insensitive membership check, tolerating empty/stray entries.
    # @(...) forces an array so += always appends (a bare pipeline assignment
    # yields a scalar string for a single entry, which += would concatenate).
    $entries = @($current -split ';' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    foreach ($entry in $entries) {
        if ($entry.TrimEnd('\') -ieq $Dir.TrimEnd('\')) {
            return $false
        }
    }

    $entries += $Dir
    $updated = ($entries -join ';')
    [Environment]::SetEnvironmentVariable('Path', $updated, 'User')

    # Make sortie usable in the current session without a restart.
    $env:Path = "$Dir;$env:Path"
    return $true
}

# ── Post-install summary ──────────────────────────────────────────────────────────

# Mirror install.sh's summary block exactly (taglines, labels, ordering).
function Show-Summary {
    param(
        [Parameter(Mandatory)][string]$Tag,
        [Parameter(Mandatory)][string]$Version
    )
    $utf8 = Test-Utf8Console

    Write-Host ''
    if ($utf8) {
        $art = @(
            '  ███████╗ ██████╗ ██████╗ ████████╗██╗███████╗',
            '  ██╔════╝██╔═══██╗██╔══██╗╚══██╔══╝██║██╔════╝',
            '  ███████╗██║   ██║██████╔╝   ██║   ██║█████╗',
            '  ╚════██║██║   ██║██╔══██╗   ██║   ██║██╔══╝',
            '  ███████║╚██████╔╝██║  ██║   ██║   ██║███████╗',
            '  ╚══════╝ ╚═════╝ ╚═╝  ╚═╝   ╚═╝   ╚═╝╚══════╝'
        )
        foreach ($line in $art) { Write-Host $line -ForegroundColor Cyan }
    }
    else {
        Write-Host '  Sortie'
    }

    Write-Host '  Turns issue tickets into autonomous sessions.' -ForegroundColor DarkGray
    Write-Host ''
    Write-Host '  Docs:      ' -ForegroundColor Yellow -NoNewline
    Write-Host ' https://docs.sortie-ai.com'
    Write-Host '  Changelog: ' -ForegroundColor Yellow -NoNewline
    Write-Host " https://docs.sortie-ai.com/changelog/#$Version"
    Write-Host '  GitHub:    ' -ForegroundColor Yellow -NoNewline
    Write-Host " https://github.com/$Repo"

    Write-Host ''
    if ($utf8) {
        Write-Host '  Happy hacking! ' -NoNewline
        Write-Host '♠' -ForegroundColor Cyan
    }
    else {
        Write-Host '  Happy hacking!'
    }
    Write-Host ''
}

# ── Main ──────────────────────────────────────────────────────────────────────────

function Invoke-Install {
    # Force TLS 1.2 for PS 5.1 on older Windows; -bor keeps TLS 1.3 on PS 7.
    [Net.ServicePointManager]::SecurityProtocol = `
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    # The Invoke-WebRequest progress bar makes downloads very slow on PS 5.1.
    $script:OldProgressPreference = $ProgressPreference
    $ProgressPreference = 'SilentlyContinue'

    $arch = Get-Architecture
    Write-Info "Platform: windows/$arch"

    $resolved = Resolve-Tag
    $version = $resolved -replace '^v', ''
    # $tag is the label reported to the user; the tag form itself matters only
    # when building the download URL.
    $tag = $version
    Write-Info "Release:  $version"

    $archive = "${Bin}_${version}_windows_${arch}.zip"

    # Releases from 1.19.0 on are tagged "v1.19.0"; earlier ones are tagged
    # "1.18.0". A pinned version comes from the user and may use either form,
    # so try the current convention first and fall back to the legacy one. A
    # tag read from the latest release is already exact - use it as is.
    if (-not [string]::IsNullOrEmpty($env:SORTIE_VERSION)) {
        $candidates = @("v$version", $version)
    }
    else {
        $candidates = @($resolved)
    }

    $tmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("sortie-install-" + (New-Guid).Guid)
    New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

    try {
        $archivePath = Join-Path $tmpDir $archive

        Write-Info "Downloading $archive"
        # Errors are held back while candidates are tried: a 404 on the first
        # form only means the release uses the other one, and reporting it would
        # alarm a user whose install then succeeds. If every form fails, the last
        # error is released so a genuine network fault stays diagnosable.
        $base = $null
        $lastError = $null
        foreach ($candidate in $candidates) {
            $candidateBase = "https://github.com/$Repo/releases/download/$candidate"
            try {
                Invoke-Download -Url "$candidateBase/$archive" -OutFile $archivePath
                $base = $candidateBase
                break
            }
            catch {
                $lastError = $_
            }
        }
        if ($null -eq $base) {
            if ($null -ne $lastError) {
                Write-Err $lastError.Exception.Message
            }
            throw "download failed - verify release $version has asset for windows/$arch"
        }

        if ($env:SORTIE_NO_VERIFY -ne '1') {
            $sumsPath = Join-Path $tmpDir 'checksums.txt'
            try {
                Invoke-Download -Url "$base/checksums.txt" -OutFile $sumsPath
            }
            catch {
                throw 'failed to download checksums'
            }
            Test-Checksum -FilePath $archivePath -SumsPath $sumsPath
            Write-Info 'Checksum verified'
        }

        # Extract into the temp dir; the real install happens only after this
        # succeeds, so a failure never leaves files in the install directory.
        $extractDir = Join-Path $tmpDir 'extracted'
        Expand-Archive -Path $archivePath -DestinationPath $extractDir -Force

        $exe = Get-ChildItem -Path $extractDir -Filter "$Bin.exe" -Recurse -File |
            Select-Object -First 1
        if ($null -eq $exe) {
            throw "$Bin.exe not found in archive"
        }

        $dir = Resolve-InstallDir
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
        $target = Join-Path $dir "$Bin.exe"
        Move-Item -LiteralPath $exe.FullName -Destination $target -Force
        Unblock-File -LiteralPath $target

        Write-Ok "Installed $Bin $tag to $target"

        if (Add-ToUserPath -Dir $dir) {
            Write-Host ''
            Write-Info "Added $dir to your PATH."
            Write-Host '  Restart open shell sessions for the change to take effect.' -ForegroundColor DarkGray
        }

        Show-Summary -Tag $tag -Version $version
    }
    finally {
        if (Test-Path -LiteralPath $tmpDir) {
            Remove-Item -LiteralPath $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
        }
        $ProgressPreference = $script:OldProgressPreference
    }
}

try {
    Invoke-Install
}
catch {
    Write-Err $_.Exception.Message
    exit 1
}
