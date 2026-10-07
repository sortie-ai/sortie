package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

func EffectivePermissionsForTest(ctx context.Context, session domain.Session) (effective map[string]string, raw []byte, err error) {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return nil, nil, fmt.Errorf("session.Internal is %T, want *sessionState", session.Internal)
	}

	args := []string{"api", "--standalone", "GET", "/api/config"}
	cmd, agentErr := state.target.AuxiliaryCommand(ctx, args, nil, buildTurnEnv(state), sortedEnvVars(buildManagedEnv())...)
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

	effective, err = parseEffectivePermissions(raw)
	return effective, raw, err
}

// A later rule overwrites an earlier one for the same action, matching
// the runtime's own last-matching-rule resolution over documents in
// load order.
func parseEffectivePermissions(raw []byte) (map[string]string, error) {
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
