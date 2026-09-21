//go:build unix

package copilot

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// A remote launch carrying environment variables already sets the
// command's stdin to the SSH preamble; the deletion request must
// compose with it rather than panic on a second stdin.
func TestDeleteVerificationSession_RemoteLaunchWithCarriedEnv(t *testing.T) {
	// Not parallel: t.Setenv carries the SSH environment variable.
	const carriedName = "SORTIE_COPILOT_SESSIONDELETE_TEST_CARRY"
	t.Setenv(carriedName, "carried-value")

	dir := t.TempDir()
	capturePath := dir + "/captured.json"
	serverBin := agenttest.FakeRuntime(t, dir, "copilot", deleteServerScenario, deleteServerParams{CapturePath: capturePath})
	sshPath := agenttest.WriteScript(t, t.TempDir(), "ssh", "last=\"\"\nfor a in \"$@\"; do last=\"$a\"; done\nexec sh -c \"$last\"\n")

	assertDeletedWithoutWarning(t, agentcore.LaunchTarget{
		Command:       sshPath,
		WorkspacePath: dir,
		RemoteCommand: serverBin,
		SSHHost:       "user@stand-in-host",
		SSHEnvNames:   []string{carriedName},
	}, capturePath)
}
