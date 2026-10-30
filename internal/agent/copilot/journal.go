package copilot

import (
	"context"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/copilot/sessionstate"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

// readJournal reads the session's usage records from the runtime's host. Every
// failure, a missing file included, returns a read that did not complete: a
// resumed session's journal exists, so a miss means the read cannot see the
// runtime's home, and an empty result would count the whole history as the
// run's spend.
func (s *sessionState) readJournal(ctx context.Context) usage.RecordsRead {
	scan := func(ctx context.Context, path agentcore.RuntimePath, limit, lineLimit int64, fn func(line []byte) error) error {
		return s.target.ScanRuntimeFile(ctx, nil, path, limit, lineLimit, fn)
	}
	readings, err := sessionstate.ScanRecords(ctx, s.copilotSessionID, scan)
	if err != nil {
		s.logger().Debug("copilot journal read failed", slog.Any("error", err))
		return usage.RecordsRead{}
	}
	return usage.RecordsRead{Readings: readings, OK: true}
}
