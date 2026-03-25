package orchestrator

import (
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
		name           string
		extensions     map[string]any
		wantHosts      []string
		wantMaxPerHost int
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hosts, maxPerHost := parseWorkerConfig(tt.extensions)

			if len(hosts) != len(tt.wantHosts) {
				t.Fatalf("parseWorkerConfig() hosts = %v, want %v", hosts, tt.wantHosts)
			}
			for i := range hosts {
				if hosts[i] != tt.wantHosts[i] {
					t.Errorf("hosts[%d] = %q, want %q", i, hosts[i], tt.wantHosts[i])
				}
			}
			if maxPerHost != tt.wantMaxPerHost {
				t.Errorf("parseWorkerConfig() maxPerHost = %d, want %d", maxPerHost, tt.wantMaxPerHost)
			}
		})
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
