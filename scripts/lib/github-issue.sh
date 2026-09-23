# shellcheck shell=sh
# shellcheck disable=SC2016
# Issue operations shared across the scripts that manage GitHub issues.
# Sourced, never executed.

ensure_label() {
	gh label create "$1" --color "$2" --description "$3" --force >/dev/null 2>&1 || true
}

label_node_id() {
	_lni_name=$(printf '%s' "$1" | jq -sRr @uri)
	gh api "repos/${GITHUB_REPOSITORY}/labels/${_lni_name}" --jq .node_id
}

# create_issue creates an issue titled title, with body read from
# body_file, of issue type issue_type_id, carrying every label named in
# the remaining arguments, and prints the new issue number.
create_issue() {
	_ci_title=$1
	_ci_body_file=$2
	_ci_issue_type_id=$3
	shift 3

	_ci_repository_id=$(gh api "repos/${GITHUB_REPOSITORY}" --jq .node_id)

	_ci_have_label=0
	for _ci_label in "$@"; do
		_ci_label_id=$(label_node_id "$_ci_label")
		if [ "$_ci_have_label" -eq 0 ]; then
			set -- -f "labels[]=${_ci_label_id}"
			_ci_have_label=1
		else
			set -- "$@" -f "labels[]=${_ci_label_id}"
		fi
	done
	[ "$_ci_have_label" -eq 1 ] || set --

	gh api graphql \
		-f query='mutation($repository: ID!, $title: String!, $body: String!, $type: ID!, $labels: [ID!]!) { createIssue(input: {repositoryId: $repository, title: $title, body: $body, issueTypeId: $type, labelIds: $labels}) { issue { number } } }' \
		-f repository="$_ci_repository_id" \
		-f title="$_ci_title" \
		-F body="@${_ci_body_file}" \
		-f type="$_ci_issue_type_id" \
		"$@" \
		--jq .data.createIssue.issue.number
}

set_issue_type() {
	_sit_number=$1
	_sit_issue_type_id=$2
	_sit_issue_id=$(gh api "repos/${GITHUB_REPOSITORY}/issues/${_sit_number}" --jq .node_id)
	gh api graphql \
		-f query='mutation($issue: ID!, $type: ID!) { updateIssue(input: {id: $issue, issueTypeId: $type}) { issue { number } } }' \
		-f issue="$_sit_issue_id" \
		-f type="$_sit_issue_type_id" >/dev/null
}

# list_labeled_issues lists every issue labeled label, newest created
# first, paginated to exhaustion, appending "<number>\t<state>\t<title>"
# per issue to out_file. It uses the issues REST route rather than the
# search route: the search index lags issue creation by an unbounded
# interval, and a durable report a search misses would file as a
# duplicate. It returns non-zero on a non-zero gh exit, leaving out_file
# holding whatever earlier pages already wrote.
list_labeled_issues() {
	_lli_label=$1
	_lli_out=$2
	: >"$_lli_out"
	_lli_page=1
	while :; do
		_lli_page_json=$(gh api "repos/${GITHUB_REPOSITORY}/issues?state=all&labels=${_lli_label}&sort=created&direction=desc&per_page=100&page=${_lli_page}") || return 1
		printf '%s' "$_lli_page_json" | jq -r \
			'.[] | select(.pull_request == null) | "\(.number)\t\(.state)\t\(.title)"' >>"$_lli_out"
		_lli_count=$(printf '%s' "$_lli_page_json" | jq -r 'length')
		if [ "$_lli_count" -lt 100 ]; then
			return 0
		fi
		_lli_page=$((_lli_page + 1))
	done
}
