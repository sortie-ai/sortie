package orchestrator

import (
	"log/slog"
	"strings"
)

// HostPool manages SSH host allocation for dispatch. Not safe for
// concurrent access — all methods must be called from the event loop.
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
	for _, h := range hp.hosts {
		if hp.hasHostCapacity(h) {
			return true
		}
	}
	return false
}

// Update replaces the host list and per-host cap from a new config
// snapshot. Active assignments are preserved. Hosts removed from
// config that still have active workers continue to be tracked until
// those workers exit.
func (hp *HostPool) Update(hosts []string, maxPerHost int) {
	deduped := deduplicateHosts(hosts)
	hp.maxPerHost = maxPerHost
	hp.hosts = deduped

	// Initialize usage for new hosts.
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
	for h, c := range hp.usage {
		snap[h] = c
	}
	return snap
}

// isConfigured reports whether host is in the current host list.
func (hp *HostPool) isConfigured(host string) bool {
	for _, h := range hp.hosts {
		if h == host {
			return true
		}
	}
	return false
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
	result := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		result = append(result, h)
	}
	if len(result) == 0 {
		return nil
	}
	return result
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
}

// ParseWorkerConfig parses worker extension configuration from the
// Extensions map. Returns a [WorkerConfig] with SSH host list,
// per-host concurrency cap, and SSH StrictHostKeyChecking behavior.
// When the worker key is absent or malformed, returns zero-value
// defaults (local mode).
func ParseWorkerConfig(extensions map[string]any) WorkerConfig {
	if extensions == nil {
		return WorkerConfig{}
	}
	workerRaw, ok := extensions["worker"]
	if !ok {
		return WorkerConfig{}
	}
	workerMap, ok := workerRaw.(map[string]any)
	if !ok {
		return WorkerConfig{}
	}

	var hosts []string
	if rawHosts, ok := workerMap["ssh_hosts"]; ok {
		if hostList, ok := rawHosts.([]any); ok {
			for _, h := range hostList {
				if s, ok := h.(string); ok && s != "" {
					hosts = append(hosts, s)
				}
			}
		}
	}

	var maxPerHost int
	if rawMax, ok := workerMap["max_concurrent_agents_per_host"]; ok {
		switch v := rawMax.(type) {
		case int:
			if v > 0 {
				maxPerHost = v
			}
		case float64:
			if int(v) > 0 {
				maxPerHost = int(v)
			}
		}
	}

	strictHostKeyChecking := parseSSHStrictHostKeyChecking(workerMap)

	hosts = deduplicateHosts(hosts)
	return WorkerConfig{
		SSHHosts:                 hosts,
		MaxPerHost:               maxPerHost,
		SSHStrictHostKeyChecking: strictHostKeyChecking,
	}
}

// parseSSHStrictHostKeyChecking extracts and validates the
// ssh_strict_host_key_checking value from the worker extension map.
// Returns one of "accept-new", "yes", "no", or empty string (meaning
// the caller should use the default "accept-new" behavior).
func parseSSHStrictHostKeyChecking(workerMap map[string]any) string {
	raw, ok := workerMap["ssh_strict_host_key_checking"]
	if !ok {
		return ""
	}

	s, ok := raw.(string)
	if !ok {
		slog.Warn("ssh_strict_host_key_checking must be a string, using default",
			slog.String("default", "accept-new"),
		)
		return ""
	}

	normalized := strings.ToLower(strings.TrimSpace(s))
	switch normalized {
	case "accept-new", "yes", "no":
		return normalized
	default:
		slog.Warn("invalid ssh_strict_host_key_checking value, using default",
			slog.String("value", s),
			slog.String("default", "accept-new"),
		)
		return ""
	}
}
