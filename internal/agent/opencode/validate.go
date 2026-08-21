package opencode

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// validateConfig checks opencode-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or launch a subprocess. It shares the
// overlap check with [NewOpenCodeAdapter], which reaches it through
// parsePassthroughConfig, so the constructor's refusal and the offline
// verdict report that fault identically. The warning below has no
// constructor counterpart.
func validateConfig(fields registry.AgentConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	diags = append(diags, validateSkipPermissions(fields.Passthrough)...)
	diags = append(diags, validateToolOverlap(fields.Passthrough)...)

	return diags
}

// validateSkipPermissions warns when opencode.dangerously_skip_permissions
// is explicitly false, which makes the runtime auto-reject every
// permissioned tool call. Absent or true draws no diagnostic: the value does not defeat the
// non-interactive launch posture, so no verdict is required.
func validateSkipPermissions(passthrough map[string]any) []registry.ValidationDiag {
	v, ok := passthrough["dangerously_skip_permissions"].(bool)
	if !ok || v {
		return nil
	}

	return []registry.ValidationDiag{{
		Severity: "warning",
		Check:    "opencode.dangerously_skip_permissions.auto_reject",
		Message: "opencode.dangerously_skip_permissions is set to false, so the " +
			"runtime auto-rejects every permissioned tool call and reports each " +
			"rejection as a warning rather than performing the call",
	}}
}

// validateToolOverlap reports an error when allowed_tools and
// denied_tools name at least one of the same tools, mirroring the check
// [parsePassthroughConfig] used to enforce inline at construction.
func validateToolOverlap(passthrough map[string]any) []registry.ValidationDiag {
	message := overlapMessage(
		typeutil.ExtractStringSlice(passthrough["allowed_tools"]),
		typeutil.ExtractStringSlice(passthrough["denied_tools"]),
	)
	if message == "" {
		return nil
	}

	return []registry.ValidationDiag{{
		Severity: "error",
		Check:    "opencode.allowed_tools.overlap",
		Message:  message,
	}}
}

// overlapMessage returns the byte-identical message both [validateConfig]
// and [NewOpenCodeAdapter] report when allowed and denied name at least
// one common tool, or "" when they do not overlap.
func overlapMessage(allowed, denied []string) string {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}

	var conflicts []string
	for _, key := range denied {
		if _, ok := allowedSet[key]; ok {
			conflicts = append(conflicts, key)
		}
	}
	if len(conflicts) == 0 {
		return ""
	}

	slices.Sort(conflicts)
	return fmt.Sprintf("allowed_tools and denied_tools overlap: %s", strings.Join(conflicts, ", "))
}
