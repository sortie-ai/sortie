package clientprotocol

import (
	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// outcomeSelected and outcomeCancelled are the two literal values the
// pinned schema declares for RequestPermissionOutcome's discriminator.
const (
	outcomeSelected  = requestPermissionOutcomeSelected
	outcomeCancelled = requestPermissionOutcomeCancelled
)

// selectRefusingOption scans options for a refusing kind, reject_once
// first and then reject_always, in the order the agent sent them, and
// ignores the option's identifier and name entirely. It reports
// found=false for an empty list or a list offering only allowing kinds;
// it never selects allow_once or allow_always under any condition.
func selectRefusingOption(options []permissionOption) (optionID string, found bool) {
	for _, opt := range options {
		if opt.Kind == permissionOptionKindRejectOnce {
			return string(opt.OptionID), true
		}
	}
	for _, opt := range options {
		if opt.Kind == permissionOptionKindRejectAlways {
			return string(opt.OptionID), true
		}
	}
	return "", false
}

const permissionsKey = "permissions"

const (
	permissionsDenyAll    = "deny-all"
	permissionsApproveAll = "approve-all"
)

// permissionsSetting reads the operator's declared answer to permission
// requests. An absent or null key refuses, as the kind always has.
func permissionsSetting(settings map[string]any) (agentcore.PermissionDeclaration, *settingFault) {
	raw, present := settings[permissionsKey]
	if !present || raw == nil {
		return agentcore.DeclaredRefusal, nil
	}

	value, ok := raw.(string)
	if !ok {
		return agentcore.DeclaredRefusal, newTypeFault("agent-client-protocol.permissions.wrong_type",
			&typeutil.TypeFault{Key: permissionsKey, Want: "string", Got: typeutil.DescribeYAMLType(raw)})
	}

	switch value {
	case permissionsDenyAll:
		return agentcore.DeclaredRefusal, nil
	case permissionsApproveAll:
		return agentcore.DeclaredApproval, nil
	default:
		return agentcore.DeclaredRefusal, &settingFault{
			check:   "agent-client-protocol.permissions.invalid_value",
			message: permissionsKey + `: accepted values are "` + permissionsDenyAll + `" and "` + permissionsApproveAll + `"`,
		}
	}
}

// selectApprovingOption returns the offer's allow_once option when it holds
// exactly one, chosen by kind alone and never by identifier, name or
// position. It never returns allow_always: the runtime keeps that grant in
// its own store, where it outlives the declared posture. Several allow_once
// options are a choice among answers, not consent to one call, so they are
// not found either.
func selectApprovingOption(options []permissionOption) (optionID string, found bool) {
	for _, opt := range options {
		if opt.Kind != permissionOptionKindAllowOnce {
			continue
		}
		if found {
			return "", false
		}
		optionID, found = string(opt.OptionID), true
	}
	return optionID, found
}
