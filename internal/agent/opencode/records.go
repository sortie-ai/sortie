package opencode

import (
	"context"
	"log/slog"
	"os"
	"slices"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/opencode/sessionexport"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

// runtimeCommand describes a runtime subcommand run on the session's host
// through the shared host-command operation. It runs the configured command
// whole in the session's workspace with the managed variable and never the
// inline configuration; a zero limit takes the operation's default.
func (s *sessionState) runtimeCommand(args []string, limit int64) agentcore.HostCommand {
	return agentcore.HostCommand{
		LaunchWords: len(s.target.CommandWords()),
		Args:        args,
		InSession:   true,
		Limit:       limit,
		Settings:    sortedEnvVars(buildManagedEnv()),
	}
}

// readExport returns the readings of one session's export. A failure returns
// its error alone: the export may hold tool output, which never reaches an
// error or a log.
func (s *sessionState) readExport(ctx context.Context, sessionID string) ([]usage.SpendReading, error) {
	out, err := s.target.RunHostCommand(ctx, buildRunEnv(os.Environ()), s.runtimeCommand(sessionexport.Args(sessionID), usage.MaxRecordsBytes))
	if err != nil {
		return nil, err
	}
	return sessionexport.Parse(out, sessionID)
}

// readRecords reads the export of the session and of every child session the
// turn named. A failed read reports OK false, so a resumed session's history
// is never counted as the run's spend, and a failed session read still reads
// the children.
func (s *sessionState) readRecords(ctx context.Context) usage.RecordsRead {
	sessionID := s.currentSessionID()
	if sessionID == "" {
		return usage.RecordsRead{}
	}

	var read usage.RecordsRead
	readings, err := s.readExport(ctx, sessionID)
	if err != nil {
		s.logger().Debug("opencode session export failed", slog.Any("error", err))
	} else {
		read.Readings, read.OK = readings, true
	}

	named := s.namedChildSessions()
	read.ChildrenOK = len(named) > 0
	for _, child := range named {
		childReadings, err := s.readExport(ctx, child)
		if err != nil {
			read.ChildrenOK = false
			s.logger().Debug("opencode session export failed",
				slog.Any("error", err), slog.String("child_session_id", child))
			continue
		}
		read.ChildReadings = append(read.ChildReadings, childReadings...)
	}
	return read
}

// namedChildSessions returns the distinct session ids the turn's subagent
// calls named, in first-sighting order.
func (s *sessionState) namedChildSessions() []string {
	var named []string
	for _, child := range s.children {
		if child.session != "" && !slices.Contains(named, child.session) {
			named = append(named, child.session)
		}
	}
	return named
}

// childrenSettled reports whether every subagent call the turn showed
// completed and named its session.
func (s *sessionState) childrenSettled() bool {
	return !slices.ContainsFunc(s.children, func(child childSession) bool {
		return !child.completed || child.session == ""
	})
}
