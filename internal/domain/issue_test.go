package domain

import "testing"

func intPtr(v int) *int { return &v }

func TestToTemplateMap_FullyPopulated(t *testing.T) {
	t.Parallel()

	iss := &Issue{
		ID:          "10001",
		Identifier:  "PROJ-42",
		Title:       "Fix login bug",
		Description: "Users cannot log in with SSO.",
		Priority:    intPtr(2),
		State:       "In Progress",
		BranchName:  "feature/PROJ-42",
		URL:         "https://tracker.example.com/PROJ-42",
		Labels:      []string{"bug", "sso"},
		Assignee:    "alice",
		IssueType:   "Bug",
		Parent: &ParentRef{
			ID:         "10000",
			Identifier: "PROJ-40",
		},
		Comments: []Comment{
			{ID: "c1", Author: "bob", Body: "Reproduced on staging.", CreatedAt: "2026-03-01T10:00:00Z"},
		},
		BlockedBy: []BlockerRef{
			{ID: "10002", Identifier: "PROJ-43", State: "To Do"},
		},
		CreatedAt: "2026-02-28T09:00:00Z",
		UpdatedAt: "2026-03-01T12:00:00Z",
	}

	m := iss.ToTemplateMap()

	// Verify all 16 keys exist.
	expectedKeys := []string{
		"id", "identifier", "title", "description", "priority", "state",
		"branch_name", "url", "labels", "assignee", "issue_type", "parent",
		"comments", "blocked_by", "created_at", "updated_at",
	}
	for _, k := range expectedKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}

	// Scalar fields.
	if m["id"] != "10001" {
		t.Errorf("id = %v, want 10001", m["id"])
	}
	if m["identifier"] != "PROJ-42" {
		t.Errorf("identifier = %v, want PROJ-42", m["identifier"])
	}
	if m["title"] != "Fix login bug" {
		t.Errorf("title = %v, want Fix login bug", m["title"])
	}
	if m["state"] != "In Progress" {
		t.Errorf("state = %v, want In Progress", m["state"])
	}
	if m["branch_name"] != "feature/PROJ-42" {
		t.Errorf("branch_name = %v, want feature/PROJ-42", m["branch_name"])
	}
	if m["assignee"] != "alice" {
		t.Errorf("assignee = %v, want alice", m["assignee"])
	}
	if m["issue_type"] != "Bug" {
		t.Errorf("issue_type = %v, want Bug", m["issue_type"])
	}

	// Priority.
	if p, ok := m["priority"].(int); !ok || p != 2 {
		t.Errorf("priority = %v (%T), want 2 (int)", m["priority"], m["priority"])
	}

	// Parent.
	parent, ok := m["parent"].(map[string]any)
	if !ok {
		t.Fatalf("parent type = %T, want map[string]any", m["parent"])
	}
	if parent["id"] != "10000" || parent["identifier"] != "PROJ-40" {
		t.Errorf("parent = %v, want id=10000 identifier=PROJ-40", parent)
	}

	// Comments.
	comments, ok := m["comments"].([]map[string]any)
	if !ok {
		t.Fatalf("comments type = %T, want []map[string]any", m["comments"])
	}
	if len(comments) != 1 {
		t.Fatalf("len(comments) = %d, want 1", len(comments))
	}
	if comments[0]["author"] != "bob" {
		t.Errorf("comments[0].author = %v, want bob", comments[0]["author"])
	}

	// BlockedBy.
	blockers, ok := m["blocked_by"].([]map[string]any)
	if !ok {
		t.Fatalf("blocked_by type = %T, want []map[string]any", m["blocked_by"])
	}
	if len(blockers) != 1 {
		t.Fatalf("len(blocked_by) = %d, want 1", len(blockers))
	}
	if blockers[0]["state"] != "To Do" {
		t.Errorf("blocked_by[0].state = %v, want To Do", blockers[0]["state"])
	}

	// Labels.
	labels, ok := m["labels"].([]string)
	if !ok {
		t.Fatalf("labels type = %T, want []string", m["labels"])
	}
	if len(labels) != 2 || labels[0] != "bug" || labels[1] != "sso" {
		t.Errorf("labels = %v, want [bug sso]", labels)
	}
}

func TestToTemplateMap_Minimal(t *testing.T) {
	t.Parallel()

	iss := &Issue{
		ID:         "10001",
		Identifier: "PROJ-1",
		Title:      "Minimal",
		State:      "Open",
		Labels:     []string{},
		BlockedBy:  []BlockerRef{},
	}

	m := iss.ToTemplateMap()

	if m["priority"] != nil {
		t.Errorf("priority = %v, want nil", m["priority"])
	}
	if m["parent"] != nil {
		t.Errorf("parent = %v, want nil", m["parent"])
	}
	if m["comments"] != nil {
		t.Errorf("comments = %v, want nil (not fetched)", m["comments"])
	}

	// Labels: non-nil empty slice.
	labels, ok := m["labels"].([]string)
	if !ok {
		t.Fatalf("labels type = %T, want []string", m["labels"])
	}
	if len(labels) != 0 {
		t.Errorf("len(labels) = %d, want 0", len(labels))
	}

	// BlockedBy: non-nil empty slice.
	blockers, ok := m["blocked_by"].([]map[string]any)
	if !ok {
		t.Fatalf("blocked_by type = %T, want []map[string]any", m["blocked_by"])
	}
	if len(blockers) != 0 {
		t.Errorf("len(blocked_by) = %d, want 0", len(blockers))
	}
}

func TestToTemplateMap_NilVsEmptyComments(t *testing.T) {
	t.Parallel()

	// Nil comments = not fetched.
	issNil := &Issue{
		ID:         "1",
		Identifier: "X-1",
		Title:      "Nil",
		State:      "Open",
		Labels:     []string{},
		BlockedBy:  []BlockerRef{},
		Comments:   nil,
	}
	mNil := issNil.ToTemplateMap()
	if mNil["comments"] != nil {
		t.Errorf("nil comments: got %v (%T), want nil", mNil["comments"], mNil["comments"])
	}

	// Empty comments = fetched, none exist.
	issEmpty := &Issue{
		ID:         "2",
		Identifier: "X-2",
		Title:      "Empty",
		State:      "Open",
		Labels:     []string{},
		BlockedBy:  []BlockerRef{},
		Comments:   []Comment{},
	}
	mEmpty := issEmpty.ToTemplateMap()
	comments, ok := mEmpty["comments"].([]map[string]any)
	if !ok {
		t.Fatalf("empty comments: type = %T, want []map[string]any", mEmpty["comments"])
	}
	if len(comments) != 0 {
		t.Errorf("empty comments: len = %d, want 0", len(comments))
	}
}

func TestToTemplateMap_Priority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		priority *int
		want     any
	}{
		{"nil", nil, nil},
		{"zero", intPtr(0), 0},
		{"positive", intPtr(3), 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			iss := &Issue{
				ID:         "1",
				Identifier: "X-1",
				Title:      "P",
				State:      "Open",
				Priority:   tt.priority,
				Labels:     []string{},
				BlockedBy:  []BlockerRef{},
			}
			m := iss.ToTemplateMap()
			got := m["priority"]
			if got != tt.want {
				t.Errorf("priority = %v (%T), want %v (%T)", got, got, tt.want, tt.want)
			}
		})
	}
}

func TestToTemplateMap_BlockedByEmptyState(t *testing.T) {
	t.Parallel()

	iss := &Issue{
		ID:         "1",
		Identifier: "X-1",
		Title:      "Blocked",
		State:      "Open",
		Labels:     []string{},
		BlockedBy: []BlockerRef{
			{ID: "2", Identifier: "X-2", State: ""},
		},
	}
	m := iss.ToTemplateMap()

	blockers, ok := m["blocked_by"].([]map[string]any)
	if !ok {
		t.Fatalf("blocked_by type = %T, want []map[string]any", m["blocked_by"])
	}
	if len(blockers) != 1 {
		t.Fatalf("len(blocked_by) = %d, want 1", len(blockers))
	}
	if blockers[0]["state"] != "" {
		t.Errorf("blocked_by[0].state = %q, want empty string", blockers[0]["state"])
	}
}

func TestToTemplateMap_NilLabelsNormalized(t *testing.T) {
	t.Parallel()

	iss := &Issue{
		ID:         "1",
		Identifier: "X-1",
		Title:      "No labels",
		State:      "Open",
		Labels:     nil,
		BlockedBy:  []BlockerRef{},
	}
	m := iss.ToTemplateMap()

	labels, ok := m["labels"].([]string)
	if !ok {
		t.Fatalf("labels type = %T, want []string", m["labels"])
	}
	if labels == nil {
		t.Error("labels is nil, want non-nil empty slice")
	}
	if len(labels) != 0 {
		t.Errorf("len(labels) = %d, want 0", len(labels))
	}
}
