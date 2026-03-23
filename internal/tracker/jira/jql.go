package jira

import (
	"fmt"
	"strings"
)

// escapeJQLString removes double-quote characters from a string value
// before it is interpolated into a JQL query. JQL string literals are
// delimited by double quotes; embedded quotes would break the query
// syntax. Backslash-escaping is not supported by JQL for string
// literals, so removal is the safe approach.
func escapeJQLString(s string) string {
	return strings.ReplaceAll(s, `"`, "")
}

// buildStatusIN formats a list of status names as a JQL IN clause
// value. Each status is sanitized and double-quoted.
func buildStatusIN(states []string) string {
	quoted := make([]string, len(states))
	for i, s := range states {
		quoted[i] = fmt.Sprintf(`"%s"`, escapeJQLString(s))
	}
	return strings.Join(quoted, ", ")
}

// buildCandidateJQL builds the JQL for fetching candidate issues in
// active states, ordered by priority then creation time.
func buildCandidateJQL(project string, states []string, queryFilter string) string {
	jql := fmt.Sprintf(`project = "%s" AND status IN (%s)`,
		escapeJQLString(project), buildStatusIN(states))
	if queryFilter != "" {
		jql += fmt.Sprintf(" AND (%s)", queryFilter)
	}
	jql += " ORDER BY priority ASC, created ASC"
	return jql
}

// buildStatesFetchJQL builds the JQL for fetching issues by states,
// ordered by creation time. Used for terminal cleanup.
func buildStatesFetchJQL(project string, states []string, queryFilter string) string {
	jql := fmt.Sprintf(`project = "%s" AND status IN (%s)`,
		escapeJQLString(project), buildStatusIN(states))
	if queryFilter != "" {
		jql += fmt.Sprintf(" AND (%s)", queryFilter)
	}
	jql += " ORDER BY created ASC"
	return jql
}

// buildKeyINJQL builds the JQL for fetching issues by their keys
// (human-readable identifiers like "PROJ-123").
// The queryFilter is intentionally not applied — these issues already
// passed filtering at dispatch time.
func buildKeyINJQL(keys []string) string {
	quoted := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = fmt.Sprintf(`"%s"`, escapeJQLString(k))
	}
	return fmt.Sprintf("key IN (%s) ORDER BY key ASC", strings.Join(quoted, ", "))
}

// buildIDINJQL builds JQL for fetching issues by their internal numeric
// IDs. Each ID is sanitized to digits only to prevent JQL injection.
// Used by [FetchIssueStatesByIDs] for reconciliation and worker state
// refresh where state.Running is keyed by numeric ID.
func buildIDINJQL(ids []string) string {
	sanitized := make([]string, 0, len(ids))
	for _, id := range ids {
		clean := stripNonDigits(id)
		if clean != "" {
			sanitized = append(sanitized, clean)
		}
	}
	return fmt.Sprintf("id IN (%s) ORDER BY key ASC", strings.Join(sanitized, ", "))
}

// stripNonDigits removes all non-digit characters from a string.
func stripNonDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
