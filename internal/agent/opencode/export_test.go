package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

func RuntimeMajorForTest(session domain.Session) (int, error) {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return 0, fmt.Errorf("session.Internal is %T, want *sessionState", session.Internal)
	}
	if state.major == majorUnknown {
		return 0, errors.New("session major was never detected")
	}
	return int(state.major), nil
}

func EffectivePermissionsForTest(ctx context.Context, session domain.Session) (effective map[string]string, raw []byte, err error) {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return nil, nil, fmt.Errorf("session.Internal is %T, want *sessionState", session.Internal)
	}
	if state.major == majorUnknown {
		return nil, nil, errors.New("session major was never detected")
	}

	env, err := buildTurnEnv(state)
	if err != nil {
		return nil, nil, fmt.Errorf("build opencode environment: %w", err)
	}
	managedEnv, err := buildManagedEnv(state.passthrough, state.major)
	if err != nil {
		return nil, nil, fmt.Errorf("build opencode managed environment: %w", err)
	}

	args := []string{"debug", "config"}
	if state.major == major2 {
		args = []string{"api", "--standalone", "GET", "/api/config"}
	}

	cmd, agentErr := state.target.AuxiliaryCommand(ctx, args, nil, env, sortedEnvVars(managedEnv)...)
	if agentErr != nil {
		return nil, nil, fmt.Errorf("build effective-permissions command: %w", agentErr)
	}

	var stdout bytes.Buffer
	result, startErr := procutil.RunCapture(cmd, procutil.StopGrace(state.agentConfig.StopGraceMS), procutil.CaptureParams{
		Stdout: &stdout,
	})
	raw = stdout.Bytes()
	if startErr != nil {
		return nil, raw, fmt.Errorf("start effective-permissions command: %w", startErr)
	}
	if result.WaitErr != nil {
		return nil, raw, fmt.Errorf("effective-permissions command failed: %w", result.WaitErr)
	}

	if state.major == major2 {
		effective, err = parseMajor2EffectivePermissions(raw)
		return effective, raw, err
	}
	effective, err = parseMajor1EffectivePermissions(raw)
	return effective, raw, err
}

func parseMajor1EffectivePermissions(raw []byte) (map[string]string, error) {
	var doc struct {
		Permission map[string]any `json:"permission"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode debug config output: %w", err)
	}

	effective := make(map[string]string, len(doc.Permission))
	for key, value := range doc.Permission {
		if str, ok := value.(string); ok {
			effective[key] = str
		}
	}
	return effective, nil
}

// A later rule overwrites an earlier one for the same action, matching
// the runtime's own last-matching-rule resolution over documents in
// load order.
func parseMajor2EffectivePermissions(raw []byte) (map[string]string, error) {
	var elements []struct {
		Type string `json:"type"`
		Info struct {
			Permissions []struct {
				Resource string `json:"resource"`
				Action   string `json:"action"`
				Effect   string `json:"effect"`
			} `json:"permissions"`
		} `json:"info"`
	}
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, fmt.Errorf("decode /api/config output: %w", err)
	}

	effective := make(map[string]string)
	for _, element := range elements {
		if element.Type != "document" {
			continue
		}
		for _, rule := range element.Info.Permissions {
			if rule.Resource != "*" {
				continue
			}
			effective[rule.Action] = rule.Effect
		}
	}
	return effective, nil
}
