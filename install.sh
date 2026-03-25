#!/bin/sh
# Installer for sortie — spec-first agent orchestrator.
#
# Usage:
#   curl -sSL https://get.sortie-ai.com/install.sh | sh
#
# Environment:
#   SORTIE_VERSION      Pin a specific release tag (e.g. v1.0.0 or 0.0.7).
#   SORTIE_INSTALL_DIR  Override install directory  (default: ~/.local/bin).
#   SORTIE_NO_VERIFY    Set to 1 to skip checksum verification.

set -eu

REPO="sortie-ai/sortie"
BIN="sortie"

# ── Formatting ────────────────────────────────────────────────────────────────

setup_colors() {
    if [ -t 1 ] && [ "${TERM-}" != "dumb" ]; then
        BOLD='\033[1m'  DIM='\033[2m'
        RED='\033[31m'  GREEN='\033[32m'  CYAN='\033[36m'
        RESET='\033[0m'
    else
        BOLD=''  DIM=''  RED=''  GREEN=''  CYAN=''  RESET=''
    fi
}

info() { printf '%b%s\n' "${BOLD}${CYAN}:: ${RESET}" "$*"; }
ok()   { printf '%b%s\n' "${BOLD}${GREEN}:: ${RESET}" "$*"; }
err()  { printf '%b%s\n' "${BOLD}${RED}error: ${RESET}" "$*" >&2; }
die()  { err "$@"; exit 1; }

# ── Platform detection ────────────────────────────────────────────────────────

detect_platform() {
    OS=$(uname -s)
    case "$OS" in
        Linux*)  OS=linux  ;;
        Darwin*) OS=darwin ;;
        *)       die "unsupported OS: $OS" ;;
    esac

    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64|amd64)  ARCH=amd64 ;;
        aarch64|arm64) ARCH=arm64 ;;
        *)             die "unsupported architecture: $ARCH" ;;
    esac
}

# ── HTTP abstraction ──────────────────────────────────────────────────────────

need_cmd() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }

fetch() {
    _url=$1 _out=${2:-}
    if command -v curl >/dev/null 2>&1; then
        if [ -n "$_out" ]; then curl -fsSL -o "$_out" "$_url"
        else                     curl -fsSL "$_url"; fi
    elif command -v wget >/dev/null 2>&1; then
        if [ -n "$_out" ]; then wget -qO "$_out" "$_url"
        else                     wget -qO- "$_url"; fi
    else
        die "curl or wget is required"
    fi
}

# ── Version resolution ────────────────────────────────────────────────────────

resolve_tag() {
    if [ -n "${SORTIE_VERSION-}" ]; then
        printf '%s' "$SORTIE_VERSION"
        return
    fi
    _json=$(fetch "https://api.github.com/repos/${REPO}/releases/latest") \
        || die "GitHub API request failed (rate-limited? set SORTIE_VERSION to skip)"
    printf '%s' "$_json" \
        | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
        | head -n1
}

# ── Checksum verification ────────────────────────────────────────────────────

verify_checksum() {
    _file=$1 _sums=$2
    _want=$(grep "$(basename "$_file")" "$_sums" | awk '{print $1}')
    [ -n "$_want" ] || die "no checksum entry for $(basename "$_file")"

    if command -v sha256sum >/dev/null 2>&1; then
        _got=$(sha256sum "$_file" | awk '{print $1}')
    elif command -v shasum >/dev/null 2>&1; then
        _got=$(shasum -a 256 "$_file" | awk '{print $1}')
    else
        die "sha256sum or shasum is required"
    fi

    [ "$_want" = "$_got" ] \
        || die "checksum mismatch (expected ${_want}, got ${_got})"
}

# ── Cleanup ───────────────────────────────────────────────────────────────────

cleanup() { [ -d "${TMPDIR_INSTALL-}" ] && rm -rf "$TMPDIR_INSTALL"; }

# ── Main ──────────────────────────────────────────────────────────────────────

main() {
    setup_colors
    need_cmd uname
    need_cmd tar

    detect_platform
    info "Platform: ${OS}/${ARCH}"

    _tag=$(resolve_tag)
    [ -n "$_tag" ] || die "could not determine latest release"
    _version=$(printf '%s' "$_tag" | sed 's/^v//')
    info "Release:  ${_tag}"

    _archive="${BIN}_${_version}_${OS}_${ARCH}.tar.gz"
    _base="https://github.com/${REPO}/releases/download/${_tag}"

    TMPDIR_INSTALL=$(mktemp -d)
    trap cleanup EXIT
    trap 'exit 1' INT TERM

    info "Downloading ${_archive}"
    fetch "${_base}/${_archive}" "${TMPDIR_INSTALL}/${_archive}" \
        || die "download failed — verify release ${_tag} has asset for ${OS}/${ARCH}"

    if [ "${SORTIE_NO_VERIFY-}" != "1" ]; then
        fetch "${_base}/checksums.txt" "${TMPDIR_INSTALL}/checksums.txt" \
            || die "failed to download checksums"
        verify_checksum "${TMPDIR_INSTALL}/${_archive}" "${TMPDIR_INSTALL}/checksums.txt"
        info "Checksum verified"
    fi

    tar -xzf "${TMPDIR_INSTALL}/${_archive}" -C "${TMPDIR_INSTALL}"

    _dir="${SORTIE_INSTALL_DIR:-${HOME}/.local/bin}"
    mkdir -p "$_dir"
    install -m 755 "${TMPDIR_INSTALL}/${BIN}" "${_dir}/${BIN}"

    ok "Installed ${BIN} ${_tag} to ${_dir}/${BIN}"

    case ":${PATH}:" in
        *":${_dir}:"*) ;;
        *)
            printf '\n'
            info "Add to your PATH to get started:"
            # shellcheck disable=SC2016
            printf '  %bexport PATH="%s:$PATH"%b\n\n' "${DIM}" "$_dir" "${RESET}"
            ;;
    esac

    printf '\n  %bDocs:%b       https://docs.sortie-ai.com\n' "${DIM}" "${RESET}"
    printf '  %bChangelog:%b  https://github.com/%s/blob/%s/CHANGELOG.md\n' "${DIM}" "${RESET}" "$REPO" "$_tag"
    printf '\n  Happy hacking! %b♠%b\n\n' "${CYAN}" "${RESET}"
}

main "$@"
