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

artifact=${1:?usage: signpath-sign.sh <path-to-binary>}

# Only Authenticode-sign Windows PE executables.
case "$artifact" in
    *.exe) ;;
    *) exit 0 ;;
esac

# Skip when credentials are absent (snapshot / pull-request / local builds).
if [ -z "${SIGNPATH_API_TOKEN:-}" ] || [ -z "${SIGNPATH_ORG_ID:-}" ]; then
    echo "signpath-sign: SIGNPATH_ORG_ID/SIGNPATH_API_TOKEN not set; skipping ${artifact}" >&2
    exit 0
fi

if ! command -v pwsh >/dev/null 2>&1; then
    echo "signpath-sign: pwsh (PowerShell 7+) is required to sign ${artifact}" >&2
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

    if (-not (Get-Module -ListAvailable -Name SignPath)) {
        Install-Module -Name SignPath -Force -Scope CurrentUser -AcceptLicense
    }
    Import-Module SignPath

    $project = if ($env:SIGNPATH_PROJECT_SLUG) { $env:SIGNPATH_PROJECT_SLUG } else { "sortie" }
    $policy  = if ($env:SIGNPATH_POLICY_SLUG)  { $env:SIGNPATH_POLICY_SLUG }  else { "release" }

    Write-Host "signpath-sign: signing $($env:SIGNPATH_ARTIFACT) (project=$project policy=$policy)"
    Submit-SigningRequest `
        -OrganizationId     $env:SIGNPATH_ORG_ID `
        -ProjectSlug        $project `
        -SigningPolicySlug  $policy `
        -InputArtifactPath  $env:SIGNPATH_ARTIFACT `
        -OutputArtifactPath $env:SIGNPATH_ARTIFACT `
        -ApiToken           $env:SIGNPATH_API_TOKEN `
        -WaitForCompletion -Force
'
