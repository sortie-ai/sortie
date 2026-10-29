#!/bin/sh
# Print a short-lived Anthropic access token obtained through Workload Identity
# Federation from the GitHub Actions OIDC token of the running job.
#
# Runs as Claude Code's apiKeyHelper, which calls it again before the access
# token expires; each call fetches a fresh GitHub token because the federation
# issuer accepts every token only once.

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"

require_tools curl jq
require_env ACTIONS_ID_TOKEN_REQUEST_URL ACTIONS_ID_TOKEN_REQUEST_TOKEN \
	ANTHROPIC_FEDERATION_RULE_ID ANTHROPIC_ORGANIZATION_ID \
	ANTHROPIC_SERVICE_ACCOUNT_ID ANTHROPIC_WORKSPACE_ID

jwt=$(curl -fsS --retry 3 \
	-H "Authorization: Bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
	"${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=https://api.anthropic.com" |
	jq -er '.value // empty')

request=$(jq -n \
	--arg assertion "$jwt" \
	--arg rule "$ANTHROPIC_FEDERATION_RULE_ID" \
	--arg organization "$ANTHROPIC_ORGANIZATION_ID" \
	--arg account "$ANTHROPIC_SERVICE_ACCOUNT_ID" \
	--arg workspace "$ANTHROPIC_WORKSPACE_ID" \
	'{
		grant_type: "urn:ietf:params:oauth:grant-type:jwt-bearer",
		assertion: $assertion,
		federation_rule_id: $rule,
		organization_id: $organization,
		service_account_id: $account,
		workspace_id: $workspace
	}')

response=$(printf '%s' "$request" |
	curl -sS --fail-with-body https://api.anthropic.com/v1/oauth/token \
		-H 'content-type: application/json' \
		--data-binary @-) || {
	log "token exchange failed: ${response}"
	exit 1
}

printf '%s' "$response" | jq -er '.access_token // empty'
