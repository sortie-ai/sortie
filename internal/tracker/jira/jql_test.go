package jira

import "testing"

func TestEscapeJQLString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no change", "Done", "Done"},
		{"strips embedded quote", `Done"or`, "Doneor"},
		{"all quotes", `"""`, ""},
		{"special chars preserved", "In Progress (Old)", "In Progress (Old)"},
		{"empty string", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := escapeJQLString(tt.input)
			if got != tt.want {
				t.Errorf("escapeJQLString(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildStatusIN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		states []string
		want   string
	}{
		{"single state", []string{"To Do"}, `"To Do"`},
		{"multiple states", []string{"To Do", "In Progress"}, `"To Do", "In Progress"`},
		{"empty", []string{}, ""},
		{"state with quotes", []string{`Done"or`}, `"Doneor"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildStatusIN(tt.states)
			if got != tt.want {
				t.Errorf("buildStatusIN(%v) = %q, want %q", tt.states, got, tt.want)
			}
		})
	}
}

func TestBuildCandidateJQL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		project     string
		states      []string
		queryFilter string
		want        string
	}{
		{
			name:    "basic",
			project: "PROJ",
			states:  []string{"To Do", "In Progress"},
			want:    `project = "PROJ" AND status IN ("To Do", "In Progress") ORDER BY priority ASC, created ASC`,
		},
		{
			name:        "with query filter",
			project:     "PROJ",
			states:      []string{"To Do"},
			queryFilter: "component = 'api' OR component = 'web'",
			want:        `project = "PROJ" AND status IN ("To Do") AND (component = 'api' OR component = 'web') ORDER BY priority ASC, created ASC`,
		},
		{
			name:    "state with embedded quote",
			project: "PROJ",
			states:  []string{`Do"ne`},
			want:    `project = "PROJ" AND status IN ("Done") ORDER BY priority ASC, created ASC`,
		},
		{
			name:    "empty states",
			project: "PROJ",
			states:  []string{},
			want:    `project = "PROJ" AND status IN () ORDER BY priority ASC, created ASC`,
		},
		{
			name:    "project with quote",
			project: `PRO"J`,
			states:  []string{"To Do"},
			want:    `project = "PROJ" AND status IN ("To Do") ORDER BY priority ASC, created ASC`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildCandidateJQL(tt.project, tt.states, tt.queryFilter)
			if got != tt.want {
				t.Errorf("buildCandidateJQL() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestBuildStatesFetchJQL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		project     string
		states      []string
		queryFilter string
		want        string
	}{
		{
			name:    "basic",
			project: "PROJ",
			states:  []string{"Done"},
			want:    `project = "PROJ" AND status IN ("Done") ORDER BY created ASC`,
		},
		{
			name:        "with query filter",
			project:     "PROJ",
			states:      []string{"Done"},
			queryFilter: "label = 'critical'",
			want:        `project = "PROJ" AND status IN ("Done") AND (label = 'critical') ORDER BY created ASC`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildStatesFetchJQL(tt.project, tt.states, tt.queryFilter)
			if got != tt.want {
				t.Errorf("buildStatesFetchJQL() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestBuildKeyINJQL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		keys []string
		want string
	}{
		{
			name: "two keys",
			keys: []string{"PROJ-1", "PROJ-2"},
			want: `key IN ("PROJ-1", "PROJ-2") ORDER BY key ASC`,
		},
		{
			name: "single key",
			keys: []string{"PROJ-1"},
			want: `key IN ("PROJ-1") ORDER BY key ASC`,
		},
		{
			name: "key with quote sanitized",
			keys: []string{`PROJ-"1`},
			want: `key IN ("PROJ-1") ORDER BY key ASC`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildKeyINJQL(tt.keys)
			if got != tt.want {
				t.Errorf("buildKeyINJQL(%v) =\n  %q\nwant\n  %q", tt.keys, got, tt.want)
			}
		})
	}
}
