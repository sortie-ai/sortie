package orchestrator

import (
	"log/slog"
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

	// At capacity.
	if hp.HasCapacity() {
		t.Error("HasCapacity() = true before release, want false")
	}

	hp.ReleaseHost("ISS-1")

	// Capacity restored.
	if !hp.HasCapacity() {
		t.Error("HasCapacity() = false after release, want true")
	}

	// Assignment cleared.
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

	// Update to new hosts and cap.
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
		extensions                map[string]any
		wantHosts                 []string
		wantMaxPerHost            int
		wantSSHStrictHostKeyCheck string
		wantWarnings              []WorkerWarning
	}{
		{
			name:       "nil extensions",
			extensions: nil,
		},
		{
			name:       "missing worker key",
			extensions: map[string]any{"other": "value"},
		},
		{
			name:       "worker not a map",
			extensions: map[string]any{"worker": "invalid"},
		},
		{
			name: "empty ssh_hosts",
			extensions: map[string]any{
				"worker": map[string]any{
					"ssh_hosts": []any{},
				},
			},
		},
		{
			name: "valid config",
			extensions: map[string]any{
				"worker": map[string]any{
					"ssh_hosts":                      []any{"host-a", "host-b"},
					"max_concurrent_agents_per_host": 3,
				},
			},
			wantHosts:      []string{"host-a", "host-b"},
			wantMaxPerHost: 3,
		},
		{
			name: "float64 max_concurrent_agents_per_host",
			extensions: map[string]any{
				"worker": map[string]any{
					"ssh_hosts":                      []any{"host-a"},
					"max_concurrent_agents_per_host": float64(2),
				},
			},
			wantHosts:      []string{"host-a"},
			wantMaxPerHost: 2,
		},
		{
			name: "deduplicates hosts",
			extensions: map[string]any{
				"worker": map[string]any{
					"ssh_hosts": []any{"host-a", "host-a", "host-b"},
				},
			},
			wantHosts: []string{"host-a", "host-b"},
		},
		{
			name: "skips empty and non-string hosts",
			extensions: map[string]any{
				"worker": map[string]any{
					"ssh_hosts": []any{"", 42, "host-a"},
				},
			},
			wantHosts: []string{"host-a"},
		},
		// ssh_strict_host_key_checking cases
		{
			name: "absent ssh_strict_host_key_checking",
			extensions: map[string]any{
				"worker": map[string]any{"ssh_hosts": []any{"host-a"}},
			},
			wantHosts:                 []string{"host-a"},
			wantSSHStrictHostKeyCheck: "",
		},
		{
			name: "valid accept-new",
			extensions: map[string]any{
				"worker": map[string]any{
					"ssh_strict_host_key_checking": "accept-new",
				},
			},
			wantSSHStrictHostKeyCheck: "accept-new",
		},
		{
			name: "valid yes",
			extensions: map[string]any{
				"worker": map[string]any{
					"ssh_strict_host_key_checking": "yes",
				},
			},
			wantSSHStrictHostKeyCheck: "yes",
		},
		{
			name: "valid no",
			extensions: map[string]any{
				"worker": map[string]any{
					"ssh_strict_host_key_checking": "no",
				},
			},
			wantSSHStrictHostKeyCheck: "no",
		},
		{
			name: "uppercase YES normalized to yes",
			extensions: map[string]any{
				"worker": map[string]any{
					"ssh_strict_host_key_checking": "YES",
				},
			},
			wantSSHStrictHostKeyCheck: "yes",
		},
		{
			name: "invalid string falls back to empty",
			extensions: map[string]any{
				"worker": map[string]any{
					"ssh_strict_host_key_checking": "ask",
				},
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
			extensions: map[string]any{
				"worker": map[string]any{
					"ssh_strict_host_key_checking": 42,
				},
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

			wc := ParseWorkerConfig(tt.extensions)

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

	// Remove "b" from the configured host list.
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
