package gitlab

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// skipUnlessIntegration skips the current test when SORTIE_GITLAB_TEST is
// not "1", so disabled integration tests report as skipped rather than
// passing silently or hitting the network.
func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_GITLAB_TEST") != "1" {
		t.Skip("skipping GitLab integration test: set SORTIE_GITLAB_TEST=1 to enable")
	}
}

// requireEnv reads an environment variable and fails the test when empty.
func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

// optionalEnv reads an environment variable and skips the test when empty,
// naming the variable and the fixture it gates. It is a precondition helper,
// not a second integration gate: it never reads SORTIE_GITLAB_TEST.
func optionalEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("optional environment variable %s is not set; skipping the fixture it gates", key)
	}
	return v
}

// activeStates returns the SORTIE_GITLAB_ACTIVE_STATES override when set,
// else the adapter's own defaultActiveStates, so a change to the adapter's
// defaults cannot silently desynchronize the suite.
func activeStates(t *testing.T) []string {
	t.Helper()
	if raw := os.Getenv("SORTIE_GITLAB_ACTIVE_STATES"); raw != "" {
		return splitStates(raw)
	}
	return defaultActiveStates
}

// terminalStates returns the SORTIE_GITLAB_TERMINAL_STATES override when
// set, else the adapter's own defaultTerminalStates.
func terminalStates(t *testing.T) []string {
	t.Helper()
	if raw := os.Getenv("SORTIE_GITLAB_TERMINAL_STATES"); raw != "" {
		return splitStates(raw)
	}
	return defaultTerminalStates
}

// splitStates parses a comma-separated state list into a trimmed, non-empty
// string slice.
func splitStates(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// integrationConfig builds the adapter config from the integration env
// vars.
func integrationConfig(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"api_key":         requireEnv(t, "SORTIE_GITLAB_TOKEN"),
		"project":         requireEnv(t, "SORTIE_GITLAB_PROJECT"),
		"endpoint":        requireEnv(t, "SORTIE_GITLAB_ENDPOINT"),
		"active_states":   activeStates(t),
		"terminal_states": terminalStates(t),
	}
}

// newIntegrationAdapter constructs a live adapter, running the real
// preflight.
func newIntegrationAdapter(t *testing.T) domain.TrackerAdapter {
	t.Helper()
	adapter, err := NewGitLabAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewGitLabAdapter: %v", err)
	}
	return adapter
}

// firstCandidate returns the first candidate issue or skips when the
// project has none, so a write test has a real issue to act on.
func firstCandidate(t *testing.T, ctx context.Context, adapter domain.TrackerAdapter) domain.Issue {
	t.Helper()
	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in project; cannot run write test")
	}
	return candidates[0]
}

// parseCreatedAt parses a comment's CreatedAt so ordering checks compare
// chronological order rather than lexicographic string order.
func parseCreatedAt(t *testing.T, raw string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("CreatedAt %q is not a valid RFC 3339 timestamp: %v", raw, err)
	}
	return parsed
}

func TestIntegration_FetchCandidateIssues(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issues, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	for _, issue := range issues {
		if issue.ID == "" {
			t.Errorf("issue %q: ID is empty", issue.Identifier)
		}
		if issue.Identifier != issue.ID {
			t.Errorf("issue %s: Identifier = %q, want %q", issue.ID, issue.Identifier, issue.ID)
		}
		if issue.Labels == nil {
			t.Errorf("issue %s: Labels is nil, want non-nil slice", issue.Identifier)
		}
		if issue.BlockedBy == nil || len(issue.BlockedBy) != 0 {
			t.Errorf("issue %s: BlockedBy = %v, want non-nil empty slice", issue.Identifier, issue.BlockedBy)
		}
		if issue.Comments != nil {
			t.Errorf("issue %s: Comments = %v, want nil", issue.Identifier, issue.Comments)
		}
		if issue.IssueType != "issue" {
			t.Errorf("issue %s: IssueType = %q, want %q", issue.Identifier, issue.IssueType, "issue")
		}
		if issue.Priority != nil {
			t.Errorf("issue %s: Priority = %v, want nil", issue.Identifier, *issue.Priority)
		}
		if issue.Parent != nil {
			t.Errorf("issue %s: Parent = %v, want nil", issue.Identifier, issue.Parent)
		}
	}

	for i := 1; i < len(issues); i++ {
		prev := parseCreatedAt(t, issues[i-1].CreatedAt)
		curr := parseCreatedAt(t, issues[i].CreatedAt)
		if prev.After(curr) {
			t.Errorf("issues not in non-descending CreatedAt order at index %d: %q before %q",
				i, issues[i-1].CreatedAt, issues[i].CreatedAt)
		}
	}
}

func TestIntegration_FetchCandidateIssues_FixtureFloor(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issues, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	const floor = 2
	if len(issues) < floor {
		t.Fatalf("FetchCandidateIssues returned %d candidates, want at least %d; the fixture is depleted, seed another open issue carrying an active-state label to restore it",
			len(issues), floor)
	}
}

func TestIntegration_FetchIssueByID(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidate := firstCandidate(t, ctx, adapter)

	fetched, err := adapter.FetchIssueByID(ctx, candidate.ID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s): %v", candidate.ID, err)
	}
	if fetched.Identifier != candidate.Identifier {
		t.Errorf("FetchIssueByID(%s) Identifier = %q, want %q", candidate.ID, fetched.Identifier, candidate.Identifier)
	}
	if fetched.Title != candidate.Title {
		t.Errorf("FetchIssueByID(%s) Title = %q, want %q", candidate.ID, fetched.Title, candidate.Title)
	}
	if fetched.Comments == nil {
		t.Error("Comments is nil, want non-nil slice for fully populated issue")
	}
	if fetched.BlockedBy == nil || len(fetched.BlockedBy) != 0 {
		t.Errorf("BlockedBy = %v, want non-nil empty slice", fetched.BlockedBy)
	}
}

func TestIntegration_FetchIssueByID_NotFound(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := adapter.FetchIssueByID(ctx, "999999")
	if err == nil {
		t.Fatal("FetchIssueByID(999999) = nil error, want error")
	}

	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if te.Kind != domain.ErrTrackerNotFound {
		t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, domain.ErrTrackerNotFound)
	}
}

func TestIntegration_FetchIssueByID_WorkItem(t *testing.T) {
	skipUnlessIntegration(t)

	taskIID := optionalEnv(t, "SORTIE_GITLAB_TASK_IID")

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := adapter.FetchIssueByID(ctx, taskIID)
	if err == nil {
		t.Fatalf("FetchIssueByID(%s) = nil error, want error", taskIID)
	}

	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if te.Kind != domain.ErrTrackerNotFound {
		t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, domain.ErrTrackerNotFound)
	}
}

func TestIntegration_FetchIssuesByStates_Empty(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issues, err := adapter.FetchIssuesByStates(ctx, []string{})
	if err != nil {
		t.Fatalf("FetchIssuesByStates(empty): %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("FetchIssuesByStates(empty) returned %d issues, want 0", len(issues))
	}
}

func TestIntegration_FetchIssuesByStates_Active(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in project; cannot determine requested active states")
	}

	requested := make(map[string]struct{})
	states := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := requested[c.State]; ok {
			continue
		}
		requested[c.State] = struct{}{}
		states = append(states, c.State)
	}

	issues, err := adapter.FetchIssuesByStates(ctx, states)
	if err != nil {
		t.Fatalf("FetchIssuesByStates(%v): %v", states, err)
	}
	if issues == nil {
		t.Fatal("FetchIssuesByStates returned nil, want non-nil slice")
	}
	for _, issue := range issues {
		if _, ok := requested[issue.State]; !ok {
			t.Errorf("FetchIssuesByStates(%v): issue %s State = %q, want one of %v", states, issue.Identifier, issue.State, states)
		}
	}
}

func TestIntegration_FetchIssuesByStates_Terminal(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	candidateIDs := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		candidateIDs[c.ID] = struct{}{}
	}

	states := terminalStates(t)
	requested := make(map[string]struct{}, len(states))
	for _, s := range states {
		requested[s] = struct{}{}
	}

	issues, err := adapter.FetchIssuesByStates(ctx, states)
	if err != nil {
		t.Fatalf("FetchIssuesByStates(%v): %v", states, err)
	}
	if issues == nil {
		t.Fatal("FetchIssuesByStates returned nil, want non-nil slice")
	}
	for _, issue := range issues {
		if _, ok := requested[issue.State]; !ok {
			t.Errorf("FetchIssuesByStates(%v): issue %s State = %q, want one of %v", states, issue.Identifier, issue.State, states)
		}
		if _, ok := candidateIDs[issue.ID]; ok {
			t.Errorf("FetchIssuesByStates(%v): issue %s also appears in the active candidate set", states, issue.Identifier)
		}
	}
}

func TestIntegration_FetchIssueStatesByIDs(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in project; cannot test FetchIssueStatesByIDs")
	}

	ids := make([]string, 0, len(candidates)+1)
	for _, c := range candidates {
		ids = append(ids, c.ID)
	}
	ids = append(ids, "999999")

	states, err := adapter.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	for _, c := range candidates {
		got, ok := states[c.ID]
		if !ok {
			t.Errorf("FetchIssueStatesByIDs: candidate %s missing from result", c.ID)
			continue
		}
		if got != c.State {
			t.Errorf("FetchIssueStatesByIDs: state[%s] = %q, want %q", c.ID, got, c.State)
		}
	}
	if _, ok := states["999999"]; ok {
		t.Error("FetchIssueStatesByIDs: nonexistent id 999999 present in result, want absent")
	}
}

func TestIntegration_FetchIssueStatesByIdentifiers(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in project; cannot test FetchIssueStatesByIdentifiers")
	}

	identifiers := make([]string, 0, len(candidates)+1)
	for _, c := range candidates {
		identifiers = append(identifiers, c.Identifier)
	}
	identifiers = append(identifiers, "999999")

	states, err := adapter.FetchIssueStatesByIdentifiers(ctx, identifiers)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}
	for _, c := range candidates {
		got, ok := states[c.Identifier]
		if !ok {
			t.Errorf("FetchIssueStatesByIdentifiers: candidate %s missing from result", c.Identifier)
			continue
		}
		if got != c.State {
			t.Errorf("FetchIssueStatesByIdentifiers: state[%s] = %q, want %q", c.Identifier, got, c.State)
		}
	}
	if _, ok := states["999999"]; ok {
		t.Error("FetchIssueStatesByIdentifiers: nonexistent identifier 999999 present in result, want absent")
	}
}

func TestIntegration_FetchIssueComments(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidate := firstCandidate(t, ctx, adapter)

	comments, err := adapter.FetchIssueComments(ctx, candidate.ID)
	if err != nil {
		t.Fatalf("FetchIssueComments(%s): %v", candidate.ID, err)
	}
	if comments == nil {
		t.Fatal("comments is nil, want non-nil slice")
	}

	for _, c := range comments {
		if c.ID == "" {
			t.Error("comment ID is empty")
		}
		if c.Author == "" {
			t.Errorf("comment %s: Author is empty", c.ID)
		}
		if c.Body == "" {
			t.Errorf("comment %s: Body is empty", c.ID)
		}
	}

	for i := 1; i < len(comments); i++ {
		prev := parseCreatedAt(t, comments[i-1].CreatedAt)
		curr := parseCreatedAt(t, comments[i].CreatedAt)
		if prev.After(curr) {
			t.Errorf("comments not in non-descending CreatedAt order at index %d: %q before %q",
				i, comments[i-1].CreatedAt, comments[i].CreatedAt)
		}
	}
}

func TestIntegration_FetchIssueComments_SystemNotesExcluded(t *testing.T) {
	skipUnlessIntegration(t)

	systemIID := optionalEnv(t, "SORTIE_GITLAB_SYSTEM_NOTES_IID")

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	comments, err := adapter.FetchIssueComments(ctx, systemIID)
	if err != nil {
		t.Fatalf("FetchIssueComments(%s): %v", systemIID, err)
	}
	if comments == nil {
		t.Fatal("comments is nil, want non-nil slice")
	}
	if len(comments) != 0 {
		t.Errorf("FetchIssueComments(%s) returned %d comments, want 0 (system notes only)", systemIID, len(comments))
	}
}

func TestIntegration_FetchIssueComments_Paginated(t *testing.T) {
	skipUnlessIntegration(t)

	manyIID := optionalEnv(t, "SORTIE_GITLAB_MANY_NOTES_IID")

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	comments, err := adapter.FetchIssueComments(ctx, manyIID)
	if err != nil {
		t.Fatalf("FetchIssueComments(%s): %v", manyIID, err)
	}

	const minComments = 101
	if len(comments) < minComments {
		t.Errorf("FetchIssueComments(%s) returned %d comments, want at least %d", manyIID, len(comments), minComments)
	}

	seen := make(map[string]struct{}, len(comments))
	for _, c := range comments {
		if _, ok := seen[c.ID]; ok {
			t.Errorf("FetchIssueComments(%s): duplicate comment ID %s", manyIID, c.ID)
		}
		seen[c.ID] = struct{}{}
	}

	for i := 1; i < len(comments); i++ {
		prev := parseCreatedAt(t, comments[i-1].CreatedAt)
		curr := parseCreatedAt(t, comments[i].CreatedAt)
		if prev.After(curr) {
			t.Errorf("comments not in non-descending CreatedAt order at index %d: %q before %q",
				i, comments[i-1].CreatedAt, comments[i].CreatedAt)
		}
	}
}

func TestIntegration_TransitionIssue_SameState(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidate := firstCandidate(t, ctx, adapter)

	if err := adapter.TransitionIssue(ctx, candidate.ID, candidate.State); err != nil {
		t.Fatalf("TransitionIssue(%s, %q): %v", candidate.Identifier, candidate.State, err)
	}

	fetched, err := adapter.FetchIssueByID(ctx, candidate.ID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s): %v", candidate.ID, err)
	}
	if fetched.State != candidate.State {
		t.Errorf("State after same-state transition = %q, want %q", fetched.State, candidate.State)
	}
}

func TestIntegration_TransitionIssue_RoundTrip(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidate := firstCandidate(t, ctx, adapter)

	var target string
	for _, s := range activeStates(t) {
		if s != candidate.State {
			target = s
			break
		}
	}
	if target == "" {
		t.Skip("no configured active state distinct from the candidate's current state")
	}

	originalState := candidate.State
	originalLabels := slices.Clone(candidate.Labels)

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := adapter.TransitionIssue(cleanupCtx, candidate.ID, originalState); err != nil {
			t.Errorf("cleanup TransitionIssue(%s, %q): %v", candidate.Identifier, originalState, err)
		}
	})

	if err := adapter.TransitionIssue(ctx, candidate.ID, target); err != nil {
		t.Fatalf("TransitionIssue(%s, %q): %v", candidate.Identifier, target, err)
	}

	fetched, err := adapter.FetchIssueByID(ctx, candidate.ID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s): %v", candidate.ID, err)
	}
	if fetched.State != target {
		t.Errorf("State after transition = %q, want %q", fetched.State, target)
	}
	if slices.Contains(fetched.Labels, originalState) {
		t.Errorf("Labels = %v, still contains previous state label %q", fetched.Labels, originalState)
	}

	if err := adapter.TransitionIssue(ctx, candidate.ID, originalState); err != nil {
		t.Fatalf("TransitionIssue back to %q: %v", originalState, err)
	}

	restored, err := adapter.FetchIssueByID(ctx, candidate.ID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s) after restore: %v", candidate.ID, err)
	}
	if restored.State != originalState {
		t.Errorf("State after restore = %q, want %q", restored.State, originalState)
	}
	for _, label := range originalLabels {
		if !slices.Contains(restored.Labels, label) {
			t.Errorf("Labels after restore = %v, missing original label %q", restored.Labels, label)
		}
	}
}

func TestIntegration_TransitionIssue_TerminalAndBack(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidate := firstCandidate(t, ctx, adapter)

	terminal := terminalStates(t)
	if len(terminal) == 0 {
		t.Skip("no configured terminal state")
	}
	target := terminal[0]
	originalState := candidate.State

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := adapter.TransitionIssue(cleanupCtx, candidate.ID, originalState); err != nil {
			t.Errorf("cleanup TransitionIssue(%s, %q): %v", candidate.Identifier, originalState, err)
		}
	})

	if err := adapter.TransitionIssue(ctx, candidate.ID, target); err != nil {
		t.Fatalf("TransitionIssue(%s, %q): %v", candidate.Identifier, target, err)
	}

	fetched, err := adapter.FetchIssueByID(ctx, candidate.ID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s): %v", candidate.ID, err)
	}
	if fetched.State != target {
		t.Errorf("State after terminal transition = %q, want %q", fetched.State, target)
	}

	afterClose, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues after terminal transition: %v", err)
	}
	if slices.ContainsFunc(afterClose, func(i domain.Issue) bool { return i.ID == candidate.ID }) {
		t.Errorf("candidate %s still present in FetchCandidateIssues after transitioning to terminal state %q", candidate.Identifier, target)
	}

	if err := adapter.TransitionIssue(ctx, candidate.ID, originalState); err != nil {
		t.Fatalf("TransitionIssue back to %q: %v", originalState, err)
	}

	restored, err := adapter.FetchIssueByID(ctx, candidate.ID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s) after restore: %v", candidate.ID, err)
	}
	if restored.State != originalState {
		t.Errorf("State after restore = %q, want %q", restored.State, originalState)
	}

	afterReopen, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues after restore: %v", err)
	}
	if !slices.ContainsFunc(afterReopen, func(i domain.Issue) bool { return i.ID == candidate.ID }) {
		t.Errorf("candidate %s absent from FetchCandidateIssues after transitioning back to %q", candidate.Identifier, originalState)
	}
}

func TestIntegration_TransitionIssue_InvalidTarget(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidate := firstCandidate(t, ctx, adapter)

	const invalidTarget = "not-a-configured-state"
	err := adapter.TransitionIssue(ctx, candidate.ID, invalidTarget)
	if err == nil {
		t.Fatalf("TransitionIssue(%s, %q) = nil, want error", candidate.Identifier, invalidTarget)
	}

	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if te.Kind != domain.ErrTrackerPayload {
		t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, domain.ErrTrackerPayload)
	}
}

func TestIntegration_CommentIssue(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidate := firstCandidate(t, ctx, adapter)

	body := "sortie write-path integration test comment at " + time.Now().UTC().Format(time.RFC3339)
	if err := adapter.CommentIssue(ctx, candidate.ID, body); err != nil {
		t.Fatalf("CommentIssue(%s): %v", candidate.Identifier, err)
	}

	comments, err := adapter.FetchIssueComments(ctx, candidate.ID)
	if err != nil {
		t.Fatalf("FetchIssueComments(%s): %v", candidate.ID, err)
	}
	if !slices.ContainsFunc(comments, func(c domain.Comment) bool { return c.Body == body }) {
		t.Errorf("FetchIssueComments(%s) does not contain the posted comment %q", candidate.ID, body)
	}
}

func TestIntegration_AddLabel(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidate := firstCandidate(t, ctx, adapter)
	originalLabels := slices.Clone(candidate.Labels)

	const newLabel = "needs-human"
	if err := adapter.AddLabel(ctx, candidate.ID, newLabel); err != nil {
		t.Fatalf("AddLabel(%s, %q): %v", candidate.Identifier, newLabel, err)
	}

	fetched, err := adapter.FetchIssueByID(ctx, candidate.ID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s): %v", candidate.ID, err)
	}
	if !slices.Contains(fetched.Labels, newLabel) {
		t.Errorf("Labels = %v, want to contain %q", fetched.Labels, newLabel)
	}
	for _, label := range originalLabels {
		if !slices.Contains(fetched.Labels, label) {
			t.Errorf("Labels = %v, missing previously recorded label %q", fetched.Labels, label)
		}
	}
}

func TestIntegration_NewGitLabAdapter_ForeignProject(t *testing.T) {
	skipUnlessIntegration(t)

	foreign := optionalEnv(t, "SORTIE_GITLAB_FOREIGN_PROJECT")

	cfg := integrationConfig(t)
	cfg["project"] = foreign

	_, err := NewGitLabAdapter(cfg)
	if err == nil {
		t.Fatalf("NewGitLabAdapter(project=%q) = nil error, want error", foreign)
	}

	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if te.Kind != domain.ErrTrackerNotFound {
		t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, domain.ErrTrackerNotFound)
	}
}

func TestIntegration_FetchCandidateIssues_QueryFilter(t *testing.T) {
	skipUnlessIntegration(t)

	unfiltered := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	baseline, err := unfiltered.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues (unfiltered): %v", err)
	}
	baselineIDs := make(map[string]struct{}, len(baseline))
	for _, issue := range baseline {
		baselineIDs[issue.ID] = struct{}{}
	}

	cfg := integrationConfig(t)
	cfg["query_filter"] = "scope=assigned_to_me"
	filtered, err := NewGitLabAdapter(cfg)
	if err != nil {
		t.Fatalf("NewGitLabAdapter with query_filter: %v", err)
	}

	issues, err := filtered.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues (filtered): %v", err)
	}

	for _, issue := range issues {
		if _, ok := baselineIDs[issue.ID]; !ok {
			t.Errorf("FetchCandidateIssues (filtered) returned issue %s not present in the unfiltered candidate set", issue.Identifier)
		}
		if issue.Assignee == "" {
			t.Errorf("issue %s: Assignee is empty, want non-empty under scope=assigned_to_me", issue.Identifier)
		}
	}
}
