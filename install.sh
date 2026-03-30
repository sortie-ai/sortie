#!/bin/sh
# Installer for sortie — spec-first agent orchestrator.
#
# Usage:
#   curl -sSL https://get.sortie-ai.com/install.sh | sh
#
# Environment:
#   SORTIE_VERSION      Pin a specific release tag (e.g. v1.0.0 or 0.0.7).
#   SORTIE_INSTALL_DIR  Override install directory
#                       (default: /usr/local/bin as root, ~/.local/bin otherwise).
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
    _want=$(awk -v f="$(basename "$_file")" '$2 == f {print $1}' "$_sums")
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

# ── Install directory resolution ──────────────────────────────────────────────

resolve_install_dir() {
    if [ -n "${SORTIE_INSTALL_DIR-}" ]; then
        printf '%s' "$SORTIE_INSTALL_DIR"
        return
    fi

    # Root (e.g. Docker) → /usr/local/bin, the FHS standard for local binaries.
    if [ "$(id -u)" -eq 0 ]; then
        printf '%s' "/usr/local/bin"
        return
    fi

    # Non-root → ~/.local/bin (XDG convention, same as pip, mise, pipx).
    printf '%s' "${HOME}/.local/bin"
}

# Detect rc file for the current shell so PATH hint is copy-pasteable.
shell_rc() {
    case "${SHELL-}" in
        */zsh)  printf '%s' "${ZDOTDIR:-$HOME}/.zshrc"  ;;
        */bash) printf '%s' "${HOME}/.bashrc"            ;;
        */fish) printf '%s' "${HOME}/.config/fish/config.fish" ;;
        *)      printf '%s' "${HOME}/.profile"           ;;
    esac
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

    _dir=$(resolve_install_dir)
    mkdir -p "$_dir"
    install -m 755 "${TMPDIR_INSTALL}/${BIN}" "${_dir}/${BIN}"

    ok "Installed ${BIN} ${_tag} to ${_dir}/${BIN}"

    case ":${PATH}:" in
        *":${_dir}:"*) ;;
        *)
            _rc=$(shell_rc)
            printf '\n'
            info "Add to your PATH to get started:"
            # shellcheck disable=SC2016
            printf '  %becho '\''export PATH="%s:$PATH"'\'' >> %s%b\n\n' \
                "${DIM}" "$_dir" "$_rc" "${RESET}"
            ;;
    esac

    printf '\n'

    _utf8=false
    case "${LC_ALL:-${LC_CTYPE:-${LANG:-}}}" in
        *[Uu][Tt][Ff]8*|*[Uu][Tt][Ff]-8*) _utf8=true ;;
    esac

    # Skip decorative art in CI pipelines and non-interactive terminals.
    printf '\n'
    if [ -z "${CI-}" ] && [ -t 1 ] && [ "${TERM-}" != "dumb" ]; then
        if [ "$_utf8" = "true" ]; then
            printf '\033[36m    █████████                      █████     ███\n'
            printf '   ███░░░░░███                    ░░███     ░░░\n'
            printf '  ░███    ░░░   ██████  ████████  ███████   ████   ██████\n'
            printf '  ░░█████████  ███░░███░░███░░███░░░███░   ░░███  ███░░███\n'
            printf '   ░░░░░░░░███░███ ░███ ░███ ░░░   ░███     ░███ ░███████\n'
            printf '   ███    ░███░███ ░███ ░███       ░███ ███ ░███ ░███░░░\n'
            printf '  ░░█████████ ░░██████  █████      ░░█████  █████░░██████\n'
            printf '   ░░░░░░░░░   ░░░░░░  ░░░░░        ░░░░░  ░░░░░  ░░░░░░\033[0m\n'
        else
            printf '  Sortie\n'
        fi
    fi

    printf '\n  %bDocs:%b       https://docs.sortie-ai.com\n' "${DIM}" "${RESET}"
    printf '  %bChangelog:%b  https://docs.sortie-ai.com/changelog/#%s\n' "${DIM}" "${RESET}" "$_version"
    printf '  %bGitHub:%b     https://github.com/%s\n' "${DIM}" "${RESET}" "$REPO"
    if [ "$_utf8" = "true" ]; then
        printf '\n  Happy hacking! %b♠%b\n\n' "${CYAN}" "${RESET}"
    else
        printf '\n  Happy hacking!\n\n'
    fi
}

main "$@"
