// Package file implements [domain.TrackerAdapter] for a local JSON
// fixture file. Issues are read from disk on each operation call,
// normalized to domain types with labels lowercased, integer-only
// priority (non-integers become nil), and nil-vs-empty comments
// semantics. Intended for development and testing where a live
// tracker API is unavailable. Registered under kind "file" via
// an init function.
package file

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Trackers.Register("file", NewFileAdapter)
}

// Compile-time interface satisfaction check.
var _ domain.TrackerAdapter = (*FileAdapter)(nil)

// FileAdapter reads issues from a JSON file and implements all seven
// [domain.TrackerAdapter] operations. The file is re-read on each
// call to support test scenarios that modify the fixture between
// operations. State mutations via [FileAdapter.TransitionIssue] are
// stored in an in-memory override map layered on top of disk reads.
// Overrides are not persisted to disk — they exist only for the
// lifetime of the adapter instance. Safe for concurrent use.
type FileAdapter struct {
	path         string
	activeStates map[string]bool

	mu               sync.RWMutex
	overrides        map[string]string           // issue ID → overridden state
	commentOverrides map[string][]domain.Comment // issue ID → appended comments
	metrics          domain.Metrics              // nil-safe: check before calling
}

// NewFileAdapter creates a [FileAdapter] from adapter configuration.
// Required config keys:
//   - "path" (string): filesystem path to the JSON fixture file.
//
// Optional config keys:
//   - "active_states" ([]any holding strings): states considered active
//     for [FileAdapter.FetchCandidateIssues]. If empty, all issues are
//     candidates.
//
// Returns a [*domain.TrackerError] with Kind [domain.ErrTrackerPayload]
// if "path" is missing or empty.
func NewFileAdapter(config map[string]any) (domain.TrackerAdapter, error) {
	path, _ := config["path"].(string)
	if path == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "missing required config key: path",
		}
	}

	return &FileAdapter{
		path:             path,
		activeStates:     toStringSet(typeutil.ExtractStringSlice(config["active_states"])),
		overrides:        make(map[string]string),
		commentOverrides: make(map[string][]domain.Comment),
	}, nil
}

// FetchCandidateIssues returns issues whose state matches the
// configured active states. Comments are set to nil on all returned
// issues.
func (a *FileAdapter) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_candidates", func() error {
		raws, err := loadIssues(a.path)
		if err != nil {
			return err
		}

		a.mu.RLock()
		defer a.mu.RUnlock()

		issues = make([]domain.Issue, 0, len(raws))
		for _, raw := range raws {
			raw = a.applyOverride(raw)
			if len(a.activeStates) > 0 && !a.activeStates[strings.ToLower(raw.State)] {
				continue
			}
			iss := normalize(raw)
			iss.Comments = nil
			issues = append(issues, iss)
		}
		return nil
	})
	return issues, err
}

// FetchIssueByID returns a single fully-populated issue including
// comments. Returns a [*domain.TrackerError] with Kind
// [domain.ErrTrackerNotFound] if the issue does not exist.
func (a *FileAdapter) FetchIssueByID(_ context.Context, issueID string) (domain.Issue, error) {
	var issue domain.Issue
	err := trackermetrics.Track(a.metrics, "fetch_issue", func() error {
		raws, err := loadIssues(a.path)
		if err != nil {
			return err
		}

		a.mu.RLock()
		defer a.mu.RUnlock()

		for _, raw := range raws {
			if raw.ID != issueID {
				continue
			}

			raw = a.applyOverride(raw)
			issue = normalize(raw)
			if issue.Comments == nil {
				issue.Comments = []domain.Comment{}
			}
			issue.Comments = append(issue.Comments, a.commentOverrides[issueID]...)
			return nil
		}

		return &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("issue not found: %s", issueID),
		}
	})
	return issue, err
}

// FetchIssuesByStates returns issues in the specified states. An
// empty states slice returns immediately with no file read. Comments
// are set to nil on returned issues.
func (a *FileAdapter) FetchIssuesByStates(_ context.Context, states []string) ([]domain.Issue, error) {
	if len(states) == 0 {
		return []domain.Issue{}, nil
	}

	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_by_states", func() error {
		raws, err := loadIssues(a.path)
		if err != nil {
			return err
		}

		a.mu.RLock()
		defer a.mu.RUnlock()

		stateSet := make(map[string]bool, len(states))
		for _, s := range states {
			stateSet[strings.ToLower(s)] = true
		}

		issues = make([]domain.Issue, 0, len(raws))
		for _, raw := range raws {
			raw = a.applyOverride(raw)
			if stateSet[strings.ToLower(raw.State)] {
				iss := normalize(raw)
				iss.Comments = nil
				issues = append(issues, iss)
			}
		}
		return nil
	})
	return issues, err
}

// FetchIssueStatesByIDs returns the current state for each requested
// issue ID. Issues not found in the file are omitted from the map.
func (a *FileAdapter) FetchIssueStatesByIDs(_ context.Context, issueIDs []string) (map[string]string, error) {
	if len(issueIDs) == 0 {
		return map[string]string{}, nil
	}

	states := make(map[string]string, len(issueIDs))
	err := trackermetrics.Track(a.metrics, "fetch_states_by_ids", func() error {
		raws, err := loadIssues(a.path)
		if err != nil {
			return err
		}

		a.mu.RLock()
		defer a.mu.RUnlock()

		wanted := make(map[string]bool, len(issueIDs))
		for _, id := range issueIDs {
			wanted[id] = true
		}

		for _, raw := range raws {
			if wanted[raw.ID] {
				raw = a.applyOverride(raw)
				states[raw.ID] = raw.State
			}
		}
		return nil
	})
	return states, err
}

// FetchIssueStatesByIdentifiers returns the current state for each
// requested issue identifier. Issues not found in the file are omitted
// from the map.
func (a *FileAdapter) FetchIssueStatesByIdentifiers(_ context.Context, identifiers []string) (map[string]string, error) {
	if len(identifiers) == 0 {
		return map[string]string{}, nil
	}

	states := make(map[string]string, len(identifiers))
	err := trackermetrics.Track(a.metrics, "fetch_states_by_identifiers", func() error {
		raws, err := loadIssues(a.path)
		if err != nil {
			return err
		}

		a.mu.RLock()
		defer a.mu.RUnlock()

		wanted := make(map[string]bool, len(identifiers))
		for _, id := range identifiers {
			wanted[id] = true
		}

		for _, raw := range raws {
			if wanted[raw.Identifier] {
				raw = a.applyOverride(raw)
				states[raw.Identifier] = raw.State
			}
		}
		return nil
	})
	return states, err
}

// FetchIssueComments returns comments for the specified issue.
// Returns an empty non-nil slice when no comments exist. Returns a
// [*domain.TrackerError] with Kind [domain.ErrTrackerNotFound] if
// the issue does not exist.
func (a *FileAdapter) FetchIssueComments(_ context.Context, issueID string) ([]domain.Comment, error) {
	comments := make([]domain.Comment, 0)
	err := trackermetrics.Track(a.metrics, "fetch_comments", func() error {
		raws, err := loadIssues(a.path)
		if err != nil {
			return err
		}

		for _, raw := range raws {
			if raw.ID != issueID {
				continue
			}

			issue := normalize(raw)
			comments = issue.Comments
			if comments == nil {
				comments = []domain.Comment{}
			}

			a.mu.RLock()
			comments = append(comments, a.commentOverrides[issueID]...)
			a.mu.RUnlock()
			return nil
		}

		return &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("issue not found: %s", issueID),
		}
	})
	return comments, err
}

// TransitionIssue records a state override for the given issue in the
// adapter's in-memory override map. Subsequent read operations
// ([FileAdapter.FetchCandidateIssues], [FileAdapter.FetchIssueByID],
// etc.) reflect the overridden state. The on-disk fixture file is
// never modified. Returns a [*domain.TrackerError] with Kind
// [domain.ErrTrackerNotFound] if the issue ID does not exist in the
// fixture. Safe for concurrent use.
func (a *FileAdapter) TransitionIssue(_ context.Context, issueID string, targetState string) error {
	return trackermetrics.Track(a.metrics, "transition", func() error {
		raws, err := loadIssues(a.path)
		if err != nil {
			return err
		}

		found := false
		for _, raw := range raws {
			if raw.ID == issueID {
				found = true
				break
			}
		}
		if !found {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("issue not found: %s", issueID),
			}
		}

		a.mu.Lock()
		a.overrides[issueID] = targetState
		a.mu.Unlock()
		return nil
	})
}

// CommentIssue records a comment for the given issue in the adapter's
// in-memory comment store. Subsequent [FileAdapter.FetchIssueByID] and
// [FileAdapter.FetchIssueComments] calls include these comments after
// any comments present in the fixture file. Returns a [*domain.TrackerError]
// with Kind [domain.ErrTrackerNotFound] if the issue does not exist.
func (a *FileAdapter) CommentIssue(_ context.Context, issueID string, text string) error {
	return trackermetrics.Track(a.metrics, "comment", func() error {
		raws, err := loadIssues(a.path)
		if err != nil {
			return err
		}

		found := false
		for _, raw := range raws {
			if raw.ID == issueID {
				found = true
				break
			}
		}
		if !found {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("issue not found: %s", issueID),
			}
		}

		a.mu.Lock()
		a.commentOverrides[issueID] = append(a.commentOverrides[issueID], domain.Comment{
			Body:      text,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		})
		a.mu.Unlock()
		return nil
	})
}

// SetMetrics configures the metrics recorder for tracker API call
// instrumentation. When not called or called with nil, the adapter
// operates without recording metrics. Safe to call before any
// adapter operations. Not safe to call concurrently with adapter
// operations.
func (a *FileAdapter) SetMetrics(m domain.Metrics) {
	a.metrics = m
}

// AddLabel is a no-op for the file adapter. File-based issues do not
// support labels.
func (a *FileAdapter) AddLabel(_ context.Context, _ string, _ string) error {
	return nil
}

// applyOverride returns a copy of raw with its State replaced by the
// in-memory override value when one exists. Caller must hold at least
// a read lock on a.mu.
func (a *FileAdapter) applyOverride(raw rawIssue) rawIssue {
	if st, ok := a.overrides[raw.ID]; ok {
		raw.State = st
	}
	return raw
}

type rawIssue struct {
	ID          string          `json:"id"`
	Identifier  string          `json:"identifier"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Priority    json.RawMessage `json:"priority"`
	State       string          `json:"state"`
	BranchName  string          `json:"branch_name"`
	URL         string          `json:"url"`
	Labels      []string        `json:"labels"`
	Assignee    string          `json:"assignee"`
	IssueType   string          `json:"issue_type"`
	Parent      *rawParentRef   `json:"parent"`
	Comments    []rawComment    `json:"comments"`
	BlockedBy   []rawBlockerRef `json:"blocked_by"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
}

type rawParentRef struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
}

type rawComment struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

type rawBlockerRef struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	State      string `json:"state"`
}

func loadIssues(path string) ([]rawIssue, error) {
	contents, err := os.ReadFile(path) //nolint:gosec // G304: path is from trusted adapter configuration
	if err != nil {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("failed to read file: %s", path),
			Err:     err,
		}
	}

	var raws []rawIssue
	if err := json.Unmarshal(contents, &raws); err != nil {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("failed to parse file: %s", path),
			Err:     err,
		}
	}
	return raws, nil
}

func normalize(raw rawIssue) domain.Issue {
	iss := domain.Issue{
		ID:          raw.ID,
		Identifier:  raw.Identifier,
		Title:       raw.Title,
		Description: raw.Description,
		State:       raw.State,
		BranchName:  raw.BranchName,
		URL:         raw.URL,
		Assignee:    raw.Assignee,
		IssueType:   raw.IssueType,
		CreatedAt:   raw.CreatedAt,
		UpdatedAt:   raw.UpdatedAt,
	}

	iss.Priority = issuekit.ParsePriorityIntStrict(raw.Priority)
	iss.Labels = issuekit.NormalizeLabels(raw.Labels)

	// Parent: nil stays nil.
	if raw.Parent != nil {
		iss.Parent = &domain.ParentRef{
			ID:         raw.Parent.ID,
			Identifier: raw.Parent.Identifier,
		}
	}

	if raw.Comments != nil {
		source := make([]issuekit.SourceComment, len(raw.Comments))
		for i, c := range raw.Comments {
			source[i] = issuekit.SourceComment{
				ID:        c.ID,
				Author:    c.Author,
				Body:      c.Body,
				CreatedAt: c.CreatedAt,
			}
		}
		iss.Comments = issuekit.NormalizeComments(source)
	}

	// BlockedBy: non-nil empty slice when absent.
	if raw.BlockedBy != nil {
		iss.BlockedBy = make([]domain.BlockerRef, len(raw.BlockedBy))
		for i, b := range raw.BlockedBy {
			iss.BlockedBy[i] = domain.BlockerRef{
				ID:         b.ID,
				Identifier: b.Identifier,
				State:      b.State,
			}
		}
	} else {
		iss.BlockedBy = []domain.BlockerRef{}
	}

	return iss
}

func toStringSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[strings.ToLower(s)] = true
	}
	return m
}
