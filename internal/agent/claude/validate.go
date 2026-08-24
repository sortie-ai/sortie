package claude

import (
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// nonAskingPermissionModes is the allowlist of claude-code.permission_mode
// values that do not leave the agent waiting on a person. The direction is
// deliberately an allowlist, not a denylist of asking modes, so a mode the
// runtime adds later is refused until someone establishes what it does
// rather than passing validation unexamined. Its only member is the
// semantic equivalent of the --dangerously-skip-permissions flag this
// adapter passes by default.
var nonAskingPermissionModes = map[string]bool{
	"bypassPermissions": true,
}

// validateConfig checks claude-code-specific configuration constraints
// and returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or launch a subprocess.
func validateConfig(fields registry.AgentConfigFields) []registry.ValidationDiag {
	mode := typeutil.StringFrom(fields.Passthrough, "permission_mode")
	if mode == "" || nonAskingPermissionModes[mode] {
		return nil
	}

	return []registry.ValidationDiag{{
		Severity: "error",
		Check:    "claude-code.permission_mode.interactive",
		Message: "claude-code.permission_mode is set to a value that lets the agent stop and ask " +
			"for approval, and an unattended run has no one to answer; only \"bypassPermissions\" is supported",
	}}
}

// sessionResumeBlockedBy reports the claude-code.session_persistence key
// when the given passthrough disables it, using the same helper and
// default parsePassthroughConfig uses so this verdict cannot diverge
// from what the adapter would build.
func sessionResumeBlockedBy(passthrough map[string]any) string {
	if typeutil.BoolFrom(passthrough, "session_persistence", true) {
		return ""
	}

	return "session_persistence"
}
