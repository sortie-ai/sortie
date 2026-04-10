package domain

// SCMMetadata represents the workspace SCM metadata written by the
// agent or hook to .sortie/scm.json. The orchestrator reads this to
// determine the git ref for CI status queries and other SCM-related
// operations.
//
// The struct grows additively as new SCM features are added.
type SCMMetadata struct {
	// Branch is the branch name (e.g. "feature/PROJ-42").
	Branch string `json:"branch"`

	// SHA is the commit SHA at push time. When present, the
	// orchestrator passes this to [CIStatusProvider.FetchCIStatus]
	// instead of the branch name for deterministic results.
	SHA string `json:"sha,omitempty"`

	// PushedAt is an ISO-8601 timestamp of the push. Used by the
	// orchestrator to skip CI checks for stale pushes.
	PushedAt string `json:"pushed_at,omitempty"`

	// PRNumber is the pull request number associated with this branch.
	// Zero when no PR has been created. Written by the agent or
	// post-push hook. The orchestrator uses this to query review
	// comments. Existing scm.json files without this field decode to
	// zero; review polling is skipped when PRNumber is 0.
	PRNumber int `json:"pr_number,omitempty"`

	// Owner is the SCM repository owner (e.g. GitHub org or user).
	// Written by the agent alongside PRNumber. Required for review
	// comment polling; when empty, review polling is skipped.
	Owner string `json:"owner,omitempty"`

	// Repo is the SCM repository name. Written by the agent alongside
	// PRNumber. Required for review comment polling; when empty,
	// review polling is skipped.
	Repo string `json:"repo,omitempty"`
}
