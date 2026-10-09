#!/bin/sh
# Replace a freshly built Windows binary with its SignPath-signed copy.
#
# Invoked by GoReleaser's post-build hook once per build target. The release
# workflow builds once, has SignPath sign the Windows binaries, then runs
# GoReleaser again; only GoReleaser Pro can import prebuilt binaries, so the
# second build's .exe is swapped for the signed one here, before archiving and
# checksums. Without SIGNED_WINDOWS_DIR (dry runs, snapshots, local
# builds) every target passes through untouched.
#
# Usage: swap-signed.sh <path-to-binary> <os>_<arch>
# Input (environment): SIGNED_WINDOWS_DIR holds <os>_<arch>/sortie.exe

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"

artifact=${1:?usage: swap-signed.sh <path-to-binary> <os>_<arch>}
target=${2:?usage: swap-signed.sh <path-to-binary> <os>_<arch>}

case "$artifact" in
*.exe) ;;
*) exit 0 ;;
esac

if [ -z "${SIGNED_WINDOWS_DIR:-}" ]; then
	exit 0
fi

signed="${SIGNED_WINDOWS_DIR}/${target}/sortie.exe"
if [ ! -f "$signed" ]; then
	log "signed binary not found: ${signed}"
	exit 1
fi

cp "$signed" "$artifact"
log "using signed ${signed} for ${artifact}"
