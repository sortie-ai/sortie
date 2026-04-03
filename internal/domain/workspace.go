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
}
