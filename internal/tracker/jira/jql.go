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

func buildStatusIN(states []string) string {
	quoted := make([]string, len(states))
	for i, s := range states {
		quoted[i] = fmt.Sprintf(`"%s"`, escapeJQLString(s))
	}
	return strings.Join(quoted, ", ")
}

func buildCandidateJQL(project string, states []string, queryFilter string) string {
	jql := fmt.Sprintf(`project = "%s" AND status IN (%s)`,
		escapeJQLString(project), buildStatusIN(states))
	if queryFilter != "" {
		jql += fmt.Sprintf(" AND (%s)", queryFilter)
	}
	jql += " ORDER BY priority ASC, created ASC"
	return jql
}

// Used by terminal-state cleanup to find issues that have already been
// processed but may still have running sessions to reconcile.
func buildStatesFetchJQL(project string, states []string, queryFilter string) string {
	jql := fmt.Sprintf(`project = "%s" AND status IN (%s)`,
		escapeJQLString(project), buildStatusIN(states))
	if queryFilter != "" {
		jql += fmt.Sprintf(" AND (%s)", queryFilter)
	}
	jql += " ORDER BY created ASC"
	return jql
}

// queryFilter is intentionally omitted — these issues already passed
// filtering at dispatch time.
func buildKeyINJQL(keys []string) string {
	quoted := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = fmt.Sprintf(`"%s"`, escapeJQLString(k))
	}
	return fmt.Sprintf("key IN (%s) ORDER BY key ASC", strings.Join(quoted, ", "))
}

// Non-numeric IDs are skipped so caller bugs (e.g. passing a Jira key
// instead of the numeric ID) surface as missing results rather than
// querying the wrong issue. Returns an empty string when no valid IDs
// remain, letting the caller short-circuit.
func buildIDINJQL(ids []string) string {
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if isNumericID(trimmed) {
			valid = append(valid, trimmed)
		}
	}
	if len(valid) == 0 {
		return ""
	}
	return fmt.Sprintf("id IN (%s) ORDER BY key ASC", strings.Join(valid, ", "))
}

func isNumericID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
