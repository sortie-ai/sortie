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

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
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

	mu        sync.RWMutex
	overrides map[string]string // issue ID → overridden state
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
		path:         path,
		activeStates: toStringSet(extractStringSlice(config["active_states"])),
		overrides:    make(map[string]string),
	}, nil
}

// FetchCandidateIssues returns issues whose state matches the
// configured active states. Comments are set to nil on all returned
// issues.
func (a *FileAdapter) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	raws, err := loadIssues(a.path)
	if err != nil {
		return nil, err
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make([]domain.Issue, 0, len(raws))
	for _, raw := range raws {
		raw = a.applyOverride(raw)
		if len(a.activeStates) > 0 && !a.activeStates[strings.ToLower(raw.State)] {
			continue
		}
		iss := normalize(raw)
		iss.Comments = nil
		result = append(result, iss)
	}
	return result, nil
}

// FetchIssueByID returns a single fully-populated issue including
// comments. Returns a [*domain.TrackerError] with Kind
// [domain.ErrTrackerNotFound] if the issue does not exist.
func (a *FileAdapter) FetchIssueByID(_ context.Context, issueID string) (domain.Issue, error) {
	raws, err := loadIssues(a.path)
	if err != nil {
		return domain.Issue{}, err
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	for _, raw := range raws {
		if raw.ID == issueID {
			raw = a.applyOverride(raw)
			iss := normalize(raw)
			if iss.Comments == nil {
				iss.Comments = []domain.Comment{}
			}
			return iss, nil
		}
	}

	return domain.Issue{}, &domain.TrackerError{
		Kind:    domain.ErrTrackerNotFound,
		Message: fmt.Sprintf("issue not found: %s", issueID),
	}
}

// FetchIssuesByStates returns issues in the specified states. An
// empty states slice returns immediately with no file read. Comments
// are set to nil on returned issues.
func (a *FileAdapter) FetchIssuesByStates(_ context.Context, states []string) ([]domain.Issue, error) {
	if len(states) == 0 {
		return []domain.Issue{}, nil
	}

	raws, err := loadIssues(a.path)
	if err != nil {
		return nil, err
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	stateSet := make(map[string]bool, len(states))
	for _, s := range states {
		stateSet[strings.ToLower(s)] = true
	}

	result := make([]domain.Issue, 0, len(raws))
	for _, raw := range raws {
		raw = a.applyOverride(raw)
		if stateSet[strings.ToLower(raw.State)] {
			iss := normalize(raw)
			iss.Comments = nil
			result = append(result, iss)
		}
	}
	return result, nil
}

// FetchIssueStatesByIDs returns the current state for each requested
// issue ID. Issues not found in the file are omitted from the map.
func (a *FileAdapter) FetchIssueStatesByIDs(_ context.Context, issueIDs []string) (map[string]string, error) {
	if len(issueIDs) == 0 {
		return map[string]string{}, nil
	}

	raws, err := loadIssues(a.path)
	if err != nil {
		return nil, err
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	wanted := make(map[string]bool, len(issueIDs))
	for _, id := range issueIDs {
		wanted[id] = true
	}

	result := make(map[string]string, len(issueIDs))
	for _, raw := range raws {
		if wanted[raw.ID] {
			raw = a.applyOverride(raw)
			result[raw.ID] = raw.State
		}
	}
	return result, nil
}

// FetchIssueStatesByIdentifiers returns the current state for each
// requested issue identifier. Issues not found in the file are omitted
// from the map.
func (a *FileAdapter) FetchIssueStatesByIdentifiers(_ context.Context, identifiers []string) (map[string]string, error) {
	if len(identifiers) == 0 {
		return map[string]string{}, nil
	}

	raws, err := loadIssues(a.path)
	if err != nil {
		return nil, err
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	wanted := make(map[string]bool, len(identifiers))
	for _, id := range identifiers {
		wanted[id] = true
	}

	result := make(map[string]string, len(identifiers))
	for _, raw := range raws {
		if wanted[raw.Identifier] {
			raw = a.applyOverride(raw)
			result[raw.Identifier] = raw.State
		}
	}
	return result, nil
}

// FetchIssueComments returns comments for the specified issue.
// Returns an empty non-nil slice when no comments exist. Returns a
// [*domain.TrackerError] with Kind [domain.ErrTrackerNotFound] if
// the issue does not exist.
func (a *FileAdapter) FetchIssueComments(_ context.Context, issueID string) ([]domain.Comment, error) {
	raws, err := loadIssues(a.path)
	if err != nil {
		return nil, err
	}

	for _, raw := range raws {
		if raw.ID == issueID {
			iss := normalize(raw)
			if iss.Comments == nil {
				return []domain.Comment{}, nil
			}
			return iss.Comments, nil
		}
	}

	return nil, &domain.TrackerError{
		Kind:    domain.ErrTrackerNotFound,
		Message: fmt.Sprintf("issue not found: %s", issueID),
	}
}

// TransitionIssue records a state override for the given issue in the
// adapter's in-memory override map. Subsequent read operations
// ([FileAdapter.FetchCandidateIssues], [FileAdapter.FetchIssueByID],
// etc.) reflect the overridden state. The on-disk fixture file is
// never modified. Returns a [*domain.TrackerError] with Kind
// [domain.ErrTrackerNotFound] if the issue ID does not exist in the
// fixture. Safe for concurrent use.
func (a *FileAdapter) TransitionIssue(_ context.Context, issueID string, targetState string) error {
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

// --- unexported helpers ---

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
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is from trusted adapter configuration
	if err != nil {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("failed to read file: %s", path),
			Err:     err,
		}
	}

	var raws []rawIssue
	if err := json.Unmarshal(data, &raws); err != nil {
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

	// Priority: integer only, non-integers become nil.
	if len(raw.Priority) > 0 {
		var p int
		if json.Unmarshal(raw.Priority, &p) == nil {
			// Guard against floats that json.Unmarshal silently truncates:
			// re-marshal the int and compare to the raw bytes.
			canonical, _ := json.Marshal(p)
			if string(canonical) == string(raw.Priority) {
				iss.Priority = &p
			}
		}
	}

	// Labels: lowercase, non-nil empty slice when absent.
	if raw.Labels != nil {
		iss.Labels = make([]string, len(raw.Labels))
		for i, l := range raw.Labels {
			iss.Labels[i] = strings.ToLower(l)
		}
	} else {
		iss.Labels = []string{}
	}

	// Parent: nil stays nil.
	if raw.Parent != nil {
		iss.Parent = &domain.ParentRef{
			ID:         raw.Parent.ID,
			Identifier: raw.Parent.Identifier,
		}
	}

	// Comments: nil means "not fetched", empty non-nil means "none exist".
	if raw.Comments != nil {
		iss.Comments = make([]domain.Comment, len(raw.Comments))
		for i, c := range raw.Comments {
			iss.Comments[i] = domain.Comment{
				ID:        c.ID,
				Author:    c.Author,
				Body:      c.Body,
				CreatedAt: c.CreatedAt,
			}
		}
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

// extractStringSlice safely extracts a []string from a value that may
// be []any (as produced by YAML decoders) or []string. Non-string
// elements are silently skipped.
func extractStringSlice(val any) []string {
	switch v := val.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	default:
		return nil
	}
}

func toStringSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[strings.ToLower(s)] = true
	}
	return m
}
