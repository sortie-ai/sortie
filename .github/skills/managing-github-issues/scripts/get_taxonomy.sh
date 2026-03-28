#!/usr/bin/env bash
# get_taxonomy.sh — Fetch live labels, milestones, and project board info.
#
# Usage: bash .github/skills/managing-github-issues/scripts/get_taxonomy.sh
#
# Outputs a structured summary for the agent to use when composing
# gh issue create/edit commands. All values are fetched from the GitHub API
# so they are always current.
#
# Exit codes:
#   0 — success
#   1 — gh CLI not found or not authenticated

set -euo pipefail

if ! command -v gh &>/dev/null; then
  echo "ERROR: gh CLI not found. Install from https://cli.github.com/" >&2
  exit 1
fi

if ! gh auth status &>/dev/null 2>&1; then
  echo "ERROR: gh not authenticated. Run: gh auth login" >&2
  exit 1
fi

# Detect repo owner/name from git remote (works inside the repo)
REPO=$(gh repo view --json nameWithOwner -q '.nameWithOwner' 2>/dev/null || true)
if [[ -z "$REPO" ]]; then
  echo "ERROR: not inside a GitHub repository" >&2
  exit 1
fi

echo "=== TAXONOMY for ${REPO} ==="
echo ""

# --- Issue Types ---
echo "--- ISSUE TYPES ---"
echo ""

OWNER="${REPO%%/*}"
gh api "/orgs/${OWNER}/issue-types" \
  --jq '.[] | "\(.name)\t\(.description)"' 2>/dev/null \
  | sort | while IFS=$'\t' read -r name desc; do
  printf "  %-14s  %s\n" "$name" "$desc"
done || echo "  (issue types not available)"

echo ""

# --- Labels ---
echo "--- LABELS ---"
echo ""

# Fetch all labels and group by prefix
gh label list --limit 100 --json name,description \
  --jq '.[] | "\(.name)\t\(.description)"' | sort | while IFS=$'\t' read -r name desc; do
  prefix="${name%%:*}"
  if [[ "$prefix" != "$name" ]]; then
    printf "  %-28s  %s\n" "$name" "$desc"
  else
    printf "  %-28s  %s\n" "$name" "$desc"
  fi
done

echo ""

# --- Milestones ---
echo "--- MILESTONES (open) ---"
echo ""

gh api "repos/${REPO}/milestones" --paginate \
  -q '.[] | select(.state=="open") | "  \(.title)  [open: \(.open_issues), closed: \(.closed_issues)]"' \
  2>/dev/null || echo "  (none)"

echo ""

# Also show closed milestones count for context
closed_count=$(gh api "repos/${REPO}/milestones?state=closed" --paginate \
  -q '. | length' 2>/dev/null || echo "0")
echo "  (${closed_count} closed milestones omitted)"
echo ""

# --- Project board ---
echo "--- PROJECT BOARD ---"
echo ""

gh project list --owner "${OWNER}" --limit 5 --format json \
  --jq '.projects[] | "  \(.title) (#\(.number), \(.url))"' \
  2>/dev/null || echo "  (no projects found)"

echo ""
echo "=== END ==="
