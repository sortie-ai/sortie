package orchestrator

import (
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
)

// HostPool manages SSH host allocation for dispatch. Not safe for
// concurrent access; all methods must be called from the event loop.
type HostPool struct {
	hosts       []string
	maxPerHost  int
	usage       map[string]int
	assignments map[string]string
}

// NewHostPool creates a HostPool from config. If hosts is nil or
// empty, the pool operates in local mode: AcquireHost always returns
// ("", true) and ReleaseHost is a no-op.
func NewHostPool(hosts []string, maxPerHost int) *HostPool {
	deduped := deduplicateHosts(hosts)
	usage := make(map[string]int, len(deduped))
	for _, h := range deduped {
		usage[h] = 0
	}
	return &HostPool{
		hosts:       deduped,
		maxPerHost:  maxPerHost,
		usage:       usage,
		assignments: make(map[string]string),
	}
}

// AcquireHost selects a host for the given issue and increments its
// usage counter. In local mode (no SSH hosts configured), returns
// ("", true). When preferredHost is non-empty and has capacity, it is
// selected regardless of load (retry affinity). Otherwise, selects the
// least-loaded host below the per-host cap; ties broken by list
// position. Returns ("", false) when all hosts are at capacity.
func (hp *HostPool) AcquireHost(issueID, preferredHost string) (string, bool) {
	if len(hp.hosts) == 0 {
		return "", true
	}

	// Retry affinity: prefer same host when it is still configured
	// and has capacity. A host removed by config reload is not eligible
	// even if it still has tracked usage from in-flight workers.
	if preferredHost != "" && hp.isConfigured(preferredHost) && hp.hasHostCapacity(preferredHost) {
		hp.assign(issueID, preferredHost)
		return preferredHost, true
	}

	// Least-loaded selection, ties broken by list position.
	bestHost := ""
	bestLoad := -1
	for _, h := range hp.hosts {
		load := hp.usage[h]
		if hp.maxPerHost > 0 && load >= hp.maxPerHost {
			continue
		}
		if bestHost == "" || load < bestLoad {
			bestHost = h
			bestLoad = load
		}
	}

	if bestHost == "" {
		return "", false
	}

	hp.assign(issueID, bestHost)
	return bestHost, true
}

// ReleaseHost decrements the usage counter for the host assigned to
// issueID and removes the assignment. No-op if issueID has no
// assignment (local mode or already released).
func (hp *HostPool) ReleaseHost(issueID string) {
	host, ok := hp.assignments[issueID]
	if !ok {
		return
	}
	delete(hp.assignments, issueID)
	if hp.usage[host] > 0 {
		hp.usage[host]--
	}
}

// HostFor returns the host assigned to issueID, or "" if none.
func (hp *HostPool) HostFor(issueID string) string {
	return hp.assignments[issueID]
}

// IsSSHEnabled reports whether the pool has configured SSH hosts.
func (hp *HostPool) IsSSHEnabled() bool {
	return len(hp.hosts) > 0
}

// HasCapacity reports whether at least one host has capacity for a
// new worker. In local mode, always returns true.
func (hp *HostPool) HasCapacity() bool {
	if len(hp.hosts) == 0 {
		return true
	}
	return slices.ContainsFunc(hp.hosts, hp.hasHostCapacity)
}

// Update replaces the host list and per-host cap from a new config
// snapshot. Active assignments are preserved. Hosts removed from
// config that still have active workers continue to be tracked until
// those workers exit.
func (hp *HostPool) Update(hosts []string, maxPerHost int) {
	deduped := deduplicateHosts(hosts)
	hp.maxPerHost = maxPerHost
	hp.hosts = deduped

	for _, h := range deduped {
		if _, ok := hp.usage[h]; !ok {
			hp.usage[h] = 0
		}
	}

	// Prune usage entries for hosts that are no longer configured and
	// have no active assignments. This prevents unbounded growth in the
	// usage map (and downstream metric label cardinality) when hosts are
	// repeatedly added and removed via dynamic config reload.
	configured := make(map[string]struct{}, len(deduped))
	for _, h := range deduped {
		configured[h] = struct{}{}
	}
	assigned := make(map[string]struct{}, len(hp.assignments))
	for _, host := range hp.assignments {
		assigned[host] = struct{}{}
	}
	for host := range hp.usage {
		if _, ok := configured[host]; ok {
			continue
		}
		if _, ok := assigned[host]; ok {
			continue
		}
		delete(hp.usage, host)
	}
}

// Snapshot returns a copy of the usage map for observability.
func (hp *HostPool) Snapshot() map[string]int {
	snap := make(map[string]int, len(hp.usage))
	maps.Copy(snap, hp.usage)
	return snap
}

func (hp *HostPool) isConfigured(host string) bool {
	return slices.Contains(hp.hosts, host)
}

func (hp *HostPool) hasHostCapacity(host string) bool {
	if hp.maxPerHost <= 0 {
		return true
	}
	return hp.usage[host] < hp.maxPerHost
}

func (hp *HostPool) assign(issueID, host string) {
	hp.assignments[issueID] = host
	hp.usage[host]++
}

// deduplicateHosts returns a copy of hosts with duplicates removed,
// preserving order. Empty strings are skipped.
func deduplicateHosts(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(hosts))
	deduped := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		deduped = append(deduped, h)
	}
	if len(deduped) == 0 {
		return nil
	}
	return deduped
}

// WorkerWarning is a structured validation diagnostic produced by
// [ParseWorkerConfig]. It carries a stable log message and typed
// slog attributes so the caller can emit it through its scoped
// logger without interpolating variable data into the message string.
type WorkerWarning struct {
	Message string
	Attrs   []slog.Attr
}

// WorkerConfig holds parsed worker extension configuration.
type WorkerConfig struct {
	// SSHHosts is the list of SSH host strings for remote dispatch.
	// Nil or empty means local mode.
	SSHHosts []string

	// MaxPerHost is the per-host concurrency cap. Zero means unlimited.
	MaxPerHost int

	// SSHStrictHostKeyChecking is the OpenSSH StrictHostKeyChecking
	// value used when building SSH arguments for agent adapters.
	// Valid values: "accept-new", "yes", "no". Empty means "accept-new".
	SSHStrictHostKeyChecking string

	// SSHPassEnv lists the valid entries of worker.ssh_pass_env, in
	// declared order, deduplicated to first occurrence. Nil when
	// absent, empty, or entirely invalid.
	SSHPassEnv []string

	// SSHDisallowPassEnv lists the valid entries of
	// worker.ssh_disallow_pass_env, in declared order, deduplicated to
	// first occurrence. Nil when absent, empty, or entirely invalid.
	SSHDisallowPassEnv []string

	// Warnings contains structured validation diagnostics produced
	// during parsing. Empty when all values are valid or absent.
	// The caller logs these through its scoped logger after
	// change-detection.
	Warnings []WorkerWarning
}

// ParseWorkerConfig parses the worker extension section. Returns a
// [WorkerConfig] with SSH host list, per-host concurrency cap, and SSH
// StrictHostKeyChecking behavior. When workerSection is nil, returns
// zero-value defaults (local mode).
func ParseWorkerConfig(workerSection map[string]any) WorkerConfig {
	if workerSection == nil {
		return WorkerConfig{}
	}

	var hosts []string
	if rawHosts, ok := workerSection["ssh_hosts"]; ok {
		if hostList, ok := rawHosts.([]any); ok {
			for _, h := range hostList {
				if s, ok := h.(string); ok && s != "" {
					hosts = append(hosts, s)
				}
			}
		}
	}

	var maxPerHost int
	if rawMax, ok := workerSection["max_concurrent_agents_per_host"]; ok {
		switch v := rawMax.(type) {
		case int:
			if v > 0 {
				maxPerHost = v
			}
		case float64:
			if v > 0 && v == float64(int(v)) {
				maxPerHost = int(v)
			}
		}
	}

	strictHostKeyChecking, warn := parseSSHStrictHostKeyChecking(workerSection)

	var warnings []WorkerWarning
	if warn != nil {
		warnings = append(warnings, *warn)
	}

	listed, listedWarnings := nameList(workerSection, "ssh_pass_env",
		"received non-list ssh_pass_env, carrying no variables",
		"ignored ssh_pass_env entry that is not an environment variable name")
	disallowed, disallowedWarnings := nameList(workerSection, "ssh_disallow_pass_env",
		"received non-list ssh_disallow_pass_env, disallowing no variables",
		"ignored ssh_disallow_pass_env entry that is not an environment variable name")
	warnings = append(warnings, listedWarnings...)
	warnings = append(warnings, disallowedWarnings...)

	hosts = deduplicateHosts(hosts)

	if len(hosts) > 0 {
		for _, name := range listed {
			if slices.Contains(disallowed, name) {
				warnings = append(warnings, WorkerWarning{
					Message: "ssh_pass_env variable is disallowed by ssh_disallow_pass_env, not carrying it",
					Attrs:   []slog.Attr{slog.String("variable", name)},
				})
				continue
			}
			if value, present := os.LookupEnv(name); !present || value == "" {
				warnings = append(warnings, WorkerWarning{
					Message: "ssh_pass_env variable is not set or empty in the orchestrator environment",
					Attrs:   []slog.Attr{slog.String("variable", name)},
				})
			}
		}
	}

	return WorkerConfig{
		SSHHosts:                 hosts,
		MaxPerHost:               maxPerHost,
		SSHStrictHostKeyChecking: strictHostKeyChecking,
		SSHPassEnv:               listed,
		SSHDisallowPassEnv:       disallowed,
		Warnings:                 warnings,
	}
}

// nameList extracts and validates the environment variable name list
// under key in workerSection. An absent key or an explicit null
// returns (nil, nil). A value that is not a list produces
// nonListMessage and (nil, warnings). Each element that is not a
// string or fails [sshutil.IsEnvName] produces entryMessage carrying
// only its index, never its text, and is skipped. A valid name
// already seen is dropped, keeping the first occurrence.
func nameList(workerSection map[string]any, key, nonListMessage, entryMessage string) ([]string, []WorkerWarning) {
	raw, present := workerSection[key]
	if !present || raw == nil {
		return nil, nil
	}

	rawList, ok := raw.([]any)
	if !ok {
		return nil, []WorkerWarning{{Message: nonListMessage}}
	}

	var names []string
	var warnings []WorkerWarning
	for i, element := range rawList {
		s, ok := element.(string)
		if !ok || !sshutil.IsEnvName(s) {
			warnings = append(warnings, WorkerWarning{
				Message: entryMessage,
				Attrs:   []slog.Attr{slog.Int("index", i)},
			})
			continue
		}
		if !slices.Contains(names, s) {
			names = append(names, s)
		}
	}
	return names, warnings
}

// carriedEnvNames returns the union of declared then listed, in
// order, deduplicated to first occurrence, less any name in
// disallowed. Nil when the result is empty.
func carriedEnvNames(declared, listed, disallowed []string) []string {
	var names []string
	add := func(name string) {
		if slices.Contains(disallowed, name) || slices.Contains(names, name) {
			return
		}
		names = append(names, name)
	}
	for _, name := range declared {
		add(name)
	}
	for _, name := range listed {
		add(name)
	}
	return names
}

// parseSSHStrictHostKeyChecking extracts and validates the
// ssh_strict_host_key_checking value from the worker extension map.
// Returns the normalized value (one of "accept-new", "yes", "no", or
// empty for default) and a structured diagnostic. The diagnostic is
// non-nil when the raw value has the wrong type or is unrecognized.
func parseSSHStrictHostKeyChecking(workerMap map[string]any) (string, *WorkerWarning) {
	raw, ok := workerMap["ssh_strict_host_key_checking"]
	if !ok {
		return "", nil
	}

	s, ok := raw.(string)
	if !ok {
		return "", &WorkerWarning{
			Message: "received non-string ssh_strict_host_key_checking, using default",
			Attrs:   []slog.Attr{slog.String("default", "accept-new")},
		}
	}

	normalized := strings.ToLower(strings.TrimSpace(s))
	switch normalized {
	case "accept-new", "yes", "no":
		return normalized, nil
	default:
		return "", &WorkerWarning{
			Message: "rejected unrecognized ssh_strict_host_key_checking value",
			Attrs:   []slog.Attr{slog.String("value", s), slog.String("default", "accept-new")},
		}
	}
}

func workerWarningsEqual(a, b []WorkerWarning) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Message != b[i].Message {
			return false
		}
		if len(a[i].Attrs) != len(b[i].Attrs) {
			return false
		}
		for j := range a[i].Attrs {
			if a[i].Attrs[j].Key != b[i].Attrs[j].Key {
				return false
			}
			if !a[i].Attrs[j].Value.Equal(b[i].Attrs[j].Value) {
				return false
			}
		}
	}
	return true
}
