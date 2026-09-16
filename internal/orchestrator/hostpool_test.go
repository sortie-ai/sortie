package orchestrator

import (
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestNewHostPool_LocalMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		hosts      []string
		maxPerHost int
	}{
		{"nil hosts", nil, 0},
		{"empty hosts", []string{}, 0},
		{"all empty strings", []string{"", ""}, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hp := NewHostPool(tt.hosts, tt.maxPerHost)
			if hp.IsSSHEnabled() {
				t.Error("IsSSHEnabled() = true, want false for local mode")
			}
			if !hp.HasCapacity() {
				t.Error("HasCapacity() = false, want true for local mode")
			}
		})
	}
}

func TestNewHostPool_SSHMode(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"host-a", "host-b"}, 2)
	if !hp.IsSSHEnabled() {
		t.Error("IsSSHEnabled() = false, want true")
	}
}

func TestAcquireHost_LocalMode(t *testing.T) {
	t.Parallel()

	hp := NewHostPool(nil, 0)
	host, ok := hp.AcquireHost("ISS-1", "")
	if !ok {
		t.Error("AcquireHost() ok = false, want true in local mode")
	}
	if host != "" {
		t.Errorf("AcquireHost() host = %q, want empty in local mode", host)
	}
}

func TestAcquireHost_LeastLoaded(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"a", "b", "c"}, 2)

	// First acquire should pick "a" (all zero, first in list).
	host, ok := hp.AcquireHost("ISS-1", "")
	if !ok || host != "a" {
		t.Errorf("AcquireHost(ISS-1) = (%q, %v), want (\"a\", true)", host, ok)
	}

	// Second acquire should pick "b" (tied at 0, "a" has 1).
	host, ok = hp.AcquireHost("ISS-2", "")
	if !ok || host != "b" {
		t.Errorf("AcquireHost(ISS-2) = (%q, %v), want (\"b\", true)", host, ok)
	}

	// Third should pick "c".
	host, ok = hp.AcquireHost("ISS-3", "")
	if !ok || host != "c" {
		t.Errorf("AcquireHost(ISS-3) = (%q, %v), want (\"c\", true)", host, ok)
	}

	// Fourth should pick "a" again (all tied at 1, first in list).
	host, ok = hp.AcquireHost("ISS-4", "")
	if !ok || host != "a" {
		t.Errorf("AcquireHost(ISS-4) = (%q, %v), want (\"a\", true)", host, ok)
	}
}

func TestAcquireHost_PreferredWithCapacity(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"a", "b"}, 2)
	// Load "a" with one issue.
	hp.AcquireHost("ISS-0", "")

	// Prefer "a" even though "b" is less loaded.
	host, ok := hp.AcquireHost("ISS-1", "a")
	if !ok || host != "a" {
		t.Errorf("AcquireHost(ISS-1, preferred=a) = (%q, %v), want (\"a\", true)", host, ok)
	}
}

func TestAcquireHost_PreferredAtCapacityFallsBack(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"a", "b"}, 1)
	hp.AcquireHost("ISS-0", "a") // "a" is now at capacity

	// Prefer "a", but it's full → should fall back to "b".
	host, ok := hp.AcquireHost("ISS-1", "a")
	if !ok || host != "b" {
		t.Errorf("AcquireHost(ISS-1, preferred=a) = (%q, %v), want (\"b\", true)", host, ok)
	}
}

func TestAcquireHost_AllAtCapacity(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"a"}, 1)
	hp.AcquireHost("ISS-0", "")

	host, ok := hp.AcquireHost("ISS-1", "")
	if ok {
		t.Errorf("AcquireHost() = (%q, true), want (\"\", false) when all at capacity", host)
	}
}

func TestReleaseHost(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"a"}, 1)
	hp.AcquireHost("ISS-1", "")

	if hp.HasCapacity() {
		t.Error("HasCapacity() = true before release, want false")
	}

	hp.ReleaseHost("ISS-1")

	if !hp.HasCapacity() {
		t.Error("HasCapacity() = false after release, want true")
	}

	if got := hp.HostFor("ISS-1"); got != "" {
		t.Errorf("HostFor(ISS-1) = %q after release, want empty", got)
	}
}

func TestReleaseHost_UnknownIssue(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"a"}, 2)
	// Should not panic.
	hp.ReleaseHost("ISS-UNKNOWN")
}

func TestHostFor(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"a", "b"}, 2)

	if got := hp.HostFor("ISS-1"); got != "" {
		t.Errorf("HostFor(ISS-1) = %q before acquire, want empty", got)
	}

	hp.AcquireHost("ISS-1", "")
	if got := hp.HostFor("ISS-1"); got != "a" {
		t.Errorf("HostFor(ISS-1) = %q after acquire, want \"a\"", got)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"a"}, 1)
	hp.AcquireHost("ISS-1", "")

	hp.Update([]string{"a", "b"}, 3)

	if !hp.HasCapacity() {
		t.Error("HasCapacity() = false after Update, want true")
	}

	// Existing assignment preserved.
	if got := hp.HostFor("ISS-1"); got != "a" {
		t.Errorf("HostFor(ISS-1) = %q after Update, want \"a\"", got)
	}

	// New host is available.
	host, ok := hp.AcquireHost("ISS-2", "b")
	if !ok || host != "b" {
		t.Errorf("AcquireHost(ISS-2, preferred=b) = (%q, %v), want (\"b\", true)", host, ok)
	}
}

func TestSnapshot(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"a", "b"}, 3)
	hp.AcquireHost("ISS-1", "a") // prefer "a"
	hp.AcquireHost("ISS-2", "a") // prefer "a"

	snap := hp.Snapshot()
	if snap["a"] != 2 {
		t.Errorf("Snapshot()[a] = %d, want 2", snap["a"])
	}
	if snap["b"] != 0 {
		t.Errorf("Snapshot()[b] = %d, want 0", snap["b"])
	}

	// Mutating snapshot does not affect pool.
	snap["a"] = 999
	snap2 := hp.Snapshot()
	if snap2["a"] != 2 {
		t.Error("Snapshot mutation leaked into pool state")
	}
}

func TestDeduplicateHosts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"all empty strings", []string{"", ""}, nil},
		{"no duplicates", []string{"a", "b"}, []string{"a", "b"}},
		{"with duplicates", []string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
		{"mixed empty and duplicates", []string{"a", "", "b", "a", ""}, []string{"a", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := deduplicateHosts(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("deduplicateHosts(%v) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("deduplicateHosts(%v)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseWorkerConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                      string
		workerSection             map[string]any
		wantHosts                 []string
		wantMaxPerHost            int
		wantSSHStrictHostKeyCheck string
		wantWarnings              []WorkerWarning
	}{
		{
			name:          "nil worker section",
			workerSection: nil,
		},
		{
			name: "empty ssh_hosts",
			workerSection: map[string]any{
				"ssh_hosts": []any{},
			},
		},
		{
			name: "valid config",
			workerSection: map[string]any{
				"ssh_hosts":                      []any{"host-a", "host-b"},
				"max_concurrent_agents_per_host": 3,
			},
			wantHosts:      []string{"host-a", "host-b"},
			wantMaxPerHost: 3,
		},
		{
			name: "float64 max_concurrent_agents_per_host",
			workerSection: map[string]any{
				"ssh_hosts":                      []any{"host-a"},
				"max_concurrent_agents_per_host": float64(2),
			},
			wantHosts:      []string{"host-a"},
			wantMaxPerHost: 2,
		},
		{
			name: "deduplicates hosts",
			workerSection: map[string]any{
				"ssh_hosts": []any{"host-a", "host-a", "host-b"},
			},
			wantHosts: []string{"host-a", "host-b"},
		},
		{
			name: "skips empty and non-string hosts",
			workerSection: map[string]any{
				"ssh_hosts": []any{"", 42, "host-a"},
			},
			wantHosts: []string{"host-a"},
		},
		// ssh_strict_host_key_checking cases
		{
			name:                      "absent ssh_strict_host_key_checking",
			workerSection:             map[string]any{"ssh_hosts": []any{"host-a"}},
			wantHosts:                 []string{"host-a"},
			wantSSHStrictHostKeyCheck: "",
		},
		{
			name: "valid accept-new",
			workerSection: map[string]any{
				"ssh_strict_host_key_checking": "accept-new",
			},
			wantSSHStrictHostKeyCheck: "accept-new",
		},
		{
			name: "valid yes",
			workerSection: map[string]any{
				"ssh_strict_host_key_checking": "yes",
			},
			wantSSHStrictHostKeyCheck: "yes",
		},
		{
			name: "valid no",
			workerSection: map[string]any{
				"ssh_strict_host_key_checking": "no",
			},
			wantSSHStrictHostKeyCheck: "no",
		},
		{
			name: "uppercase YES normalized to yes",
			workerSection: map[string]any{
				"ssh_strict_host_key_checking": "YES",
			},
			wantSSHStrictHostKeyCheck: "yes",
		},
		{
			name: "invalid string falls back to empty",
			workerSection: map[string]any{
				"ssh_strict_host_key_checking": "ask",
			},
			wantSSHStrictHostKeyCheck: "",
			wantWarnings: []WorkerWarning{
				{
					Message: "rejected unrecognized ssh_strict_host_key_checking value",
					Attrs:   []slog.Attr{slog.String("value", "ask"), slog.String("default", "accept-new")},
				},
			},
		},
		{
			name: "wrong type integer falls back to empty",
			workerSection: map[string]any{
				"ssh_strict_host_key_checking": 42,
			},
			wantSSHStrictHostKeyCheck: "",
			wantWarnings: []WorkerWarning{
				{
					Message: "received non-string ssh_strict_host_key_checking, using default",
					Attrs:   []slog.Attr{slog.String("default", "accept-new")},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wc := ParseWorkerConfig(tt.workerSection)

			if len(wc.SSHHosts) != len(tt.wantHosts) {
				t.Fatalf("ParseWorkerConfig() SSHHosts = %v, want %v", wc.SSHHosts, tt.wantHosts)
			}
			for i := range wc.SSHHosts {
				if wc.SSHHosts[i] != tt.wantHosts[i] {
					t.Errorf("SSHHosts[%d] = %q, want %q", i, wc.SSHHosts[i], tt.wantHosts[i])
				}
			}
			if wc.MaxPerHost != tt.wantMaxPerHost {
				t.Errorf("ParseWorkerConfig() MaxPerHost = %d, want %d", wc.MaxPerHost, tt.wantMaxPerHost)
			}
			if wc.SSHStrictHostKeyChecking != tt.wantSSHStrictHostKeyCheck {
				t.Errorf("ParseWorkerConfig() SSHStrictHostKeyChecking = %q, want %q", wc.SSHStrictHostKeyChecking, tt.wantSSHStrictHostKeyCheck)
			}
			if len(wc.Warnings) != len(tt.wantWarnings) {
				t.Fatalf("ParseWorkerConfig() Warnings count = %d, want %d", len(wc.Warnings), len(tt.wantWarnings))
			}
			for i, want := range tt.wantWarnings {
				if wc.Warnings[i].Message != want.Message {
					t.Errorf("Warnings[%d].Message = %q, want %q", i, wc.Warnings[i].Message, want.Message)
				}
				if len(wc.Warnings[i].Attrs) != len(want.Attrs) {
					t.Fatalf("Warnings[%d].Attrs count = %d, want %d", i, len(wc.Warnings[i].Attrs), len(want.Attrs))
				}
				for j, wantAttr := range want.Attrs {
					gotAttr := wc.Warnings[i].Attrs[j]
					if gotAttr.Key != wantAttr.Key {
						t.Errorf("Warnings[%d].Attrs[%d].Key = %q, want %q", i, j, gotAttr.Key, wantAttr.Key)
					}
					if gotAttr.Value.String() != wantAttr.Value.String() {
						t.Errorf("Warnings[%d].Attrs[%d].Value = %q, want %q", i, j, gotAttr.Value.String(), wantAttr.Value.String())
					}
				}
			}
		})
	}
}

// TestParseWorkerConfig_SSHPassEnv covers the ssh_pass_env and
// ssh_disallow_pass_env parsing and warning shapes: a non-list value
// under either key, a mixed-validity list under ssh_pass_env, and the
// interaction between the two lists once hosts are configured.
func TestParseWorkerConfig_SSHPassEnv(t *testing.T) {
	// Not parallel: uses t.Setenv.
	t.Setenv("SORTIE_TEST_SSH_PASS_ENV_GOOD", "some-value")
	t.Setenv("SORTIE_TEST_SSH_PASS_ENV_A", "some-value")
	t.Setenv("SORTIE_TEST_SSH_PASS_ENV_BLANK", " \t\r\n ")
	os.Unsetenv("SORTIE_TEST_SSH_PASS_ENV_UNSET") //nolint:errcheck // best-effort; the variable may already be absent

	tests := []struct {
		name                 string
		workerSection        map[string]any
		wantListed           []string
		wantDisallowed       []string
		wantWarningMessages  []string
		wantWarningIndexes   []int
		wantWarningVariables []string
	}{
		{
			name: "scalar ssh_pass_env yields the non-list warning and no names",
			workerSection: map[string]any{
				"ssh_pass_env": "not-a-list",
			},
			wantWarningMessages: []string{"received non-list ssh_pass_env, carrying no variables"},
		},
		{
			name: "mapping ssh_disallow_pass_env yields the non-list warning and no names",
			workerSection: map[string]any{
				"ssh_disallow_pass_env": map[string]any{"a": "b"},
			},
			wantWarningMessages: []string{"received non-list ssh_disallow_pass_env, disallowing no variables"},
		},
		{
			name: "mixed validity list without hosts yields entry warnings and names but no variable warning",
			workerSection: map[string]any{
				"ssh_pass_env": []any{
					"SORTIE_TEST_SSH_PASS_ENV_GOOD", 1, "", "$X", "X=v", "1X",
					"SORTIE_TEST_SSH_PASS_ENV_GOOD", "SORTIE_TEST_SSH_PASS_ENV_UNSET",
				},
			},
			wantListed: []string{"SORTIE_TEST_SSH_PASS_ENV_GOOD", "SORTIE_TEST_SSH_PASS_ENV_UNSET"},
			wantWarningMessages: []string{
				"ignored ssh_pass_env entry that is not an environment variable name",
				"ignored ssh_pass_env entry that is not an environment variable name",
				"ignored ssh_pass_env entry that is not an environment variable name",
				"ignored ssh_pass_env entry that is not an environment variable name",
				"ignored ssh_pass_env entry that is not an environment variable name",
			},
			wantWarningIndexes: []int{1, 2, 3, 4, 5},
		},
		{
			name: "mixed validity list with hosts adds the unset-variable warning",
			workerSection: map[string]any{
				"ssh_hosts": []any{"host-a"},
				"ssh_pass_env": []any{
					"SORTIE_TEST_SSH_PASS_ENV_GOOD", 1, "", "$X", "X=v", "1X",
					"SORTIE_TEST_SSH_PASS_ENV_GOOD", "SORTIE_TEST_SSH_PASS_ENV_UNSET",
				},
			},
			wantListed: []string{"SORTIE_TEST_SSH_PASS_ENV_GOOD", "SORTIE_TEST_SSH_PASS_ENV_UNSET"},
			wantWarningMessages: []string{
				"ignored ssh_pass_env entry that is not an environment variable name",
				"ignored ssh_pass_env entry that is not an environment variable name",
				"ignored ssh_pass_env entry that is not an environment variable name",
				"ignored ssh_pass_env entry that is not an environment variable name",
				"ignored ssh_pass_env entry that is not an environment variable name",
				"ssh_pass_env variable is not set or empty in the orchestrator environment",
			},
			wantWarningIndexes:   []int{1, 2, 3, 4, 5, -1},
			wantWarningVariables: []string{"", "", "", "", "", "SORTIE_TEST_SSH_PASS_ENV_UNSET"},
		},
		{
			name: "disallowed listed variable and a disallow-list entry warning, in order, with hosts configured",
			workerSection: map[string]any{
				"ssh_hosts":             []any{"host-a"},
				"ssh_pass_env":          []any{"SORTIE_TEST_SSH_PASS_ENV_A", "SORTIE_TEST_SSH_PASS_ENV_B", "SORTIE_TEST_SSH_PASS_ENV_C"},
				"ssh_disallow_pass_env": []any{"SORTIE_TEST_SSH_PASS_ENV_B", "B-1", "SORTIE_TEST_SSH_PASS_ENV_B"},
			},
			wantListed:     []string{"SORTIE_TEST_SSH_PASS_ENV_A", "SORTIE_TEST_SSH_PASS_ENV_B", "SORTIE_TEST_SSH_PASS_ENV_C"},
			wantDisallowed: []string{"SORTIE_TEST_SSH_PASS_ENV_B"},
			wantWarningMessages: []string{
				"ignored ssh_disallow_pass_env entry that is not an environment variable name",
				"ssh_pass_env variable is disallowed by ssh_disallow_pass_env, not carrying it",
				"ssh_pass_env variable is not set or empty in the orchestrator environment",
			},
			wantWarningIndexes:   []int{1, -1, -1},
			wantWarningVariables: []string{"", "SORTIE_TEST_SSH_PASS_ENV_B", "SORTIE_TEST_SSH_PASS_ENV_C"},
		},
		{
			name: "whitespace-only listed variable warns as an unset one does, with hosts configured",
			workerSection: map[string]any{
				"ssh_hosts":    []any{"host-a"},
				"ssh_pass_env": []any{"SORTIE_TEST_SSH_PASS_ENV_GOOD", "SORTIE_TEST_SSH_PASS_ENV_BLANK"},
			},
			wantListed: []string{"SORTIE_TEST_SSH_PASS_ENV_GOOD", "SORTIE_TEST_SSH_PASS_ENV_BLANK"},
			wantWarningMessages: []string{
				"ssh_pass_env variable is not set or empty in the orchestrator environment",
			},
			wantWarningIndexes:   []int{-1},
			wantWarningVariables: []string{"SORTIE_TEST_SSH_PASS_ENV_BLANK"},
		},
		{
			name: "a reserved ssh_pass_env name is dropped and named in its own warning",
			workerSection: map[string]any{
				"ssh_pass_env": []any{"_sortie_complete", "SORTIE_TEST_SSH_PASS_ENV_GOOD"},
			},
			wantListed: []string{"SORTIE_TEST_SSH_PASS_ENV_GOOD"},
			wantWarningMessages: []string{
				"ssh_pass_env variable is reserved by Sortie, not carrying it",
			},
			wantWarningVariables: []string{"_sortie_complete"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wc := ParseWorkerConfig(tt.workerSection)

			if !slices.Equal(wc.SSHPassEnv, tt.wantListed) {
				t.Errorf("ParseWorkerConfig(...).SSHPassEnv = %v, want %v", wc.SSHPassEnv, tt.wantListed)
			}
			if !slices.Equal(wc.SSHDisallowPassEnv, tt.wantDisallowed) {
				t.Errorf("ParseWorkerConfig(...).SSHDisallowPassEnv = %v, want %v", wc.SSHDisallowPassEnv, tt.wantDisallowed)
			}
			if len(wc.Warnings) != len(tt.wantWarningMessages) {
				t.Fatalf("ParseWorkerConfig(...).Warnings = %v, want %d warnings matching %v", wc.Warnings, len(tt.wantWarningMessages), tt.wantWarningMessages)
			}
			for i, wantMessage := range tt.wantWarningMessages {
				got := wc.Warnings[i]
				if got.Message != wantMessage {
					t.Errorf("Warnings[%d].Message = %q, want %q", i, got.Message, wantMessage)
				}
				if i < len(tt.wantWarningIndexes) && tt.wantWarningIndexes[i] >= 0 {
					wantIndexAttr := slog.Int("index", tt.wantWarningIndexes[i])
					if len(got.Attrs) != 1 || got.Attrs[0].Key != wantIndexAttr.Key || got.Attrs[0].Value.String() != wantIndexAttr.Value.String() {
						t.Errorf("Warnings[%d].Attrs = %v, want a single index attr = %d", i, got.Attrs, tt.wantWarningIndexes[i])
					}
				}
				if i < len(tt.wantWarningVariables) && tt.wantWarningVariables[i] != "" {
					wantVariableAttr := slog.String("variable", tt.wantWarningVariables[i])
					if len(got.Attrs) != 1 || got.Attrs[0].Key != wantVariableAttr.Key || got.Attrs[0].Value.String() != wantVariableAttr.Value.String() {
						t.Errorf("Warnings[%d].Attrs = %v, want a single variable attr = %q", i, got.Attrs, tt.wantWarningVariables[i])
					}
				}
			}
		})
	}
}

// TestParseWorkerConfig_SSHPassEnv_NoEntryTextLeaked asserts that no
// warning carries a malformed entry's own text, only its position: a
// pasted secret in ssh_pass_env must never reach a log record through
// an entry warning.
func TestParseWorkerConfig_SSHPassEnv_NoEntryTextLeaked(t *testing.T) {
	t.Parallel()

	const secret = "super-secret-should-never-be-logged"
	wc := ParseWorkerConfig(map[string]any{
		"ssh_pass_env": []any{secret},
	})

	for _, w := range wc.Warnings {
		if strings.Contains(w.Message, secret) {
			t.Errorf("warning message %q contains the malformed entry's text", w.Message)
		}
		for _, attr := range w.Attrs {
			if strings.Contains(attr.Value.String(), secret) {
				t.Errorf("warning attr %s=%q contains the malformed entry's text", attr.Key, attr.Value.String())
			}
		}
	}
}

// TestCarriedEnvNames pins the union-then-subtract shape: declared then
// listed names, in order, deduplicated to first occurrence, less any
// name in disallowed.
func TestCarriedEnvNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		declared   []string
		listed     []string
		disallowed []string
		want       []string
	}{
		{
			name:       "dedup and subtract",
			declared:   []string{"K1", "K2"},
			listed:     []string{"L", "K1", "D"},
			disallowed: []string{"K2", "D"},
			want:       []string{"K1", "L"},
		},
		{
			name: "nothing declared or listed yields nil",
			want: nil,
		},
		{
			name:     "declared name repeated in listed keeps first position",
			declared: []string{"A"},
			listed:   []string{"A", "B"},
			want:     []string{"A", "B"},
		},
		{
			name:       "fully disallowed yields nil",
			declared:   []string{"A"},
			listed:     []string{"B"},
			disallowed: []string{"A", "B"},
			want:       nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := carriedEnvNames(tt.declared, tt.listed, tt.disallowed)
			if !slices.Equal(got, tt.want) {
				t.Errorf("carriedEnvNames(%v, %v, %v) = %v, want %v", tt.declared, tt.listed, tt.disallowed, got, tt.want)
			}
		})
	}
}

func TestWorkerWarningsEqual(t *testing.T) {
	t.Parallel()

	attrValue := func(key, val string) slog.Attr { return slog.String(key, val) }

	tests := []struct {
		name string
		a    []WorkerWarning
		b    []WorkerWarning
		want bool
	}{
		{
			name: "both nil",
			a:    nil,
			b:    nil,
			want: true,
		},
		{
			name: "both empty",
			a:    []WorkerWarning{},
			b:    []WorkerWarning{},
			want: true,
		},
		{
			name: "nil vs empty",
			a:    nil,
			b:    []WorkerWarning{},
			want: true,
		},
		{
			name: "single warning identical",
			a: []WorkerWarning{
				{Message: "some warning", Attrs: []slog.Attr{attrValue("key", "val")}},
			},
			b: []WorkerWarning{
				{Message: "some warning", Attrs: []slog.Attr{attrValue("key", "val")}},
			},
			want: true,
		},
		{
			name: "same message different attr value",
			a: []WorkerWarning{
				{Message: "some warning", Attrs: []slog.Attr{attrValue("key", "val-a")}},
			},
			b: []WorkerWarning{
				{Message: "some warning", Attrs: []slog.Attr{attrValue("key", "val-b")}},
			},
			want: false,
		},
		{
			name: "different message",
			a: []WorkerWarning{
				{Message: "warning-a", Attrs: []slog.Attr{attrValue("k", "v")}},
			},
			b: []WorkerWarning{
				{Message: "warning-b", Attrs: []slog.Attr{attrValue("k", "v")}},
			},
			want: false,
		},
		{
			name: "different lengths",
			a: []WorkerWarning{
				{Message: "w", Attrs: []slog.Attr{attrValue("k", "v")}},
			},
			b:    []WorkerWarning{},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := workerWarningsEqual(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("workerWarningsEqual(%v, %v) = %t, want %t", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestAcquireHost_PreferredNotConfigured(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"a", "b"}, 2)
	// Load "a" so it is not empty.
	hp.AcquireHost("ISS-0", "a")

	hp.Update([]string{"a"}, 2)

	// Prefer "b", but it is no longer configured → fall back to "a".
	host, ok := hp.AcquireHost("ISS-1", "b")
	if !ok || host != "a" {
		t.Errorf("AcquireHost(ISS-1, preferred=b) = (%q, %v), want (\"a\", true)", host, ok)
	}
}

func TestUpdate_PrunesStaleHosts(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"a", "b"}, 2)

	// Acquire on "b" so it has an active assignment.
	hp.AcquireHost("ISS-1", "b")

	// Update to remove "b". It should remain in usage because ISS-1 is
	// still assigned there.
	hp.Update([]string{"a"}, 2)
	snap := hp.Snapshot()
	if _, ok := snap["b"]; !ok {
		t.Fatal("Snapshot() missing \"b\" after Update with active assignment")
	}

	// Release "b"'s assignment.
	hp.ReleaseHost("ISS-1")

	// Update again. Now "b" has no assignment and is not configured, so
	// it should be pruned from the usage map.
	hp.Update([]string{"a"}, 2)
	snap = hp.Snapshot()
	if _, ok := snap["b"]; ok {
		t.Errorf("Snapshot() still contains \"b\" after prune, want removed")
	}

	// "a" must still be present.
	if _, ok := snap["a"]; !ok {
		t.Error("Snapshot() missing \"a\" after prune")
	}
}

func TestHasCapacity_UnlimitedPerHost(t *testing.T) {
	t.Parallel()

	// maxPerHost=0 means unlimited.
	hp := NewHostPool([]string{"a"}, 0)
	for i := range 100 {
		_, ok := hp.AcquireHost(string(rune('A'+i)), "")
		if !ok {
			t.Fatalf("AcquireHost failed at iteration %d with maxPerHost=0 (unlimited)", i)
		}
	}
	if !hp.HasCapacity() {
		t.Error("HasCapacity() = false with maxPerHost=0 (unlimited)")
	}
}
