package linear

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func ptrFloat(v float64) *float64 { return &v }
func ptrStr(v string) *string     { return &v }
func ptrInt(v int) *int           { return &v }

func TestNormalizeIssue(t *testing.T) {
	t.Parallel()

	li := linearIssue{
		ID:          "uuid-1",
		Identifier:  "SOR-5",
		Title:       "Title",
		Description: ptrStr("body"),
		Priority:    ptrFloat(2),
		BranchName:  "tasks/sor-5-anything/with/slashes",
		URL:         "https://linear.app/org/issue/SOR-5/title",
		CreatedAt:   "2026-06-10T09:00:00.000Z",
		UpdatedAt:   "2026-06-12T14:30:00.000Z",
		State:       linearStateName{Name: "In Progress"},
		Assignee:    &linearUser{DisplayName: "Ada Lovelace", Name: "ada", Email: "ada@example.com"},
		Parent:      &linearParent{ID: "uuid-parent", Identifier: "SOR-1"},
		Labels: linearLabelConn{
			Nodes: []linearLabel{{Name: "Feature"}, {Name: "Backend"}},
		},
		InverseRelations: linearRelationConn{
			Nodes: []linearRelation{
				{Type: "blocks", Issue: linearRelatedIssue{ID: "uuid-b", Identifier: "SOR-7", State: linearStateName{Name: "Backlog"}}},
			},
		},
	}

	got := normalizeIssue(li, nil)

	want := domain.Issue{
		ID:          "uuid-1",
		Identifier:  "SOR-5",
		Title:       "Title",
		Description: "body",
		Priority:    ptrInt(2),
		State:       "In Progress",
		BranchName:  "tasks/sor-5-anything/with/slashes",
		URL:         "https://linear.app/org/issue/SOR-5/title",
		Labels:      []string{"feature", "backend"},
		Assignee:    "Ada Lovelace",
		Parent:      &domain.ParentRef{ID: "uuid-parent", Identifier: "SOR-1"},
		BlockedBy:   []domain.BlockerRef{{ID: "uuid-b", Identifier: "SOR-7", State: "Backlog"}},
		CreatedAt:   "2026-06-10T09:00:00.000Z",
		UpdatedAt:   "2026-06-12T14:30:00.000Z",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("normalizeIssue() = %+v, want %+v", got, want)
	}
}

func TestNormalizeIssueDisplayIDAndTypeEmpty(t *testing.T) {
	t.Parallel()

	got := normalizeIssue(linearIssue{ID: "x", Identifier: "SOR-1"}, nil)

	if got.DisplayID != "" {
		t.Errorf("DisplayID = %q, want empty", got.DisplayID)
	}
	if got.IssueType != "" {
		t.Errorf("IssueType = %q, want empty", got.IssueType)
	}
}

func TestNormalizeIssueNullableFields(t *testing.T) {
	t.Parallel()

	t.Run("null description maps to empty string", func(t *testing.T) {
		t.Parallel()

		got := normalizeIssue(linearIssue{Description: nil}, nil)
		if got.Description != "" {
			t.Errorf("Description = %q, want empty string", got.Description)
		}
	})

	t.Run("nil assignee maps to empty string", func(t *testing.T) {
		t.Parallel()

		got := normalizeIssue(linearIssue{Assignee: nil}, nil)
		if got.Assignee != "" {
			t.Errorf("Assignee = %q, want empty string", got.Assignee)
		}
	})

	t.Run("nil parent maps to nil", func(t *testing.T) {
		t.Parallel()

		got := normalizeIssue(linearIssue{Parent: nil}, nil)
		if got.Parent != nil {
			t.Errorf("Parent = %+v, want nil", got.Parent)
		}
	})

	t.Run("no labels yields non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		got := normalizeIssue(linearIssue{}, nil)
		if got.Labels == nil {
			t.Error("Labels = nil, want non-nil empty slice")
		}
		if len(got.Labels) != 0 {
			t.Errorf("Labels = %v, want empty", got.Labels)
		}
	})
}

func TestNormalizePriority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *float64
		want *int
	}{
		{"nil pointer", nil, nil},
		{"zero maps to nil", ptrFloat(0), nil},
		{"one", ptrFloat(1), ptrInt(1)},
		{"two", ptrFloat(2), ptrInt(2)},
		{"three", ptrFloat(3), ptrInt(3)},
		{"four", ptrFloat(4), ptrInt(4)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := normalizePriority(tt.in)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("normalizePriority(%v) = %v, want %v", deref(tt.in), deref(tt.want), deref(tt.want))
			}
		})
	}
}

func TestResolveAssigneeFallbackChain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		user *linearUser
		want string
	}{
		{"nil user", nil, ""},
		{"display name preferred", &linearUser{DisplayName: "Ada", Name: "ada", Email: "ada@x"}, "Ada"},
		{"name when display empty", &linearUser{Name: "ada", Email: "ada@x"}, "ada"},
		{"email when name and display empty", &linearUser{Email: "ada@x"}, "ada@x"},
		{"empty when all empty", &linearUser{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := resolveAssignee(tt.user)

			if got != tt.want {
				t.Errorf("resolveAssignee() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractBlockers(t *testing.T) {
	t.Parallel()

	t.Run("extracts only blocks relations", func(t *testing.T) {
		t.Parallel()

		rel := linearRelationConn{
			Nodes: []linearRelation{
				{Type: "blocks", Issue: linearRelatedIssue{ID: "id-a", Identifier: "SOR-5", State: linearStateName{Name: "Todo"}}},
				{Type: "related", Issue: linearRelatedIssue{ID: "id-b", Identifier: "SOR-6", State: linearStateName{Name: "Done"}}},
				{Type: "duplicate", Issue: linearRelatedIssue{ID: "id-c", Identifier: "SOR-9", State: linearStateName{Name: "Done"}}},
			},
		}

		got := extractBlockers(rel)

		want := []domain.BlockerRef{{ID: "id-a", Identifier: "SOR-5", State: "Todo"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("extractBlockers() = %v, want %v", got, want)
		}
	})

	t.Run("matches blocks case-insensitively after trimming", func(t *testing.T) {
		t.Parallel()

		rel := linearRelationConn{
			Nodes: []linearRelation{
				{Type: "  Blocks ", Issue: linearRelatedIssue{ID: "id-a", Identifier: "SOR-5", State: linearStateName{Name: "Todo"}}},
			},
		}

		got := extractBlockers(rel)

		if len(got) != 1 {
			t.Fatalf("extractBlockers() len = %d, want 1", len(got))
		}
		if got[0].Identifier != "SOR-5" {
			t.Errorf("extractBlockers()[0].Identifier = %q, want %q", got[0].Identifier, "SOR-5")
		}
	})

	t.Run("no blockers yields non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		got := extractBlockers(linearRelationConn{})

		if got == nil {
			t.Error("extractBlockers() = nil, want non-nil empty slice")
		}
		if len(got) != 0 {
			t.Errorf("extractBlockers() = %v, want empty", got)
		}
	})
}

func TestNormalizeComments(t *testing.T) {
	t.Parallel()

	t.Run("author fallback chain", func(t *testing.T) {
		t.Parallel()

		nodes := []linearComment{
			{ID: "c1", Body: "b1", CreatedAt: "t1", User: &linearUser{DisplayName: "Ada", Name: "ada"}},
			{ID: "c2", Body: "b2", CreatedAt: "t2", User: &linearUser{Name: "grace"}},
			{ID: "c3", Body: "b3", CreatedAt: "t3", User: nil, BotActor: &linearBotActor{Name: "Sortie Bot"}},
			{ID: "c4", Body: "b4", CreatedAt: "t4", User: nil, BotActor: nil},
			{ID: "c5", Body: "b5", CreatedAt: "t5", User: &linearUser{}, BotActor: &linearBotActor{Name: "Fallback Bot"}},
		}

		got := normalizeComments(nodes)

		wantAuthors := []string{"Ada", "grace", "Sortie Bot", "", "Fallback Bot"}
		for i, c := range got {
			if c.Author != wantAuthors[i] {
				t.Errorf("comment[%d].Author = %q, want %q", i, c.Author, wantAuthors[i])
			}
		}
	})

	t.Run("body passes through unchanged", func(t *testing.T) {
		t.Parallel()

		md := "Line one\n\n- bullet\n\nhttps://linear.app/org/issue/SOR-5"
		got := normalizeComments([]linearComment{{ID: "c1", Body: md, CreatedAt: "t1"}})
		if got[0].Body != md {
			t.Errorf("comment body = %q, want %q (markdown pass-through)", got[0].Body, md)
		}
	})

	t.Run("no comments yields non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		got := normalizeComments(nil)
		if got == nil {
			t.Error("normalizeComments(nil) = nil, want non-nil empty slice")
		}
		if len(got) != 0 {
			t.Errorf("normalizeComments(nil) = %v, want empty", got)
		}
	})
}

func TestSortCommentsByCreatedAt(t *testing.T) {
	t.Parallel()

	comments := []domain.Comment{
		{ID: "newest", CreatedAt: "2026-06-12T12:00:00.000Z"},
		{ID: "oldest", CreatedAt: "2026-06-10T12:00:00.000Z"},
		{ID: "middle", CreatedAt: "2026-06-11T12:00:00.000Z"},
	}

	sortCommentsByCreatedAt(comments)

	want := []string{"oldest", "middle", "newest"}
	if got := commentIDs(comments); !reflect.DeepEqual(got, want) {
		t.Errorf("sortCommentsByCreatedAt order = %v, want %v", got, want)
	}
}

func TestSortByPriorityThenCreated(t *testing.T) {
	t.Parallel()

	issues := []domain.Issue{
		{Identifier: "no-prio-older", Priority: nil, CreatedAt: "2026-06-01"},
		{Identifier: "prio2", Priority: ptrInt(2), CreatedAt: "2026-06-05"},
		{Identifier: "prio1-newer", Priority: ptrInt(1), CreatedAt: "2026-06-10"},
		{Identifier: "prio1-older", Priority: ptrInt(1), CreatedAt: "2026-06-02"},
		{Identifier: "no-prio-newer", Priority: nil, CreatedAt: "2026-06-09"},
	}

	sortByPriorityThenCreated(issues)

	want := []string{"prio1-older", "prio1-newer", "prio2", "no-prio-older", "no-prio-newer"}
	if got := identifiers(issues); !reflect.DeepEqual(got, want) {
		t.Errorf("sortByPriorityThenCreated order = %v, want %v (priority asc, nil last, then createdAt asc)", got, want)
	}
}

func TestNormalizeNestedOverflowWarn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		labelsNext     bool
		relationsNext  bool
		wantConnection []string
	}{
		{"labels overflow", true, false, []string{"labels"}},
		{"inverseRelations overflow", false, true, []string{"inverseRelations"}},
		{"both overflow", true, true, []string{"labels", "inverseRelations"}},
		{"no overflow", false, false, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			log := newTextLogger(&buf)

			li := linearIssue{
				Identifier:       "SOR-42",
				Labels:           linearLabelConn{Nodes: []linearLabel{{Name: "a"}}, PageInfo: linearPageInfo{HasNextPage: tt.labelsNext}},
				InverseRelations: linearRelationConn{Nodes: []linearRelation{{Type: "blocks", Issue: linearRelatedIssue{Identifier: "SOR-5"}}}, PageInfo: linearPageInfo{HasNextPage: tt.relationsNext}},
			}

			got := normalizeIssue(li, log)

			if len(got.Labels) != 1 {
				t.Errorf("Labels len = %d, want 1 (truncated set returned)", len(got.Labels))
			}
			if len(got.BlockedBy) != 1 {
				t.Errorf("BlockedBy len = %d, want 1 (truncated set returned)", len(got.BlockedBy))
			}

			output := buf.String()
			warnCount := strings.Count(output, "level=WARN")
			if warnCount != len(tt.wantConnection) {
				t.Errorf("WARN count = %d, want %d\noutput: %s", warnCount, len(tt.wantConnection), output)
			}
			for _, conn := range tt.wantConnection {
				if !strings.Contains(output, "connection="+conn) {
					t.Errorf("WARN output missing connection=%s\noutput: %s", conn, output)
				}
			}
			if len(tt.wantConnection) > 0 && !strings.Contains(output, "issue_identifier=SOR-42") {
				t.Errorf("WARN output missing issue_identifier=SOR-42\noutput: %s", output)
			}
		})
	}
}

func TestNormalizeIssueNilLoggerNoWarn(t *testing.T) {
	t.Parallel()

	li := linearIssue{
		Identifier: "SOR-42",
		Labels:     linearLabelConn{PageInfo: linearPageInfo{HasNextPage: true}},
	}

	got := normalizeIssue(li, nil)
	if got.Identifier != "SOR-42" {
		t.Errorf("Identifier = %q, want %q (nil logger must not panic)", got.Identifier, "SOR-42")
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
