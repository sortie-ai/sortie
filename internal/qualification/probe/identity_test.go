//go:build unix

package probe

import (
	"log/slog"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// Not parallel: agenttest.InstallLogSpy mutates the global slog.Default.
func TestInduceRuntimeIdentity(t *testing.T) {
	t.Run("each handshake record supplies its own session's name and version", func(t *testing.T) {
		spy := agenttest.InstallLogSpy(t)
		slog.Default().Info("agent stderr", slog.String("line", "noise before the handshake"))
		slog.Default().Info("agent implementation", slog.String("session_id", "sess-a"), slog.String("name", ""), slog.String("version", "unused"))
		slog.Default().Info("agent implementation", slog.String("session_id", "sess-a"), slog.String("name", "sample-runtime"), slog.String("version", "1.2.3"))
		slog.Default().Info("agent implementation", slog.String("session_id", "sess-b"), slog.String("name", "sample-runtime"), slog.String("version", "4.5.6"))

		obs, identities := induceRuntimeIdentity(&sharedFixture{logSpy: spy})

		if obs.Grade != qualification.GradeUsable || obs.Outcome != qualification.OutcomePass {
			t.Errorf("induceRuntimeIdentity() observation = %+v, want grade %q and outcome %q", obs, qualification.GradeUsable, qualification.OutcomePass)
		}
		want := map[string]qualification.SessionIdentity{
			"sess-a": {Name: "sample-runtime", Version: "1.2.3"},
			"sess-b": {Name: "sample-runtime", Version: "4.5.6"},
		}
		for sessionID, wantIdentity := range want {
			if identities[sessionID] != wantIdentity {
				t.Errorf("induceRuntimeIdentity() identity for %s = %+v, want %+v", sessionID, identities[sessionID], wantIdentity)
			}
		}
		if len(identities) != len(want) {
			t.Errorf("induceRuntimeIdentity() identities = %v, want exactly %v", identities, want)
		}
	})

	t.Run("no handshake record with a non-empty name grades not_observed with runtime_failed", func(t *testing.T) {
		spy := agenttest.InstallLogSpy(t)
		slog.Default().Info("agent implementation", slog.String("session_id", "sess-a"), slog.String("name", ""))
		slog.Default().Info("some other record", slog.String("session_id", "sess-b"), slog.String("name", "unrelated"))

		obs, identities := induceRuntimeIdentity(&sharedFixture{logSpy: spy})

		if obs.Grade != qualification.GradeNotObserved || obs.Outcome != qualification.OutcomeRuntimeFailed {
			t.Errorf("induceRuntimeIdentity() observation = %+v, want grade %q and outcome %q", obs, qualification.GradeNotObserved, qualification.OutcomeRuntimeFailed)
		}
		if len(identities) != 0 {
			t.Errorf("induceRuntimeIdentity() identities = %v, want none", identities)
		}
	})

	t.Run("a handshake naming no session is not attributed to any", func(t *testing.T) {
		spy := agenttest.InstallLogSpy(t)
		slog.Default().Info("agent implementation", slog.String("name", "sample-runtime"), slog.String("version", "1.2.3"))

		obs, identities := induceRuntimeIdentity(&sharedFixture{logSpy: spy})

		if obs.Grade != qualification.GradeNotObserved || obs.Outcome != qualification.OutcomeRuntimeFailed {
			t.Errorf("induceRuntimeIdentity() observation = %+v, want grade %q and outcome %q", obs, qualification.GradeNotObserved, qualification.OutcomeRuntimeFailed)
		}
		if len(identities) != 0 {
			t.Errorf("induceRuntimeIdentity() identities = %v, want none", identities)
		}
	})

	t.Run("an empty log spy grades not_observed with runtime_failed", func(t *testing.T) {
		spy := agenttest.InstallLogSpy(t)

		obs, identities := induceRuntimeIdentity(&sharedFixture{logSpy: spy})

		if obs.Grade != qualification.GradeNotObserved || obs.Outcome != qualification.OutcomeRuntimeFailed {
			t.Errorf("induceRuntimeIdentity() observation = %+v, want grade %q and outcome %q", obs, qualification.GradeNotObserved, qualification.OutcomeRuntimeFailed)
		}
		if len(identities) != 0 {
			t.Errorf("induceRuntimeIdentity() identities = %v, want none", identities)
		}
	})
}
