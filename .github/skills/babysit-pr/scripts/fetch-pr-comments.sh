#!/usr/bin/env bash
# fetch-pr-comments.sh — collect every reviewer comment for a GitHub PR.
#
# GitHub serves PR review feedback through three distinct endpoints:
#   1. Inline line-anchored code comments   (/pulls/{N}/comments)
#   2. Review bodies (approve/reject/comment)  (/pulls/{N}/reviews)
#   3. Issue-level conversation comments   (gh pr view --json comments)
# Missing any one of them silently drops a class of feedback and
# corrupts Step 1 of the babysit-pr protocol. This script wraps all
# three calls so the protocol cannot regress.
#
# Usage:
#   fetch-pr-comments.sh [PR_NUMBER]
#
# If PR_NUMBER is omitted, the script resolves the PR associated with
# the current branch via `gh pr view --json number`.
#
# Output (to stdout, single JSON object):
#   {
#     "pr":      <number>,
#     "inline":  [...],   # /pulls/{N}/comments
#     "reviews": [...],   # /pulls/{N}/reviews
#     "issue":   [...]    # issue-level conversation comments
#   }
#
# Exit codes:
#   0  success
#   1  prerequisite missing (gh CLI not found, not authenticated)
#   2  no PR resolvable (no PR_NUMBER given and no PR on current branch)
#   3  gh API call failed

set -euo pipefail

if ! command -v gh >/dev/null 2>&1; then
  echo "Error: gh CLI not found on PATH" >&2
  exit 1
fi

if ! gh auth status >/dev/null 2>&1; then
  echo "Error: gh is not authenticated. Run 'gh auth login'." >&2
  exit 1
fi

PR="${1:-}"
if [[ -z "${PR}" ]]; then
  PR=$(gh pr view --json number --jq '.number' 2>/dev/null || true)
  if [[ -z "${PR}" ]]; then
    echo "Error: no PR on current branch. Pass PR number explicitly." >&2
    exit 2
  fi
fi

REPO=$(gh repo view --json nameWithOwner --jq '.nameWithOwner')

fetch_or_fail() {
  local endpoint="$1"
  local label="$2"
  if ! gh api "${endpoint}" --paginate 2>/dev/null; then
    echo "Error: failed to fetch ${label} (${endpoint})" >&2
    exit 3
  fi
}

INLINE=$(fetch_or_fail "repos/${REPO}/pulls/${PR}/comments" "inline comments")
REVIEWS=$(fetch_or_fail "repos/${REPO}/pulls/${PR}/reviews" "review bodies")
ISSUE=$(gh pr view "${PR}" --json comments --jq '.comments' 2>/dev/null || {
  echo "Error: failed to fetch issue-level comments" >&2
  exit 3
})

# Paginated gh api output concatenates JSON arrays across pages as
# separate top-level arrays. Normalize with jq when available; fall
# back to raw concatenation otherwise (the JSON may require manual
# parsing in that case, so prefer to have jq installed).
if command -v jq >/dev/null 2>&1; then
  jq -n \
    --arg pr "${PR}" \
    --argjson inline "$(echo "${INLINE}" | jq -s 'add // []')" \
    --argjson reviews "$(echo "${REVIEWS}" | jq -s 'add // []')" \
    --argjson issue "${ISSUE}" \
    '{pr: ($pr|tonumber), inline: $inline, reviews: $reviews, issue: $issue}'
else
  echo "{\"pr\": ${PR}, \"inline\": ${INLINE}, \"reviews\": ${REVIEWS}, \"issue\": ${ISSUE}}"
fi
