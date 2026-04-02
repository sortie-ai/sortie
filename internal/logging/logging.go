// Package logging provides structured logging primitives for the Sortie
// orchestrator. It configures the process-wide [log/slog] default and
// supplies composable constructors for sub-loggers that carry the
// context fields required by every issue-related and session-related
// log line: issue_id, issue_identifier, and session_id.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Setup initializes the process-wide default logger and returns it. The
// resulting [slog.TextHandler] writes key=value records to w at the given
// minimum level. After Setup returns, [slog.Default] and the top-level slog
// functions (Info, Warn, …) use this logger. Setup is intended to be called
// once during service startup; subsequent calls replace the default silently.
//
// The returned logger is identical to [slog.Default] immediately after the
// call. Callers that need a logger bound to w should use the return value
// rather than calling [slog.Default] afterward to avoid a race when multiple
// goroutines call Setup concurrently.
func Setup(w io.Writer, level slog.Level) *slog.Logger {
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
	})
	l := slog.New(handler)
	slog.SetDefault(l)
	return l
}

// ParseLevel converts a case-insensitive level name to the
// corresponding [slog.Level]. Accepted names: "debug", "info",
// "warn", "error". Returns an error for unrecognized names.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q: accepted values are debug, info, warn, error", s)
	}
}

// WithIssue derives a child logger that attaches the tracker-native
// issue_id and the normalized issue_identifier to every subsequent
// record. The returned logger is safe for concurrent use and may be
// further enriched via [WithSession].
func WithIssue(logger *slog.Logger, issueID string, issueIdentifier string) *slog.Logger {
	return logger.With(
		slog.String("issue_id", issueID),
		slog.String("issue_identifier", issueIdentifier),
	)
}

// WithSession derives a child logger that attaches the coding-agent
// session_id to every subsequent record. It composes with [WithIssue]:
//
//	l := logging.WithSession(logging.WithIssue(base, id, ident), sid)
func WithSession(logger *slog.Logger, sessionID string) *slog.Logger {
	return logger.With(slog.String("session_id", sessionID))
}
