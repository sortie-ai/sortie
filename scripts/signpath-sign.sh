#!/bin/sh
# Authenticode-sign a Windows binary with SignPath, in place.
#
# Invoked by GoReleaser's post-build hook once per build target. Only Windows
# .exe artifacts are signed, and only when SignPath credentials are present, so
# non-Windows targets and credential-less builds (snapshots, pull requests,
# local builds) pass through untouched. Signing happens before archiving, so the
# published .zip and checksums.txt cover the signed binary.
#
# Inputs (environment):
#   SIGNPATH_ORG_ID        SignPath organization id        (required to sign)
#   SIGNPATH_API_TOKEN     SignPath API token              (required to sign)
#   SIGNPATH_PROJECT_SLUG  SignPath project slug           (default: sortie)
#   SIGNPATH_POLICY_SLUG   SignPath signing policy slug    (default: release)

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"

artifact=${1:?usage: signpath-sign.sh <path-to-binary>}

# Only Authenticode-sign Windows PE executables.
case "$artifact" in
	*.exe) ;;
	*) exit 0 ;;
esac

# Skip when credentials are absent (snapshot / pull-request / local builds).
if ! require_env SIGNPATH_API_TOKEN SIGNPATH_ORG_ID >/dev/null 2>&1; then
	log "SIGNPATH_ORG_ID/SIGNPATH_API_TOKEN not set; skipping ${artifact}"
	exit 0
fi

if ! require_tools pwsh >/dev/null 2>&1; then
	log "pwsh (PowerShell 7+) is required to sign ${artifact}"
	exit 1
fi

# Hand off to PowerShell; the SignPath module is PowerShell-only. The artifact
# path is passed through the environment to avoid quoting ambiguity. The module
# is expected to be pre-installed in CI (a guarded install covers local runs).
# The single-quoted block holds PowerShell expressions, not shell ones.
# shellcheck disable=SC2016
SIGNPATH_ARTIFACT="$artifact" pwsh -NoProfile -Command '
    Set-StrictMode -Version Latest
    $ErrorActionPreference = "Stop"

    $moduleVersion = "4.3.1"
    $installed = Get-Module -ListAvailable -Name SignPath |
        Where-Object { $_.Version.ToString() -eq $moduleVersion }
    if (-not $installed) {
        Install-Module -Name SignPath -RequiredVersion $moduleVersion -Force -Scope CurrentUser -AcceptLicense
    }
    Import-Module SignPath -RequiredVersion $moduleVersion

    $project = if ($env:SIGNPATH_PROJECT_SLUG) { $env:SIGNPATH_PROJECT_SLUG } else { "sortie" }
    $policy  = if ($env:SIGNPATH_POLICY_SLUG)  { $env:SIGNPATH_POLICY_SLUG }  else { "release" }

    Write-Host "signing $($env:SIGNPATH_ARTIFACT) (project=$project policy=$policy)"
    Submit-SigningRequest `
        -OrganizationId     $env:SIGNPATH_ORG_ID `
        -ProjectSlug        $project `
        -SigningPolicySlug  $policy `
        -InputArtifactPath  $env:SIGNPATH_ARTIFACT `
        -OutputArtifactPath $env:SIGNPATH_ARTIFACT `
        -ApiToken           $env:SIGNPATH_API_TOKEN `
        -WaitForCompletion -Force
'
