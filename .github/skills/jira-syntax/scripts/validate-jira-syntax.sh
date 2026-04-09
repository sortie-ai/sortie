#!/usr/bin/env sh
# Jira wiki markup syntax validator
# Checks files for common Markdown-in-Jira mistakes and unclosed block macros.
#
# Usage: sh validate-jira-syntax.sh <file> [file ...]
# Exit codes: 0 = pass (warnings allowed), 1 = errors found, 2 = usage error

# --- output helpers --------------------------------------------------------

RED='\033[0;31m'
YELLOW='\033[1;33m'
GREEN='\033[0;32m'
NC='\033[0m'

ERRORS=0
WARNINGS=0

error() {
  printf '%b\n' "${RED}ERROR:${NC} $1"
  ERRORS=$((ERRORS + 1))
}

warn() {
  printf '%b\n' "${YELLOW}WARNING:${NC} $1"
  WARNINGS=$((WARNINGS + 1))
}

ok() {
  printf '%b\n' "${GREEN}OK:${NC} $1"
}

# --- per-file checks -------------------------------------------------------

validate_file() {
  file="$1"

  printf '\n--- %s ---\n' "$file"

  if [ ! -f "$file" ]; then
    error "File not found: $file"
    return
  fi

  # Markdown headings (## instead of h2.)
  if grep -qE '^##+ ' "$file"; then
    error "Markdown headings found (##). Use: h2. Heading"
    grep -nE '^##+ ' "$file" | head -5 | sed 's/^/  /'
  fi

  # Markdown bold (**text**)
  if grep -qE '\*\*[^*]+\*\*' "$file"; then
    error "Markdown bold found (**text**). Use: *text*"
    grep -nE '\*\*[^*]+\*\*' "$file" | head -3 | sed 's/^/  /'
  fi

  # Markdown code fences (```)
  if grep -qE '^```' "$file"; then
    error "Markdown code fences found (\`\`\`). Use: {code:language}...{code}"
    grep -nE '^```' "$file" | head -5 | sed 's/^/  /'
  fi

  # Markdown links ([text](url))
  if grep -qE '\[[^]]+\]\([^)]+\)' "$file"; then
    error "Markdown links found ([text](url)). Use: [text|url]"
    grep -nE '\[[^]]+\]\([^)]+\)' "$file" | head -3 | sed 's/^/  /'
  fi

  # Markdown bullets (- item)
  if grep -qE '^- [^-]' "$file"; then
    warn "Markdown bullets found (- item). Jira uses: * item"
  fi

  # Headings without space after period (h2.Title)
  if grep -qE '^h[1-6]\.[^ ]' "$file"; then
    error "Heading missing space after period. Use: h2. Title"
    grep -nE '^h[1-6]\.[^ ]' "$file" | head -5 | sed 's/^/  /'
  fi

  # Code blocks without language ({code} used as opening tag instead of {code:lang}).
  # Closing tags are also {code}, so only warn when bare {code} count exceeds
  # {code:lang} count — meaning at least one block lacks a language.
  bare_code=$(grep -cE '^\{code\}$' "$file" || true)
  lang_code=$(grep -cE '^\{code:[a-zA-Z]+\}' "$file" || true)
  if [ "$bare_code" -gt "$lang_code" ]; then
    warn "Code block without language identifier. Use: {code:python}"
  fi

  # Unclosed {code} blocks — count all lines containing {code and check for even count.
  # Each code block pair has an opening {code:lang} and closing {code}.
  code_tags=$(grep -c '{code' "$file" || true)
  if [ "$((code_tags % 2))" -ne 0 ]; then
    error "Odd number of {code} tags ($code_tags). Likely unclosed code block."
  fi

  # Unclosed {panel} blocks
  panel_tags=$(grep -c '{panel' "$file" || true)
  if [ "$((panel_tags % 2))" -ne 0 ]; then
    error "Odd number of {panel} tags ($panel_tags). Likely unclosed panel."
  fi

  # Unclosed {color} blocks
  color_tags=$(grep -c '{color' "$file" || true)
  if [ "$((color_tags % 2))" -ne 0 ]; then
    warn "Odd number of {color} tags ($color_tags). Likely unclosed color block."
  fi

  # Table headers with single pipe instead of double
  if grep -qE '^\|[^|]+\|$' "$file" && ! grep -qE '^\|\|' "$file"; then
    warn "Table rows found but no header rows (||Header||). Verify table formatting."
  fi

  # --- positive feedback ---
  if grep -qE '^h[1-6]\. ' "$file"; then
    ok "Correctly formatted Jira headings"
  fi

  if grep -qE '\{code:[a-zA-Z]+\}' "$file"; then
    ok "Code blocks with language identifiers"
  fi

  if grep -qE '\[~[a-zA-Z._-]+\]' "$file"; then
    ok "User mentions ([~username])"
  fi

  if grep -qE '\[[A-Z]+-[0-9]+\]' "$file"; then
    ok "Issue links ([PROJ-123])"
  fi
}

# --- main ------------------------------------------------------------------

if [ $# -eq 0 ]; then
  printf 'Usage: %s <file> [file ...]\n\n' "$0"
  printf 'Validate Jira wiki markup syntax and flag Markdown mistakes.\n'
  exit 2
fi

for file in "$@"; do
  validate_file "$file"
done

printf '\n--- Summary ---\n'
printf 'Files checked: %d\n' "$#"
printf '%b\n' "${RED}Errors: ${ERRORS}${NC}"
printf '%b\n' "${YELLOW}Warnings: ${WARNINGS}${NC}"

if [ "$ERRORS" -eq 0 ] && [ "$WARNINGS" -eq 0 ]; then
  printf '%b\n' "${GREEN}All checks passed.${NC}"
  exit 0
elif [ "$ERRORS" -eq 0 ]; then
  printf '%b\n' "${YELLOW}No errors. ${WARNINGS} warning(s).${NC}"
  exit 0
else
  printf '%b\n' "${RED}${ERRORS} error(s) found. Fix before submitting to Jira.${NC}"
  exit 1
fi
